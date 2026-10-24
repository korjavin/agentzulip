package main

import (
	"errors"
	"fmt"
	"html"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The daemon keeps no files. Everything it knows is rebuilt from Zulip on every (re)start:
//   - which session to wake for whom: zm-register/zm-unregister DMs agents sent to this bot;
//   - which channels to watch: its own subscriptions;
//   - what is still unhandled: recent @-mentions the mentioned bot hasn't reacted to or answered after.

const backfillWindow = time.Hour

type daemon struct {
	c      *Client
	me     int
	host   string
	subs   map[string]map[int]string // session -> userID -> role
	nudged map[int]bool              // message ids already nudged in this process
}

func runDaemon(c *Client, host string) error {
	me, name, err := c.Me()
	if err != nil {
		return err
	}
	log.Printf("zm daemon: %s on host %s", name, host)
	d := &daemon{c: c, me: me, host: host, nudged: map[int]bool{}}
	for {
		err := d.cycle()
		log.Printf("event queue lost (%v), re-registering", err)
		time.Sleep(5 * time.Second)
	}
}

// cycle registers an event queue first, then catches up from history, then follows the queue.
// Registering first means nothing posted during the catch-up falls into a gap; d.nudged dedupes.
func (d *daemon) cycle() error {
	q, last, err := d.c.RegisterQueue()
	if err != nil {
		return err
	}
	if err := d.loadRegistry(); err != nil {
		return err
	}
	if err := d.backfill(); err != nil {
		return err
	}
	for {
		evs, err := d.c.Events(q, last)
		var api *APIError
		if errors.As(err, &api) && api.Code == "BAD_EVENT_QUEUE_ID" {
			return err
		}
		if err != nil {
			log.Printf("events: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}
		var msgs []Message
		for _, ev := range evs {
			last = max(last, ev.ID)
			if ev.Type == "message" {
				msgs = append(msgs, ev.Message)
			}
		}
		d.handle(msgs)
	}
}

func (d *daemon) handle(msgs []Message) {
	wake := map[string]map[string]int{} // session -> role -> new mentions
	for _, m := range msgs {
		if m.ChannelName() == "" { // a DM: requests to this bot
			if m.SenderID != d.me {
				d.applyRegistry(m, true)
			}
			continue
		}
		for _, uid := range mentionedIDs(m.Content) {
			if uid != m.SenderID && !d.nudged[m.ID] {
				d.addWake(wake, uid)
			}
		}
		d.nudged[m.ID] = true
	}
	d.wake(wake)
}

func (d *daemon) addWake(wake map[string]map[string]int, uid int) {
	for session, roles := range d.subs {
		if role, ok := roles[uid]; ok {
			if wake[session] == nil {
				wake[session] = map[string]int{}
			}
			wake[session][role]++
		}
	}
}

func (d *daemon) loadRegistry() error {
	var all []Message
	anchor := "newest"
	for {
		msgs, oldest, err := d.c.Messages([]narrowTerm{{"is", "dm"}}, anchor, 1000, 0, false)
		if err != nil {
			return err
		}
		all = append(msgs, all...)
		if oldest || len(msgs) == 0 {
			break
		}
		anchor = strconv.Itoa(msgs[0].ID)
		all = all[1:] // the next page repeats its anchor
	}
	d.subs = map[string]map[int]string{}
	for _, m := range all {
		if m.SenderID != d.me {
			d.applyRegistry(m, false)
		}
	}
	log.Printf("registry: %d session(s) on %s", len(d.subs), d.host)
	return nil
}

// applyRegistry handles one request DM. live=false replays history: only joins this daemon
// hasn't confirmed with ✅ yet are redone (they were requested while it was down).
func (d *daemon) applyRegistry(m Message, live bool) {
	cmd, kv, rest := parseCommand(m.Content)
	switch cmd {
	case "zm-register", "zm-unregister":
		if kv["host"] != d.host || !safe.MatchString(kv["session"]) || !safe.MatchString(kv["role"]) {
			return
		}
		s := kv["session"]
		if cmd == "zm-unregister" {
			delete(d.subs[s], m.SenderID)
			if len(d.subs[s]) == 0 {
				delete(d.subs, s)
			}
			return
		}
		// Several roles may share a session (e.g. a helper started inside it); nobody replaces anybody.
		if d.subs[s] == nil {
			d.subs[s] = map[int]string{}
		}
		d.subs[s][m.SenderID] = kv["role"]
		if live {
			log.Printf("registered %s (user %d) -> session %s", kv["role"], m.SenderID, s)
		}
	case "zm-join":
		if rest == "" || (!live && reactedBy(m, d.me)) {
			return
		}
		// ponytail: every host's daemon performs the join; subscribing is idempotent, so it's harmless.
		who := []string{m.SenderEmail, d.c.Email}
		if d.c.Owner != "" {
			who = append(who, d.c.Owner)
		}
		if err := d.c.Subscribe(rest, who...); err != nil {
			log.Printf("join %q for %s: %v", rest, m.SenderEmail, err)
			_ = d.c.SendDM(m.SenderEmail, fmt.Sprintf("join %s failed: %v", rest, err))
			return
		}
		log.Printf("joined %s to #%s", m.SenderEmail, rest)
		_ = d.c.React(m.ID, "check")
	case "zm-restart":
		if live {
			d.restart(m, kv, rest)
		}
	}
}

// restart runs the configured restart command for every session the role is registered in on
// this host. Allowed for the owner (any role) and for a role restarting itself. Never replayed.
func (d *daemon) restart(m Message, kv map[string]string, rest string) {
	role := kv["role"]
	if role == "" {
		role = strings.ToLower(rest) // the owner types "zm-restart socrates" by hand
	}
	var sessions []string
	self := false
	for s, roles := range d.subs {
		for _, r := range roles {
			if r == role {
				sessions = append(sessions, s)
				self = self || roles[m.SenderID] == role
				break
			}
		}
	}
	if len(sessions) == 0 || d.c.Restart == "" {
		return // not on this host (another host's daemon handles it), or restarts not set up here
	}
	if !self && (d.c.Owner == "" || m.SenderEmail != d.c.Owner) {
		_ = d.c.SendDM(m.SenderEmail, "restart "+role+": only the owner or the role itself may restart it")
		return
	}
	sort.Strings(sessions)
	for _, s := range sessions {
		cmd := exec.Command(d.c.Restart, s)
		if err := cmd.Start(); err != nil {
			log.Printf("restart %s: %v", s, err)
			_ = d.c.SendDM(m.SenderEmail, fmt.Sprintf("restart %s failed: %v", s, err))
			return
		}
		log.Printf("restarting %s (%s) for %s", s, role, m.SenderEmail)
		go func() {
			if err := cmd.Wait(); err != nil {
				log.Printf("restart %s: %v", s, err)
			}
		}()
	}
	_ = d.c.React(m.ID, "check")
}

// backfill wakes sessions for recent mentions that are still unhandled.
func (d *daemon) backfill() error {
	chans, err := d.c.Subscriptions()
	if err != nil {
		return err
	}
	wake := map[string]map[string]int{}
	since := time.Now().Add(-backfillWindow).Unix()
	for _, ch := range chans {
		// ponytail: last 200 messages per channel; page further if channels get that busy in a day.
		msgs, _, err := d.c.Messages([]narrowTerm{{"channel", ch}}, "newest", 200, 0, true)
		if err != nil {
			return err
		}
		for _, p := range unhandled(msgs, since) {
			if !d.nudged[p.msgID] {
				d.addWake(wake, p.userID)
			}
		}
		for _, m := range msgs {
			d.nudged[m.ID] = true
		}
	}
	d.wake(wake)
	return nil
}

type pending struct{ msgID, userID int }

// unhandled returns (message, mentioned user) pairs where the user neither reacted to the
// message nor posted later in the same topic. msgs must be in ascending id order.
func unhandled(msgs []Message, since int64) []pending {
	var out []pending
	for i, m := range msgs {
		if m.Timestamp < since {
			continue
		}
	mention:
		for _, uid := range mentionedIDs(m.Content) {
			if uid == m.SenderID {
				continue
			}
			if reactedBy(m, uid) {
				continue
			}
			for _, later := range msgs[i+1:] {
				if later.SenderID == uid && later.Topic == m.Topic {
					continue mention
				}
			}
			out = append(out, pending{m.ID, uid})
		}
	}
	return out
}

func reactedBy(m Message, uid int) bool {
	for _, r := range m.Reactions {
		if r.UserID == uid {
			return true
		}
	}
	return false
}

func (d *daemon) wake(wake map[string]map[string]int) {
	sessions := make([]string, 0, len(wake))
	for s := range wake {
		sessions = append(sessions, s)
	}
	sort.Strings(sessions)
	for _, s := range sessions {
		for role, n := range wake[s] {
			// Fixed text built only from validated names and a count: message content never
			// reaches the terminal.
			text := fmt.Sprintf("[zm] %d new mention(s) for %s. Run: zm -as %s inbox", n, role, role)
			if err := nudge(s, text); err != nil {
				log.Printf("wake %s: %v", s, err)
				continue
			}
			log.Printf("woke %s (%s): %d", s, role, n)
		}
	}
}

// nudge types text plus Enter into a terminal session. $ZM_NUDGE replaces the built-in zmx
// method: it is run as `$ZM_NUDGE SESSION TEXT` (e.g. a script calling tmux send-keys).
func nudge(session, text string) error {
	if cmd := os.Getenv("ZM_NUDGE"); cmd != "" {
		return exec.Command(cmd, session, text).Run()
	}
	if err := exec.Command("zmx", "send", session, text).Run(); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond) // separate Enter, or TUIs treat it as part of a paste
	return exec.Command("zmx", "send", session, "\r").Run()
}

// Rendered mentions look like <span class="user-mention" data-user-id="12">; silent mentions
// carry class "user-mention silent" and deliberately don't match. A quote-reply ("Quote message"
// in Zulip) starts with a silent mention of the author followed by a "said" link to the message:
// that counts as mentioning the author.
var (
	mentionRe = regexp.MustCompile(`<span class="user-mention" data-user-id="(\d+)"`)
	replyRe   = regexp.MustCompile(`<span class="user-mention silent" data-user-id="(\d+)">[^<]*</span> <a href="[^"]*/near/\d+">said</a>`)
)

// mentionedIDs returns who a rendered message mentions or quote-replies to, ignoring anything
// inside quotes (a quoted mention isn't a new one).
func mentionedIDs(renderedHTML string) []int {
	text := stripQuotes(renderedHTML)
	var ids []int
	seen := map[int]bool{}
	for _, re := range []*regexp.Regexp{mentionRe, replyRe} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			id, _ := strconv.Atoi(m[1])
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// stripQuotes drops <blockquote> elements, nested ones included.
func stripQuotes(s string) string {
	var b strings.Builder
	depth := 0
	for s != "" {
		open, end := strings.Index(s, "<blockquote>"), strings.Index(s, "</blockquote>")
		switch {
		case open >= 0 && (end < 0 || open < end):
			if depth == 0 {
				b.WriteString(s[:open])
			}
			depth++
			s = s[open+len("<blockquote>"):]
		case end >= 0 && depth > 0:
			depth--
			s = s[end+len("</blockquote>"):]
		default:
			if depth == 0 {
				b.WriteString(s)
			}
			s = ""
		}
	}
	return b.String()
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// parseCommand reads the first line of a request DM, raw or rendered:
// "zm-register role=a host=b session=c" -> ("zm-register", {role:a,...}, "role=a host=b session=c").
func parseCommand(content string) (cmd string, kv map[string]string, rest string) {
	text := strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(content, " ")))
	line, _, _ := strings.Cut(text, "\n")
	cmd, rest, _ = strings.Cut(strings.TrimSpace(line), " ")
	rest = strings.TrimSpace(rest)
	kv = map[string]string{}
	for _, f := range strings.Fields(rest) {
		if k, v, ok := strings.Cut(f, "="); ok {
			kv[k] = v
		}
	}
	return cmd, kv, rest
}
