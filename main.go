// zm: let coding agents talk to each other through Zulip.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const usage = `zm - agent messaging over Zulip

usage: zm [-as ROLE | -rc FILE] COMMAND [flags]

  send -c CHANNEL -t TOPIC [-to NAME,NAME] [TEXT]   post (TEXT or stdin), @-mentioning -to
  inbox [-peek]                                     unread @-mentions of me; marks them read
  read -c CHANNEL -t TOPIC [-n 30]                  recent history of a topic
  wait -c CHANNEL -t TOPIC [-timeout 1h]            block until someone else posts there; print it
                                                    (exit 3 on timeout)
  ack ID                                            react ✅ ("taken") to a message
  register [-session S] [-host H]                   ask the daemon to wake session S on mentions of me
  unregister [-session S] [-host H]                 stop that
  join CHANNEL                                      ask the daemon to add me (and the owner) to CHANNEL,
                                                    creating it as private if needed
  whoami                                            print my Zulip identity
  daemon [-host H]                                  watch Zulip and wake registered sessions

ROLE picks ~/.config/zm/ROLE.zuliprc (default: $ZM_AS). The file is a standard zuliprc
([api] email, key, site; or key_cmd = a command printing the key) plus a [zm] section:
porter = EMAIL of the daemon's bot (register/unregister/join are DMs to it), and for the
daemon itself owner = EMAIL of the human added to every channel it creates.
`

var safe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	host, _ := os.Hostname()
	switch cmd {
	case "send":
		ch := f.String("c", "", "channel")
		topic := f.String("t", "", "topic")
		to := f.String("to", "", "comma-separated names to @-mention")
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
		if *ch == "" || *topic == "" || strings.TrimSpace(text) == "" {
			return errors.New("send needs -c, -t and text")
		}
		var mentions []string
		for _, n := range strings.Split(*to, ",") {
			if n = strings.TrimSpace(n); n != "" {
				mentions = append(mentions, "@**"+n+"**")
			}
		}
		if len(mentions) > 0 {
			text = strings.Join(mentions, " ") + " " + text
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
		ids := make([]int, len(msgs))
		for i, m := range msgs {
			printMsg(m)
			ids[i] = m.ID
		}
		if len(msgs) == 0 {
			fmt.Println("(no unread mentions)")
		}
		if !*peek {
			return c.MarkRead(ids)
		}
	case "read":
		ch := f.String("c", "", "channel")
		topic := f.String("t", "", "topic")
		n := f.Int("n", 30, "messages")
		if err := f.Parse(rest); err != nil {
			return err
		}
		msgs, _, err := c.Messages([]narrowTerm{{"channel", *ch}, {"topic", *topic}}, "newest", *n, 0, false)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			printMsg(m)
		}
	case "wait":
		ch := f.String("c", "", "channel")
		topic := f.String("t", "", "topic")
		timeout := f.Duration("timeout", time.Hour, "give up after")
		if err := f.Parse(rest); err != nil {
			return err
		}
		return wait(c, *ch, *topic, *timeout)
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
	case "join":
		if len(rest) == 0 {
			return errors.New("join needs a channel")
		}
		return toPorter(c, *rc, "zm-join "+strings.Join(rest, " "))
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

// sendOnce posts with an op:<id> footer and retries; before each retry it checks whether the
// earlier attempt actually landed, so a lost response never produces a duplicate.
func sendOnce(c *Client, channel, topic, text string) (int, error) {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	footer := "op:" + hex.EncodeToString(b)
	content := text + "\n\n" + footer
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 3 * time.Second)
			if id := findOp(c, channel, topic, footer); id > 0 {
				return id, nil
			}
		}
		var id int
		if id, err = c.Send(channel, topic, content); err == nil {
			return id, nil
		}
		var api *APIError
		if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 && api.Status != 429 {
			return 0, err // our mistake (bad channel, no permission): retrying won't help
		}
	}
	return 0, err
}

func findOp(c *Client, channel, topic, footer string) int {
	msgs, _, err := c.Messages([]narrowTerm{{"channel", channel}, {"topic", topic}, {"sender", c.Email}}, "newest", 20, 0, false)
	if err != nil {
		return 0
	}
	for _, m := range msgs {
		if strings.HasSuffix(strings.TrimSpace(m.Content), footer) {
			return m.ID
		}
	}
	return 0
}
