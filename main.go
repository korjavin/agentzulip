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
	"strings"
	"time"
)

const usage = `zm - agent messaging over Zulip

usage: zm [-as ROLE | -rc FILE] COMMAND [flags]

  send -c CHANNEL -t TOPIC [-to NAME,NAME] [TEXT]   post (TEXT or stdin), @-mentioning -to
  inbox [-peek]                                     unread @-mentions of me; marks them read
  read -c CHANNEL -t TOPIC [-n 30]                  recent history of a topic
  ack ID                                            react ✅ ("taken") to a message
  register [-session S] [-host H]                   ask the daemon to wake session S on mentions of me
  unregister [-session S] [-host H]                 stop that
  join CHANNEL                                      ask the daemon to subscribe me to CHANNEL
  whoami                                            print my Zulip identity
  daemon [-host H]                                  watch Zulip and wake registered sessions

ROLE picks ~/.config/zm/ROLE.zuliprc (default: $ZM_AS). The file is a standard zuliprc
([api] email, key, site; or key_cmd = a command printing the key) plus
[zm] registry = CHANNEL for register/unregister/join/daemon.
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
		if c.Registry == "" {
			return errors.New("no [zm] registry channel in " + *rc)
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
		_, err := sendOnce(c, c.Registry, "registry", fmt.Sprintf("zm-%s role=%s host=%s session=%s", cmd, role, *h, *session))
		return err
	case "join":
		if len(rest) == 0 || c.Registry == "" {
			return errors.New("join needs a channel and a [zm] registry channel")
		}
		_, err := sendOnce(c, c.Registry, "registry", "zm-join "+strings.Join(rest, " "))
		return err
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
		if c.Registry == "" {
			return errors.New("no [zm] registry channel in " + *rc)
		}
		return runDaemon(c, *h)
	default:
		g.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
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
