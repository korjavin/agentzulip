package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Client is a minimal Zulip REST client for one account (a bot).
type Client struct {
	Site, Email, Key string
	Porter           string // [zm] porter: the daemon bot's email; agents DM it register/join requests
	Owner            string // [zm] owner: the human's email (daemon: added to every channel it creates; both: unmentioned replies)
	Restart          string // [zm] restart (daemon only): run as `RESTART SESSION` to restart an agent's session
	Channel          string // [zm] channel, or $ZM_CHANNEL: the project's owner channel; work is Channel+"-work"
	http             *http.Client
}

type Reaction struct {
	EmojiName string `json:"emoji_name"`
	UserID    int    `json:"user_id"`
}

type Message struct {
	ID          int        `json:"id"`
	SenderID    int        `json:"sender_id"`
	SenderName  string     `json:"sender_full_name"`
	SenderEmail string     `json:"sender_email"`
	Channel     any        `json:"display_recipient"` // string for channel messages, list for DMs
	Topic       string     `json:"subject"`
	Content     string     `json:"content"`
	Timestamp   int64      `json:"timestamp"`
	Reactions   []Reaction `json:"reactions"`
}

func (m Message) ChannelName() string { s, _ := m.Channel.(string); return s }

// APIError is a Zulip error response ({"result":"error","code":...}).
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("zulip %d %s: %s", e.Status, e.Code, e.Msg) }

// loadRC reads a standard zuliprc ([api] email/key/site) plus an optional [zm] section.
func loadRC(path string) (*Client, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := &Client{http: &http.Client{Timeout: 120 * time.Second}}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch section + "." + k {
		case "api.email":
			c.Email = v
		case "api.key":
			c.Key = v
		case "api.key_cmd": // keep the key in a secret store: key_cmd = pass show zulip/ios
			out, err := exec.Command("sh", "-c", v).Output()
			if err != nil {
				return nil, fmt.Errorf("%s: key_cmd: %w", path, err)
			}
			c.Key = strings.TrimSpace(string(out))
		case "api.site":
			c.Site = strings.TrimRight(v, "/")
		case "zm.porter":
			c.Porter = v
		case "zm.owner":
			c.Owner = v
		case "zm.restart":
			c.Restart = v
		case "zm.channel":
			c.Channel = v
		}
	}
	if c.Email == "" || c.Key == "" || c.Site == "" {
		return nil, fmt.Errorf("%s: need email, key and site in [api]", path)
	}
	return c, sc.Err()
}

