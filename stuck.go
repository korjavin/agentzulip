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
	"syscall"
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
	noHookWait = 25 * time.Minute // without an unstick hook only the owner can help: don't DM about a merely busy agent
	busyLimit  = time.Hour        // still unhandled after this long while "busy" = stuck too
	hookLimit  = 3 * time.Minute
)

type watch struct {
	session string
	role    string
	start   bool      // waiting for the session to register after a (re)start
	msgs    []pending // else: the mentions it was woken for
	since   time.Time // first expectation not met yet
	due     time.Time
	tries   int  // unstick fixes that already ran for this incident
	busy    bool // the hook is running
	alerted bool // start watch the owner was told about: kept so `zm-restart <role>` still finds the session
}

type fixResult struct {
	w    *watch // the incident the hook ran for; a newer watch for the key makes the result stale
	out  string
	code int // hook exit code, -1 = didn't run
}

// Watches are per session and role: roles sharing a session are woken and checked separately.
func watchKey(s, role string) string { return s + "\x00" + role }

// expect records what role in session s should do within stuckAfter. msgs nil = register after a start.
func (d *daemon) expect(s, role string, msgs []pending, tries int) {
	now := time.Now()
	k := watchKey(s, role)
	if w := d.watch[k]; w != nil && !w.start && msgs != nil {
		w.msgs = append(w.msgs, msgs...) // another wake while the first is still pending
		w.due = now.Add(stuckAfter)      // the new mention gets its own grace period
		return
	}
	d.watch[k] = &watch{session: s, role: role, start: msgs == nil, msgs: msgs, since: now, due: now.Add(stuckAfter), tries: tries}
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
	for k, w := range d.watch {
		s := w.session
		if w.alerted {
			if done, _ := d.progressed(w); done {
				delete(d.watch, k)
			}
			continue
		}
		if w.busy || now.Before(w.due) {
			continue
		}
		done, err := d.progressed(w)
		if err != nil {
			log.Printf("stuck check %s: %v", s, err)
			w.due = now.Add(time.Minute)
			continue
		}
		if done {
			delete(d.watch, k)
			continue
		}
		reason := "woken for a mention, nothing handled"
		if w.start {
			reason = "started, never registered"
		}
		if d.c.Unstick == "" && now.Sub(w.since) < noHookWait {
			w.due = w.since.Add(noHookWait)
			continue
		}
		if d.c.Unstick == "" || w.tries > 0 || now.Sub(w.since) > busyLimit {
			d.giveUp(k, w, reason, "")
			continue
		}
		w.busy = true
		log.Printf("%s (%s) looks stuck (%s): running unstick", s, w.role, reason)
		go func(w *watch, reason string) {
			out, code := runHook(d.c.Unstick, w.session, w.role, reason)
			d.fixed <- fixResult{w, out, code}
		}(w, reason)
	}
}

func (d *daemon) progressed(w *watch) (bool, error) {
	if w.start {
		for _, r := range d.subs[w.session] {
			if r == w.role {
				return true, nil
			}
		}
		return false, nil
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
	w, s := r.w, r.w.session
	k := watchKey(s, w.role)
	if d.watch[k] != w {
		log.Printf("unstick %s: result for an incident already replaced, ignored", s)
		return
	}
	w.busy = false
	// The agent may have recovered while the hook ran (3 min at most): then act on nothing.
	if done, err := d.progressed(w); err == nil && done {
		delete(d.watch, k)
		return
	}
	switch r.code {
	case 3: // busy: the wake is queued in its input, check again later
		log.Printf("unstick %s: %s", s, firstLine(r.out))
		w.due = time.Now().Add(stuckAfter)
	case 0:
		log.Printf("unstick %s: %s", s, firstLine(r.out))
		if w.start {
			if err := d.startSession(s, w.role, w.tries+1); err != nil {
				d.giveUp(k, w, "restart after unstick failed", err.Error())
			}
			return
		}
		w.tries++
		w.due = time.Now().Add(stuckAfter)
		if err := nudge(s, wakeText(w.role, len(w.msgs))); err != nil {
			log.Printf("wake %s: %v", s, err)
		}
	default:
		d.giveUp(k, w, "unstick couldn't fix it", r.out)
	}
}

// giveUp tells the owner and stops checking. A start watch stays (marked) until the session
// registers: it is how `zm-restart <role>` finds a session that never registered.
func (d *daemon) giveUp(k string, w *watch, reason, out string) {
	d.alert(w, reason, out)
	if w.start {
		w.alerted = true
		return
	}
	delete(d.watch, k)
}

func (d *daemon) alert(w *watch, reason, out string) {
	s := w.session
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
	msg += fmt.Sprintf("\nLook at it with `zmx attach %s`, or DM me `zm-restart %s` (kills the session and starts it again).", s, w.role)
	if err := d.c.SendDM(d.c.Owner, msg); err != nil {
		log.Printf("alert %s: %v", s, err)
	}
}

func runHook(hook string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), hookLimit)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, hook, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	// Kill the hook's whole process group at the deadline, and stop waiting for pipes its
	// orphans hold: otherwise a hung child keeps Run from returning and the watch stays busy.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil: // exited 0; an orphan holds the pipe
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
