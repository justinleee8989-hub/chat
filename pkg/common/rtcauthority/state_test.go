package rtcauthority

import "testing"

func TestOnlyCalleeAcceptsAndTerminalCannotRevive(t *testing.T) {
	s := State{Calls: map[string]*Call{}}
	c, err := s.Create("room", "a", "b", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Action(c.Room, "a", "accept", 1001); err == nil {
		t.Fatal("caller accepted own call")
	}
	if _, err = s.Action(c.Room, "x", "accept", 1001); err == nil {
		t.Fatal("stranger accepted")
	}
	if _, err = s.Action(c.Room, "b", "accept", 1001); err != nil {
		t.Fatal(err)
	}
	if !c.Allows("a", 1002) || c.Allows("x", 1002) {
		t.Fatal("invalid membership")
	}
	if _, err = s.Action(c.Room, "a", "end", 1002); err != nil {
		t.Fatal(err)
	}
	if c.Allows("b", 1003) {
		t.Fatal("ended token allowed")
	}
	if _, err = s.Action(c.Room, "b", "accept", 1003); err == nil {
		t.Fatal("terminal resurrected")
	}
}
func TestTimeoutLeaseAndBusy(t *testing.T) {
	s := State{Calls: map[string]*Call{}}
	c, _ := s.Create("r", "a", "b", "", 1000)
	if _, err := s.Create("r2", "c", "b", "", 1001); err == nil {
		t.Fatal("busy not atomic")
	}
	s.Sweep(46001)
	if c.Status != "missed" {
		t.Fatal(c.Status)
	}
	c, _ = s.Create("r2", "a", "b", "", 47000)
	s.Action(c.Room, "b", "accept", 47001)
	s.Sweep(83002)
	if c.Status != "disconnected" {
		t.Fatal(c.Status)
	}
}
