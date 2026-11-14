package messageextras

import "testing"

func TestEditRequiresOriginalAuthor(t *testing.T) {
	s := Record{}
	if s.Apply("bob", "alice", 101, Request{Action: "edit", Text: "changed"}, 1) == nil {
		t.Fatal("non-author edited")
	}
	if err := s.Apply("alice", "alice", 101, Request{Action: "edit", Text: "changed"}, 1); err != nil {
		t.Fatal(err)
	}
	if s.Text == nil || *s.Text != "changed" {
		t.Fatal("edit missing")
	}
}
func TestReactionIsActorBoundAndIdempotent(t *testing.T) {
	s := Record{}
	r := Request{Action: "react", Emoji: "👍", Enabled: true}
	for i := 0; i < 2; i++ {
		if err := s.Apply("bob", "alice", 101, r, 1); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.Reactions["👍"]) != 1 {
		t.Fatal("duplicate")
	}
	r.Enabled = false
	if err := s.Apply("mallory", "alice", 101, r, 2); err != nil {
		t.Fatal(err)
	}
	if !s.Reactions["👍"]["bob"] {
		t.Fatal("removed someone else")
	}
}
func TestRejectUnsupportedMutation(t *testing.T) {
	for _, r := range []Request{{Action: "edit", Text: ""}, {Action: "react", Emoji: "<script>"}, {Action: "delete"}} {
		if (&Record{}).Apply("alice", "alice", 101, r, 1) == nil {
			t.Fatal("invalid accepted")
		}
	}
	if (&Record{}).Apply("alice", "alice", 102, Request{Action: "edit", Text: "x"}, 1) == nil {
		t.Fatal("edited image")
	}
}
