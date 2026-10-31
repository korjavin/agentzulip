// zm: let coding agents talk to each other through Zulip.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const usage = `zm - agent messaging over Zulip

usage: zm [-as ROLE | -rc FILE] COMMAND [flags]

CHANNEL below defaults to the project's work channel; -owner picks the owner's channel instead.

  send [-c CHANNEL|-owner] -t TOPIC [-to NAME,NAME] [TEXT]
                                                    post (TEXT or stdin), @-mentioning -to
  inbox [-peek]                                     unread @-mentions of me and quote-replies to my
                                                    messages; marks them read
  read [-c CHANNEL|-owner] -t TOPIC [-n 30]         recent history of a topic
  wait [-c CHANNEL|-owner] -t TOPIC [-timeout 1h]   block until someone else posts there; print it
                                                    (exit 3 on timeout)
  resolve [-c CHANNEL|-owner] -t TOPIC              mark the topic done (Zulip's "✔ " prefix)
  unresolve [-c CHANNEL|-owner] -t TOPIC            reopen it (send does this by itself)
  rollcall TEXT                                     post TEXT in the work channel's "roll call"
  orders [-all] [add TEXT]                          my latest "standing orders" list plus the owner's
                                                    posts after it (-all: every role's); add posts TEXT
  where                                             print the project's owner and work channels
  ack ID                                            react ✅ ("taken") to a message
  register [-session S] [-host H]                   ask the daemon to wake session S on mentions of me
  unregister [-session S] [-host H]                 stop that
  join [CHANNEL]                                    ask the daemon to add me (and the owner) to CHANNEL
                                                    (default: both project channels); only the owner's
                                                    requests create a missing channel
  restart [ROLE]                                    ask the daemon to restart ROLE's session (default:
                                                    me) for a fresh context; others' only for the owner
  get [-o FILE] /user_uploads/…                     download a file attached to a message
  whoami                                            print my Zulip identity
  daemon [-host H]                                  watch Zulip and wake registered sessions

ROLE picks ~/.config/zm/ROLE.zuliprc (default: $ZM_AS). The file is a standard zuliprc
([api] email, key, site; or key_cmd = a command printing the key) plus a [zm] section:
porter = EMAIL of the daemon's bot (register/unregister/join are DMs to it), and for the
owner = EMAIL of the human (agents: their unmentioned replies count as meant for me, see inbox;
daemon: added to every channel it creates, and its unmentioned replies wake the agent who posted
last in that topic) and, for the daemon, restart =
a command run as RESTART SESSION (e.g. kill the session and start it again with its first prompt).
For the daemon, unstick = a command run as UNSTICK SESSION ROLE REASON when an agent woken for
a mention hasn't handled it, or a restarted session hasn't registered, within 5 minutes (exit 0 =
sent keys: the daemon re-delivers the wake, or for a session that never registered runs restart
again, and checks once more; 3 = busy, keep watching; else = can't fix). Unfixed or a fix that
didn't help: the owner gets a DM naming the stuck agent; without a hook, after 25 minutes.
For the daemon, harness = a command run as HARNESS ARG when the owner DMs it "zm-harness ARG"
(e.g. switch the team between Claude Code and Codex); its output comes back as a DM.
channel = the project's owner channel (overridden by $ZM_CHANNEL); its work channel is the same
name plus "-work".
Optional, daemon and agents alike: boss = EMAIL of a second human (e.g. a non-technical client):
their unmentioned replies count like the owner's, nothing else (no channel creation, restarts or
alerts); frontdoor = ROLE that gets a human's unmentioned message in a topic no agent posted in.
`

var safe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`) // no leading "-": these reach commands as args

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "zm:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	g := flag.NewFlagSet("zm", flag.ContinueOnError)
	g.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	as := g.String("as", os.Getenv("ZM_AS"), "role")
	rc := g.String("rc", "", "zuliprc path")
	if err := g.Parse(args); err != nil {
		return err
	}
	if g.NArg() == 0 {
		g.Usage()
		return errors.New("no command")
	}
	cmd, rest := g.Arg(0), g.Args()[1:]
	if *as == "" && cmd == "daemon" {
		*as = "daemon"
	}
	if *rc == "" {
		if *as == "" {
			return errors.New("set -as ROLE, -rc FILE or $ZM_AS")
		}
		home, _ := os.UserHomeDir()
		*rc = filepath.Join(home, ".config", "zm", *as+".zuliprc")
	}
	c, err := loadRC(*rc)
	if err != nil {
		return err
	}
	c.Role = *as
	if ch := os.Getenv("ZM_CHANNEL"); ch != "" {
		c.Channel = ch
	}
	// to resolves a command's channel: -c if given, else the project's owner (-owner) or work channel.
	var owner *bool
	to := func(ch string) (string, error) {
		switch {
		case ch != "":
			return ch, nil
		case c.Channel == "":
			return "", errors.New("no -c and no project channel: set $ZM_CHANNEL or [zm] channel in " + *rc)
		case owner != nil && *owner:
			return c.Channel, nil
		}
		return c.Channel + "-work", nil
	}
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	host, _ := os.Hostname()
	switch cmd {
	case "send":
		ch := f.String("c", "", "channel")
		owner = f.Bool("owner", false, "the owner's channel")
		topic := f.String("t", "", "topic")
		names := f.String("to", "", "comma-separated names to @-mention")
		if err := f.Parse(rest); err != nil {
			return err
		}
		text := strings.Join(f.Args(), " ")
		if text == "" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			text = string(b)
		}
		if *topic == "" || strings.TrimSpace(text) == "" {
			return errors.New("send needs -t and text")
		}
		if *ch, err = to(*ch); err != nil {
			return err
		}
		var mentions []string
		for _, n := range strings.Split(*names, ",") {
			if n = strings.TrimSpace(n); n != "" {
				mentions = append(mentions, "@**"+n+"**")
			}
		}
		if len(mentions) > 0 {
			text = strings.Join(mentions, " ") + " " + text
		}
		if err := reopen(c, *ch, *topic); err != nil {
			return err
		}
		id, err := sendOnce(c, *ch, *topic, text)
		if err != nil {
			return err
		}
		fmt.Println(id)
	case "inbox":
		peek := f.Bool("peek", false, "don't mark read")
		if err := f.Parse(rest); err != nil {
			return err
		}
		msgs, _, err := c.Messages([]narrowTerm{{"is", "mentioned"}, {"is", "unread"}}, "oldest", 0, 100, false)
		if err != nil {
			return err
		}
		replies, err := unreadReplies(c)
		if err != nil {
			return err
		}
		seen := map[int]bool{}
		for _, m := range msgs {
			seen[m.ID] = true
		}
		for _, m := range replies {
			if !seen[m.ID] {
				msgs = append(msgs, m)
			}
		}
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
		ids := make([]int, len(msgs))
		for i, m := range msgs {
			printMsg(m)
			ids[i] = m.ID
		}
		open, err := unanswered(c, seen)
		if err != nil {
			return err
		}
		if len(open) > 0 {
			fmt.Println("=== read earlier but never answered (reply in the topic or ack):")
			for _, m := range open {
				printMsg(m)
			}
		}
		if len(msgs) == 0 && len(open) == 0 {
			fmt.Println("(no unread mentions)")
		}
		if !*peek {
			return c.MarkRead(ids)
		}
	case "read", "orders":
		ch := f.String("c", "", "channel")
		owner = f.Bool("owner", false, "the owner's channel")
		topic := f.String("t", "", "topic")
		n := f.Int("n", 30, "messages")
		all := f.Bool("all", false, "orders: every role's lists, not just mine")
		if cmd == "orders" {
			if len(rest) > 0 && rest[0] == "add" {
				return run(append([]string{"-rc", *rc, "send", "-t", "standing orders"}, rest[1:]...))
			}
			*topic, *n = "standing orders", 1000
		}
		if err := f.Parse(rest); err != nil {
			return err
		}
		if *ch, err = to(*ch); err != nil {
			return err
		}
		msgs, _, err := c.Messages([]narrowTerm{{"channel", *ch}, {"topic", *topic}}, "newest", *n, 0, false)
		if err == nil && len(msgs) == 0 { // maybe resolved since
			msgs, _, err = c.Messages([]narrowTerm{{"channel", *ch}, {"topic", resolvedPrefix + *topic}}, "newest", *n, 0, false)
		}
		if err != nil {
			return err
		}
		if cmd == "orders" {
			if *all {
				msgs = msgs[max(0, len(msgs)-100):]
			} else {
				me, _, err := c.Me()
				if err != nil {
					return err
				}
				msgs = myOrders(msgs, me, c.Owner)
			}
		}
		for _, m := range msgs {
			printMsg(m)
		}
	case "wait":
		ch := f.String("c", "", "channel")
		topic := f.String("t", "", "topic")
		owner = f.Bool("owner", false, "the owner's channel")
		timeout := f.Duration("timeout", time.Hour, "give up after")
		if err := f.Parse(rest); err != nil {
			return err
		}
		if *ch, err = to(*ch); err != nil {
			return err
		}
		return wait(c, *ch, *topic, *timeout)
	case "resolve", "unresolve":
		ch := f.String("c", "", "channel")
		owner = f.Bool("owner", false, "the owner's channel")
		topic := f.String("t", "", "topic")
		if err := f.Parse(rest); err != nil {
			return err
		}
		if *ch, err = to(*ch); err != nil {
			return err
		}
		from, to := *topic, resolvedPrefix+*topic
		if cmd == "unresolve" {
			from, to = to, from
		}
		return renameTopic(c, *ch, from, to)
	case "ack":
		var id int
		if len(rest) != 1 {
			return errors.New("ack needs a message id")
		}
		if _, err := fmt.Sscan(rest[0], &id); err != nil {
			return err
		}
		return c.React(id, "check")
	case "register", "unregister":
		session := f.String("session", os.Getenv("ZMX_SESSION"), "terminal session to wake")
		h := f.String("host", host, "host the session runs on")
		if err := f.Parse(rest); err != nil {
			return err
		}
		role := *as
		if role == "" {
			role = strings.TrimSuffix(filepath.Base(*rc), ".zuliprc")
		}
		for _, v := range []string{role, *session, *h} {
			if !safe.MatchString(v) {
				return fmt.Errorf("role, session and host must match %s, got %q", safe, v)
			}
		}
		return toPorter(c, *rc, fmt.Sprintf("zm-%s role=%s host=%s session=%s", cmd, role, *h, *session))
	case "rollcall":
		if len(rest) == 0 {
			return errors.New("rollcall needs text")
		}
		return run(append([]string{"-rc", *rc, "send", "-t", "roll call"}, rest...))
	case "where":
		work, err := to("")
		if err != nil {
			return err
		}
		fmt.Printf("owner channel: %s\nwork channel:  %s\n", c.Channel, work)
		if c.Boss != "" {
			fmt.Printf("boss:          %s (only role %s talks to them)\n", c.Boss, c.Frontdoor)
		}
	case "join":
		if len(rest) > 0 {
			return toPorter(c, *rc, "zm-join "+strings.Join(rest, " "))
		}
		work, err := to("")
		if err != nil {
			return err
		}
		for _, ch := range []string{c.Channel, work} {
			if err := toPorter(c, *rc, "zm-join "+ch); err != nil {
				return err
			}
		}
	case "restart":
		role := *as
		if len(rest) > 0 {
			role = rest[0]
		}
		if !safe.MatchString(role) {
			return fmt.Errorf("role must match %s, got %q", safe, role)
		}
		return toPorter(c, *rc, "zm-restart role="+role)
	case "get":
		out := f.String("o", "", "output file (default: the upload's name in the current directory)")
		if err := f.Parse(rest); err != nil {
			return err
		}
		if f.NArg() != 1 {
			return errors.New("get needs one /user_uploads/… link")
		}
		dest := *out
		if dest == "" {
			dest = filepath.Base(f.Arg(0))
		}
		if err := c.Download(f.Arg(0), dest); err != nil {
			return err
		}
		fmt.Println(dest)
	case "whoami":
		id, name, err := c.Me()
		if err != nil {
			return err
		}
		fmt.Printf("%s <%s> id=%d\n", name, c.Email, id)
	case "daemon":
		h := f.String("host", host, "this host's name, as agents register it")
		if err := f.Parse(rest); err != nil {
			return err
		}
		return runDaemon(c, *h)
	default:
		g.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

