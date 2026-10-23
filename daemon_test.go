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
	}
	want := []pending{{1, 2}, {1, 4}}
	if got := unhandled(msgs, 50); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func itoa(i int) string { return string(rune('0' + i)) }
