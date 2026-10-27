package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// A harness can stop at a screen nobody expects: a new question after an update, a settings
// menu, a login prompt. Wakes typed into it are lost and the agent goes silent. So after each
// wake the daemon expects the mentions to get handled, and after each restart it expects the
// session to register. If that doesn't happen in time it runs the [zm] unstick hook
// (`UNSTICK SESSION ROLE REASON`), which looks at the screen and tries to get the harness back
// to its prompt. Exit 0 = it sent keys (the daemon re-delivers the wake or redoes the restart
// and watches once more), 3 = the agent is busy working (watch on), anything else = can't fix.
// When the hook can't fix it, isn't set up, or its fix didn't help, the owner gets a DM.

const (
	stuckAfter = 5 * time.Minute
	busyLimit  = time.Hour // still unhandled after this long while "busy" = stuck too
	hookLimit  = 3 * time.Minute
)

type watch struct {
	role  string
	start bool      // waiting for the session to register after a (re)start
	msgs  []pending // else: the mentions it was woken for
	since time.Time // first expectation not met yet
	due   time.Time
	tries int  // unstick fixes that already ran for this incident
	busy  bool // the hook is running
}

type fixResult struct {
	session string
	out     string
	code    int // hook exit code, -1 = didn't run
}

// expect records what session s should do within stuckAfter. msgs nil = register after a start.
func (d *daemon) expect(s, role string, msgs []pending, tries int) {
	now := time.Now()
	if w := d.watch[s]; w != nil && !w.start && msgs != nil {
		w.msgs = append(w.msgs, msgs...) // another wake while the first is still pending
		return
	}
	d.watch[s] = &watch{role: role, start: msgs == nil, msgs: msgs, since: now, due: now.Add(stuckAfter), tries: tries}
}

func (d *daemon) checkStuck() {
	for {
		select {
		case r := <-d.fixed:
			d.afterFix(r)
			continue
		default:
		}
		break
	}
	now := time.Now()
	for s, w := range d.watch {
		if w.busy || now.Before(w.due) {
			continue
		}
		done, err := d.progressed(s, w)
		if err != nil {
			log.Printf("stuck check %s: %v", s, err)
			w.due = now.Add(time.Minute)
			continue
		}
		if done {
			delete(d.watch, s)
			continue
		}
		reason := "woken for a mention, nothing handled"
		if w.start {
			reason = "started, never registered"
		}
		if d.c.Unstick == "" || w.tries > 0 || now.Sub(w.since) > busyLimit {
			d.alert(s, w, reason, "")
			delete(d.watch, s)
			continue
		}
		w.busy = true
		log.Printf("%s (%s) looks stuck (%s): running unstick", s, w.role, reason)
		go func(s, role, reason string) {
			out, code := runHook(d.c.Unstick, s, role, reason)
			d.fixed <- fixResult{s, out, code}
		}(s, w.role, reason)
	}
}

func (d *daemon) progressed(s string, w *watch) (bool, error) {
	if w.start {
		return d.subs[s] != nil, nil
	}
	for _, p := range w.msgs {
		ok, err := handled(d.c, p)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// handled: the mentioned user reacted to the message or posted later in its topic.
func handled(c *Client, p pending) (bool, error) {
	msgs, _, err := c.Messages([]narrowTerm{{"id", p.msgID}}, strconv.Itoa(p.msgID), 0, 0, false)
	if err != nil || len(msgs) == 0 {
		return true, err // gone (deleted, moved out of reach): nothing left to wait for
	}
	m := msgs[0]
	if reactedBy(m, p.userID) {
		return true, nil
	}
	later, _, err := c.Messages([]narrowTerm{{"channel", m.ChannelName()}, {"topic", m.Topic}}, strconv.Itoa(m.ID), 0, 100, false)
	if err != nil {
		return false, err
	}
	for _, l := range later {
		if l.ID > m.ID && l.SenderID == p.userID {
			return true, nil
		}
	}
	return false, nil
}

func (d *daemon) afterFix(r fixResult) {
	w := d.watch[r.session]
	if w == nil {
		return
	}
	w.busy = false
	switch r.code {
	case 3: // busy: the wake is queued in its input, check again later
		w.due = time.Now().Add(stuckAfter)
	case 0:
		log.Printf("unstick %s: %s", r.session, firstLine(r.out))
		if w.start {
			if err := d.startSession(r.session, w.role, w.tries+1); err != nil {
				d.alert(r.session, w, "restart after unstick failed", err.Error())
				delete(d.watch, r.session)
			}
			return
		}
		w.tries++
		w.due = time.Now().Add(stuckAfter)
		if err := nudge(r.session, wakeText(w.role, len(w.msgs))); err != nil {
			log.Printf("wake %s: %v", r.session, err)
		}
	default:
		d.alert(r.session, w, "unstick couldn't fix it", r.out)
		delete(d.watch, r.session)
	}
}

func (d *daemon) alert(s string, w *watch, reason, out string) {
	log.Printf("%s (%s) stuck: %s", s, w.role, reason)
	if d.c.Owner == "" {
		return
	}
	msg := fmt.Sprintf("**%s** looks stuck on host %s (session `%s`): %s, for %d min.", w.role, d.host, s, reason, int(time.Since(w.since).Minutes()))
	if w.tries > 0 {
		msg += " An automatic fix already ran and didn't help."
	}
	if out = strings.TrimSpace(out); out != "" {
		msg += "\n```text\n" + truncate(out, 3000) + "\n```"
	}
	msg += fmt.Sprintf("\nLook at it with `zmx attach %s`, or DM me `zm-restart %s`.", s, w.role)
	if err := d.c.SendDM(d.c.Owner, msg); err != nil {
		log.Printf("alert %s: %v", s, err)
	}
}

func runHook(hook, s, role, reason string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), hookLimit)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, hook, s, role, reason)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &exit) && ctx.Err() == nil:
		return out.String(), exit.ExitCode()
	default:
		return fmt.Sprintf("%s\nunstick hook: %v", out.String(), err), -1
	}
}

func firstLine(s string) string { l, _, _ := strings.Cut(strings.TrimSpace(s), "\n"); return l }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:] // the screen's end matters most
}
