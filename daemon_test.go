package main

import (
	"reflect"
	"testing"
)

func TestMentionedIDs(t *testing.T) {
	h := `<p><span class="user-mention" data-user-id="12">@ios</span> and ` +
		`<span class="user-mention silent" data-user-id="13">pm</span> ` +
		`<span class="user-mention" data-user-id="*">@all</span> ` +
		`<span class="user-mention" data-user-id="12">@ios</span></p>`
	if got := mentionedIDs(h); !reflect.DeepEqual(got, []int{12}) {
		t.Fatalf("got %v", got)
	}
}

func TestQuoteReply(t *testing.T) {
	reply := `<p><span class="user-mention silent" data-user-id="28">Dev</span> <a href="#narrow/channel/1-x/topic/t/near/3426">said</a>:</p>
<blockquote>
<p><span class="user-mention" data-user-id="30">Bob</span> hi</p>
<blockquote><p>x</p></blockquote>
</blockquote>
<p>answer</p>`
	if got := mentionedIDs(reply); len(got) != 1 || got[0] != 28 {
		t.Fatalf("quote-reply: got %v, want [28] (mention inside the quote ignored)", got)
	}
	if got := mentionedIDs(`<p><span class="user-mention silent" data-user-id="28">Dev</span> hi</p>`); len(got) != 0 {
		t.Fatalf("plain silent mention: got %v, want none", got)
	}
}

func TestParseCommand(t *testing.T) {
	for _, in := range []string{
		"zm-register role=ios host=box session=a.b\n\nop:12ab",
		"<p>zm-register role=ios host=box session=a.b</p>\n<p>op:12ab</p>",
	} {
		cmd, kv, _ := parseCommand(in)
		if cmd != "zm-register" || kv["role"] != "ios" || kv["host"] != "box" || kv["session"] != "a.b" {
			t.Fatalf("%q -> %s %v", in, cmd, kv)
		}
	}
	if cmd, _, rest := parseCommand("<p>zm-join My &amp; Channel</p>\n<p>op:1</p>"); cmd != "zm-join" || rest != "My & Channel" {
		t.Fatalf("join -> %s %q", cmd, rest)
	}
}

func TestUnhandled(t *testing.T) {
	at := func(id int) string { return `<span class="user-mention" data-user-id="` + itoa(id) + `">x</span>` }
	msgs := []Message{
		{ID: 1, SenderID: 1, Topic: "a", Timestamp: 100, Content: at(2) + at(3) + at(4)},
		{ID: 2, SenderID: 3, Topic: "a", Timestamp: 101, Content: "on it"},     // 3 answered
		{ID: 3, SenderID: 4, Topic: "b", Timestamp: 102, Content: "elsewhere"}, // 4 answered in another topic: still pending
		{ID: 4, SenderID: 1, Topic: "a", Timestamp: 103, Content: at(5), Reactions: []Reaction{{"check", 5}}},
		{ID: 5, SenderID: 1, Topic: "a", Timestamp: 1, Content: at(6)},   // too old
		{ID: 6, SenderID: 7, Topic: "a", Timestamp: 104, Content: at(7)}, // self-mention
		{ID: 7, SenderID: 1, Topic: "c", Timestamp: 105, Content: at(8), Reactions: []Reaction{{wokeEmoji, 8}}}, // only woken
		{ID: 8, SenderID: 1, Topic: "c", Timestamp: 106, Content: at(9), Reactions: []Reaction{{readEmoji, 9}}},   // read = acked
	}
	want := []pending{{1, 2}, {1, 4}, {7, 8}}
	if got := unhandled(msgs, 50); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestHumanAndFrontdoor(t *testing.T) {
	old := &Client{Owner: "dev@x"} // a config without boss/frontdoor behaves as before
	if !old.human("dev@x") || old.human("") || old.human("boss@x") {
		t.Fatal("owner-only config")
	}
	if (&Client{}).human("") {
		t.Fatal("empty owner must match nobody")
	}
	c := &Client{Owner: "dev@x", Boss: "boss@x"}
	if !c.human("boss@x") || !c.human("dev@x") || c.human("bot@x") {
		t.Fatal("boss counts as human")
	}
	subs := map[string]map[int]string{"s.front": {7: "front"}, "s.web": {8: "web"}}
	if roleUID(subs, "front") != 7 || roleUID(subs, "") != 0 || roleUID(subs, "nobody") != 0 {
		t.Fatal("roleUID")
	}
}

func TestMyOrders(t *testing.T) {
	msgs := []Message{
		{ID: 1, SenderID: 5, Content: "my old list"},
		{ID: 2, SenderEmail: "o@x", SenderID: 9, Content: "owner before"},
		{ID: 3, SenderID: 6, Content: "other role's list"},
		{ID: 4, SenderID: 5, Content: "my new list"},
		{ID: 5, SenderID: 6, Content: "other role again"},
		{ID: 6, SenderEmail: "o@x", SenderID: 9, Content: "owner after"},
	}
	var got []int
	for _, m := range myOrders(msgs, 5, "o@x") {
		got = append(got, m.ID)
	}
	if !reflect.DeepEqual(got, []int{4, 6}) {
		t.Fatalf("got %v", got)
	}
	got = nil
	for _, m := range myOrders(msgs, 7, "o@x") { // no list yet: every owner post
		got = append(got, m.ID)
	}
	if !reflect.DeepEqual(got, []int{2, 6}) {
		t.Fatalf("no list: got %v", got)
	}
}
