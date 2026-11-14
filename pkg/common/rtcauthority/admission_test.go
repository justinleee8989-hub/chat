package rtcauthority

import "testing"

func TestRejoinHasIndependentMediaIdentity(t *testing.T) {
	s := State{Calls: map[string]*Call{}}
	c, _ := s.Create("r", "owner", "", "group", 1000)
	s.Action("r", "user", "join", 1001)
	old := c.Members["user"].Identity
	s.Action("r", "user", "leave", 1002)
	if !c.Removals[old] {
		t.Fatal("missing persisted cleanup")
	}
	s.Action("r", "user", "join", 1003)
	next := c.Members["user"].Identity
	if old == "" || old == next {
		t.Fatal("rejoin must not reuse the removed media identity")
	}
	if c.UserForIdentity(old) != "" || c.UserForIdentity(next) != "user" {
		t.Fatal("old identity remained authorized")
	}
	if !c.Removals[old] {
		t.Fatal("rejoin discarded old cleanup")
	}
	s.Action("r", "user", "join", 1004)
	if c.Members["user"].Identity != next {
		t.Fatal("duplicate join rotates active identity")
	}
}