// wait is for short-lived helpers that have no registered session to wake: it blocks on an
// event queue narrowed to one topic until another account posts there.
func wait(c *Client, channel, topic string, timeout time.Duration) error {
	if channel == "" || topic == "" {
		return errors.New("wait needs -c and -t")
	}
	me, _, err := c.Me()
	if err != nil {
		return err
	}
	q, last, err := c.RegisterQueue([2]string{"channel", channel}, [2]string{"topic", topic})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		evs, err := c.Events(q, last)
		if err != nil {
			return err // the queue outlives a long-poll by minutes; any error here is real
		}
		for _, ev := range evs {
			last = max(last, ev.ID)
			if ev.Type == "message" && ev.Message.SenderID != me {
				msgs, _, err := c.Messages([]narrowTerm{{"id", ev.Message.ID}}, strconv.Itoa(ev.Message.ID), 0, 0, false)
				if err != nil || len(msgs) == 0 {
					return fmt.Errorf("fetch %d: %v", ev.Message.ID, err)
				}
				printMsg(msgs[0])
				return nil
			}
		}
	}
	os.Exit(3)
	return nil
}

const resolvedPrefix = "✔ " // Zulip's own marker for a resolved topic

func renameTopic(c *Client, channel, from, to string) error {
	msgs, _, err := c.Messages([]narrowTerm{{"channel", channel}, {"topic", from}}, "newest", 1, 0, false)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return fmt.Errorf("no topic %q in #%s", from, channel)
	}
	return c.RenameTopic(msgs[0].ID, to)
}

