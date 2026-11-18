package messageextras

import "testing"

func TestConfiguredReactionPolicy(t *testing.T) {
	p, err := NewReactionPolicy([]string{"✅"})
	if err != nil {
		t.Fatal(err)
	}
	r := Record{}
	if err = r.ApplyWithPolicy("a", "b", 101, Request{Action: "react", Emoji: "✅", Enabled: true}, 1, p); err != nil {
		t.Fatal(err)
	}
	if err = r.ApplyWithPolicy("a", "b", 101, Request{Action: "react", Emoji: "👍", Enabled: true}, 1, p); err == nil {
		t.Fatal("default emoji bypassed configured policy")
	}
	r.Reactions["👍"] = map[string]bool{"a": true, "b": true}
	if err = r.ApplyWithPolicy("a", "b", 101, Request{Action: "react", Emoji: "👍", Enabled: false}, 1, p); err != nil {
		t.Fatal(err)
	}
	if r.Reactions["👍"]["a"] || !r.Reactions["👍"]["b"] {
		t.Fatal("legacy removal changed another actor")
	}
	for _, values := range [][]string{{""}, {"<script>"}, {"duplicate", "duplicate"}} {
		if _, err := NewReactionPolicy(values); err == nil {
			t.Fatalf("invalid policy accepted %v", values)
		}
	}
}