// call does one API request; params go in the query for GET and the form body otherwise.
func (c *Client) call(method, path string, params url.Values, out any) error {
	u := c.Site + "/api/v1" + path
	var body io.Reader
	if method == http.MethodGet {
		u += "?" + params.Encode()
	} else {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Email, c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var res struct {
		Result string `json:"result"`
		Code   string `json:"code"`
		Msg    string `json:"msg"`
	}
	_ = json.Unmarshal(data, &res)
	if resp.StatusCode != 200 || res.Result != "success" {
		return &APIError{resp.StatusCode, res.Code, res.Msg}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func jsonParam(v any) string { b, _ := json.Marshal(v); return string(b) }

type narrowTerm struct {
	Operator string `json:"operator"`
	Operand  any    `json:"operand"`
}

// Messages fetches messages around anchor ("newest", "first_unread" or an id).
func (c *Client) Messages(narrow []narrowTerm, anchor string, before, after int, markdown bool) (msgs []Message, foundOldest bool, err error) {
	var res struct {
		Messages    []Message `json:"messages"`
		FoundOldest bool      `json:"found_oldest"`
	}
	err = c.call("GET", "/messages", url.Values{
		"narrow":         {jsonParam(narrow)},
		"anchor":         {anchor},
		"num_before":     {fmt.Sprint(before)},
		"num_after":      {fmt.Sprint(after)},
		"apply_markdown": {fmt.Sprint(markdown)},
	}, &res)
	return res.Messages, res.FoundOldest, err
}

func (c *Client) Send(channel, topic, content string) (int, error) {
	var res struct {
		ID int `json:"id"`
	}
	err := c.call("POST", "/messages", url.Values{
		"type": {"stream"}, "to": {channel}, "topic": {topic}, "content": {content},
	}, &res)
	return res.ID, err
}

func (c *Client) MarkRead(ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	return c.call("POST", "/messages/flags", url.Values{
		"messages": {jsonParam(ids)}, "op": {"add"}, "flag": {"read"},
	}, nil)
}

func (c *Client) React(id int, emoji string) error {
	err := c.call("POST", fmt.Sprintf("/messages/%d/reactions", id), url.Values{"emoji_name": {emoji}}, nil)
	if e, ok := err.(*APIError); ok && e.Code == "REACTION_ALREADY_EXISTS" {
		return nil
	}
	return err
}

// RenameTopic moves a whole topic (all its messages) to a new name, given any message in it.
func (c *Client) RenameTopic(anyMsgID int, newTopic string) error {
	return c.call("PATCH", fmt.Sprintf("/messages/%d", anyMsgID), url.Values{
		"topic":                           {newTopic},
		"propagate_mode":                  {"change_all"},
		"send_notification_to_old_thread": {"false"},
		"send_notification_to_new_thread": {"false"},
	}, nil)
}

// SendDM sends a direct message to one user by email.
func (c *Client) SendDM(to, content string) error {
	return c.call("POST", "/messages", url.Values{
		"type": {"direct"}, "to": {jsonParam([]string{to})}, "content": {content},
	}, nil)
}

// Subscribe adds users (by email) to a channel, creating it as private if it doesn't exist.
func (c *Client) Subscribe(channel string, emails ...string) error {
	return c.call("POST", "/users/me/subscriptions", url.Values{
		"subscriptions": {jsonParam([]map[string]string{{"name": channel}})},
		"principals":    {jsonParam(emails)},
		"invite_only":   {"true"},
	}, nil)
}

// ChannelExists reports whether a channel of that name exists and is visible to this account.
func (c *Client) ChannelExists(channel string) (bool, error) {
	err := c.call("GET", "/get_stream_id", url.Values{"stream": {channel}}, nil)
	var api *APIError
	if errors.As(err, &api) && api.Status == 400 {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) Subscriptions() ([]string, error) {
	var res struct {
		Subscriptions []struct {
			Name string `json:"name"`
		} `json:"subscriptions"`
	}
	if err := c.call("GET", "/users/me/subscriptions", nil, &res); err != nil {
		return nil, err
	}
	names := make([]string, len(res.Subscriptions))
	for i, s := range res.Subscriptions {
		names[i] = s.Name
	}
	return names, nil
}

// Download saves an uploaded file ("/user_uploads/…" as it appears in a message, or a full URL)
// to dest: the API trades it for a short-lived public URL, which is then fetched.
func (c *Client) Download(upload, dest string) error {
	_, rest, ok := strings.Cut(upload, "/user_uploads/")
	if !ok {
		return fmt.Errorf("not an upload link: %q", upload)
	}
	var res struct {
		URL string `json:"url"`
	}
	if err := c.call("GET", "/user_uploads/"+rest, nil, &res); err != nil {
		return err
	}
	resp, err := http.Get(c.Site + res.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (c *Client) Me() (id int, name string, err error) {
	var res struct {
		UserID   int    `json:"user_id"`
		FullName string `json:"full_name"`
	}
	err = c.call("GET", "/users/me", nil, &res)
	return res.UserID, res.FullName, err
}

type Event struct {
	ID      int     `json:"id"`
	Type    string  `json:"type"`
	Message Message `json:"message"`
}

// RegisterQueue opens an event queue for messages, optionally narrowed (e.g. to one topic).
func (c *Client) RegisterQueue(narrow ...[2]string) (queueID string, lastEventID int, err error) {
	var res struct {
		QueueID     string `json:"queue_id"`
		LastEventID int    `json:"last_event_id"`
	}
	p := url.Values{"event_types": {`["message"]`}, "apply_markdown": {"true"}}
	if len(narrow) > 0 {
		p.Set("narrow", jsonParam(narrow))
	}
	err = c.call("POST", "/register", p, &res)
	return res.QueueID, res.LastEventID, err
}

// Events long-polls the queue; Zulip answers within ~1 minute with events or a heartbeat.
func (c *Client) Events(queueID string, lastEventID int) ([]Event, error) {
	var res struct {
		Events []Event `json:"events"`
	}
	err := c.call("GET", "/events", url.Values{
		"queue_id": {queueID}, "last_event_id": {fmt.Sprint(lastEventID)},
	}, &res)
	return res.Events, err
}