// reopen un-resolves "✔ topic" before posting to "topic"; otherwise Zulip would start a second,
// unresolved topic of the same name next to the resolved one.
func reopen(c *Client, channel, topic string) error {
	if strings.HasPrefix(topic, resolvedPrefix) {
		return nil
	}
	err := renameTopic(c, channel, resolvedPrefix+topic, topic)
	if err != nil && strings.HasPrefix(err.Error(), "no topic") {
		return nil
	}
	return err
}

// lastOtherPoster returns who posted last in m's topic before m, other than m's sender: the one
// an owner's message without a mention is meant for. The daemon wakes them; their inbox shows it.
func lastOtherPoster(c *Client, m Message) (int, error) {
	msgs, _, err := c.Messages([]narrowTerm{{"channel", m.ChannelName()}, {"topic", m.Topic}}, strconv.Itoa(m.ID), 50, 0, false)
	if err != nil {
		return 0, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		// Humans (owner, boss) are skipped: an unmentioned reply is meant for the last AGENT here.
		if msgs[i].ID < m.ID && msgs[i].SenderID != m.SenderID && !c.human(msgs[i].SenderEmail) {
			return msgs[i].SenderID, nil
		}
	}
	return 0, nil
}

// unreadReplies finds unread messages meant for me that Zulip doesn't flag as mentions:
//   - quote-replies to my messages: only a silent mention, raw text starts `@_**Name|ID** [said](…)`;
//   - the owner's or boss's messages without any mention in a topic where I posted last (see
//     lastOtherPoster), or, if I'm the front door, in a topic no one else posted in yet.
//
// ponytail: scans the newest 200 unread messages; a quote-reply quoted again also matches.
func unreadReplies(c *Client) ([]Message, error) {
	me, _, err := c.Me()
	if err != nil {
		return nil, err
	}
	msgs, _, err := c.Messages([]narrowTerm{{"is", "unread"}}, "newest", 200, 0, false)
	if err != nil {
		return nil, err
	}
	mark := fmt.Sprintf("|%d** [said](", me)
	var out []Message
	for _, m := range msgs {
		switch {
		case m.SenderID == me:
		case strings.Contains(m.Content, mark):
			out = append(out, m)
		case c.human(m.SenderEmail) && !strings.Contains(m.Content, "@**") && !strings.Contains(m.Content, "[said](") && m.ChannelName() != "":
			if uid, err := lastOtherPoster(c, m); err == nil && (uid == me || uid == 0 && c.Frontdoor != "" && c.Frontdoor == c.Role) {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// unanswered returns mentions of me from the last day that are already read but that I neither
// reacted to nor answered later in their topic: e.g. read by a session that was then restarted.
func unanswered(c *Client, skip map[int]bool) ([]Message, error) {
	me, _, err := c.Me()
	if err != nil {
		return nil, err
	}
	since := time.Now().Add(-24 * time.Hour).Unix()
	mentions, _, err := c.Messages([]narrowTerm{{"is", "mentioned"}}, "newest", 100, 0, false)
	if err != nil {
		return nil, err
	}
	mine, _, err := c.Messages([]narrowTerm{{"sender", c.Email}}, "newest", 200, 0, false)
	if err != nil {
		return nil, err
	}
	topic := func(m Message) string { return m.ChannelName() + ">" + strings.TrimPrefix(m.Topic, "✔ ") }
	lastPost := map[string]int{}
	for _, m := range mine {
		lastPost[topic(m)] = max(lastPost[topic(m)], m.ID)
	}
	var out []Message
	for _, m := range mentions {
		if m.Timestamp < since || skip[m.ID] || m.SenderID == me || m.ChannelName() == "" ||
			reactedBy(m, me) || lastPost[topic(m)] > m.ID {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// myOrders keeps what a role applies on start: its own latest standing-orders list and every
// owner post after it (all owner posts if it has no list yet). Other roles' lists are noise.
func myOrders(msgs []Message, me int, owner string) []Message {
	start := 0
	for i, m := range msgs {
		if m.SenderID == me {
			start = i
		}
	}
	var out []Message
	for i, m := range msgs[start:] {
		if (i == 0 && m.SenderID == me) || (owner != "" && m.SenderEmail == owner) {
			out = append(out, m)
		}
	}
	return out
}

// toPorter sends a request to the daemon; register/join are idempotent, so no retry bookkeeping.
func toPorter(c *Client, rc, text string) error {
	if c.Porter == "" {
		return errors.New("no [zm] porter in " + rc)
	}
	return c.SendDM(c.Porter, text)
}

func printMsg(m Message) {
	t := time.Unix(m.Timestamp, 0).Format("2006-01-02 15:04")
	fmt.Printf("--- id=%d #%s > %s | %s | %s\n%s\n\n", m.ID, m.ChannelName(), m.Topic, m.SenderName, t, m.Content)
}

// sendOnce posts and retries; before each retry it checks whether the earlier attempt actually
// landed (same text from me in this topic within the last minute), so a lost response never
// produces a duplicate. ponytail: an intentional identical repeat within that minute is dropped.
func sendOnce(c *Client, channel, topic, text string) (int, error) {
	since := time.Now().Add(-time.Minute).Unix()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 3 * time.Second)
			if id := findSent(c, channel, topic, text, since); id > 0 {
				return id, nil
			}
		}
		var id int
		if id, err = c.Send(channel, topic, text); err == nil {
			return id, nil
		}
		var api *APIError
		if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 && api.Status != 429 {
			return 0, err // our mistake (bad channel, no permission): retrying won't help
		}
	}
	return 0, err
}

func findSent(c *Client, channel, topic, text string, since int64) int {
	msgs, _, err := c.Messages([]narrowTerm{{"channel", channel}, {"topic", topic}, {"sender", c.Email}}, "newest", 20, 0, false)
	if err != nil {
		return 0
	}
	for _, m := range msgs {
		if m.Timestamp >= since && strings.TrimSpace(m.Content) == strings.TrimSpace(text) {
			return m.ID
		}
	}
	return 0
}
