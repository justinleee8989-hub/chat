package rtcauthority

import (
	"errors"
	"testing"
)

func newState() *State {
	return &State{Calls: map[string]*Call{}}
}

func TestDirectCallRolePermissions(t *testing.T) {
	s := newState()
	call, err := s.Create("room", "caller", "callee", "", 1000)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		user   string
		action string
	}{
		{user: "caller", action: "accept"},
		{user: "caller", action: "reject"},
		{user: "callee", action: "cancel"},
		{user: "stranger", action: "get"},
		{user: "stranger", action: "heartbeat"},
	} {
		if _, err := s.Action(call.Room, tc.user, tc.action, 1001); !errors.Is(err, ErrPermission) {
			t.Errorf("Action(%q, %q) error = %v, want ErrPermission", tc.user, tc.action, err)
		}
	}

	if _, err := s.Action(call.Room, "callee", "accept", 1002); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Action(call.Room, "caller", "heartbeat", 1003); err != nil {
		t.Fatalf("accepted caller heartbeat: %v", err)
	}
	if _, err := s.Action(call.Room, "callee", "heartbeat", 1003); err != nil {
		t.Fatalf("accepted callee heartbeat: %v", err)
	}
}

func TestTerminalActionsAreIdempotentAndCannotReviveCall(t *testing.T) {
	s := newState()
	call, err := s.Create("room", "caller", "callee", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Action(call.Room, "caller", "cancel", 1100); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		user   string
		action string
	}{
		{user: "caller", action: "cancel"},
		{user: "callee", action: "reject"},
		{user: "caller", action: "end"},
		{user: "callee", action: "leave"},
	} {
		got, err := s.Action(call.Room, tc.user, tc.action, 1200)
		if err != nil || got != call {
			t.Errorf("terminal Action(%q, %q) = (%p, %v), want existing call and nil", tc.user, tc.action, got, err)
		}
	}
	if call.Status != "canceled" || call.Ended != 1100 {
		t.Fatalf("terminal call changed after duplicate actions: status=%q ended=%d", call.Status, call.Ended)
	}
	if _, err := s.Action(call.Room, "stranger", "cancel", 1200); !errors.Is(err, ErrPermission) {
		t.Errorf("stranger terminal action error = %v, want ErrPermission", err)
	}
	if _, err := s.Action(call.Room, "callee", "accept", 1200); !errors.Is(err, ErrState) {
		t.Errorf("late accept error = %v, want ErrState", err)
	}
	if s.Busy("caller") || s.Busy("callee") {
		t.Fatal("terminal participants remain busy")
	}
}

func TestBusyReservationsCoverDirectAndGroupParticipants(t *testing.T) {
	s := newState()
	if _, err := s.Create("direct", "alice", "bob", "", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("other", "charlie", "bob", "", 1001); !errors.Is(err, ErrBusy) {
		t.Errorf("second call to ringing callee error = %v, want ErrBusy", err)
	}

	group, err := s.Create("group-room", "owner", "", "group-id", 1002)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("owner-call", "owner", "other", "", 1003); !errors.Is(err, ErrBusy) {
		t.Errorf("group caller reservation error = %v, want ErrBusy", err)
	}
	if !s.Busy("owner") || !s.Busy("alice") || !s.Busy("bob") {
		t.Fatal("ringing or group participant was not marked busy")
	}

	if _, err := s.Action(group.Room, "owner", "leave", 1004); err != nil {
		t.Fatal(err)
	}
	if s.Busy("owner") {
		t.Fatal("owner remains busy after leaving the group")
	}
}

func TestGroupJoinLeaveAndBusyAdmission(t *testing.T) {
	s := newState()
	group, err := s.Create("group-room", "owner", "", "group-id", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if group.Status != "accepted" || !group.Allows("owner", 1001) {
		t.Fatal("group call did not start accepted with its caller admitted")
	}

	if _, err := s.Create("direct", "busy-user", "callee", "", 1001); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Action(group.Room, "busy-user", "join", 1002); !errors.Is(err, ErrBusy) {
		t.Errorf("busy group join error = %v, want ErrBusy", err)
	}

	if _, err := s.Action(group.Room, "member", "join", 1003); err != nil {
		t.Fatal(err)
	}
	if !group.Allows("member", 1004) || !s.Busy("member") {
		t.Fatal("joined group member lacks access or busy reservation")
	}
	if _, err := s.Action(group.Room, "owner", "leave", 1005); err != nil {
		t.Fatal(err)
	}
	if group.Terminal() || group.Members["owner"] != nil || s.Busy("owner") {
		t.Fatal("group did not remain active after owner left while member remained")
	}
	if _, err := s.Action(group.Room, "member", "leave", 1006); err != nil {
		t.Fatal(err)
	}
	if group.Status != "ended" || group.Ended != 1006 || s.Busy("member") {
		t.Fatalf("last group leave did not end call cleanly: status=%q ended=%d", group.Status, group.Ended)
	}
}

func TestLeaseBoundaryAndHeartbeat(t *testing.T) {
	s := newState()
	call, err := s.Create("room", "caller", "callee", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Action(call.Room, "callee", "accept", 1001); err != nil {
		t.Fatal(err)
	}

	initialLease := call.Members["caller"].Lease
	if call.Allows("caller", initialLease-1) != true || call.Allows("caller", initialLease) {
		t.Fatal("lease access boundary must be exclusive at Lease")
	}
	if _, err := s.Action(call.Room, "caller", "heartbeat", initialLease-10); err != nil {
		t.Fatalf("heartbeat before lease expiry: %v", err)
	}
	if call.Members["caller"].Lease != initialLease-10+LeaseMS {
		t.Fatal("heartbeat did not extend caller lease")
	}

	s.Sweep(call.Members["callee"].Lease)
	if call.Status != "disconnected" {
		t.Fatalf("expired direct-call participant left status %q, want disconnected", call.Status)
	}
	if call.Allows("caller", call.Ended) || call.Allows("callee", call.Ended) {
		t.Fatal("disconnected direct call still allows a participant")
	}
}

func TestLeftAtDisconnectGraceBoundaries(t *testing.T) {
	s := newState()
	call, err := s.Create("room", "caller", "callee", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Action(call.Room, "callee", "accept", 1001); err != nil {
		t.Fatal(err)
	}
	call.Members["caller"].Lease = 100_000
	call.Members["callee"].Lease = 100_000
	call.Members["caller"].LeftAt = 2000

	beforeGrace := 2000 + DisconnectGraceMS - 1
	if !call.Allows("caller", beforeGrace) {
		t.Fatal("participant lost access before disconnect grace elapsed")
	}
	s.Sweep(beforeGrace)
	if call.Status != "accepted" {
		t.Fatalf("call ended before grace elapsed: status=%q", call.Status)
	}

	atGrace := 2000 + DisconnectGraceMS
	if call.Allows("caller", atGrace) {
		t.Fatal("participant retained access at disconnect grace boundary")
	}
	s.Sweep(atGrace)
	if call.Status != "disconnected" || call.Ended != atGrace {
		t.Fatalf("call did not end at disconnect grace boundary: status=%q ended=%d", call.Status, call.Ended)
	}
}

func TestGroupLeaseAndDisconnectGraceRemoveOnlyExpiredMember(t *testing.T) {
	s := newState()
	group, err := s.Create("group-room", "owner", "", "group-id", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Action(group.Room, "member", "join", 1001); err != nil {
		t.Fatal(err)
	}
	group.Members["owner"].Lease = 100_000
	group.Members["member"].Lease = 100_000
	group.Members["member"].LeftAt = 2000

	s.Sweep(2000 + DisconnectGraceMS - 1)
	if group.Members["member"] == nil || group.Status != "accepted" {
		t.Fatal("group removed member or ended before disconnect grace elapsed")
	}
	s.Sweep(2000 + DisconnectGraceMS)
	if group.Members["member"] != nil || group.Status != "accepted" || !s.Busy("owner") || s.Busy("member") {
		t.Fatal("group grace expiry did not remove only the disconnected member")
	}

	memberLease := group.Members["owner"].Lease
	s.Sweep(memberLease)
	if group.Status != "ended" || group.Ended != memberLease {
		t.Fatalf("group did not end when last member lease expired: status=%q ended=%d", group.Status, group.Ended)
	}
}

func TestLateEventsCannotReviveExpiredCall(t *testing.T) {
	s := newState()
	call, err := s.Create("room", "caller", "callee", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	s.Sweep(call.Expires)
	if call.Status != "missed" || call.Ended != call.Expires {
		t.Fatalf("ring timeout state = %q at %d, want missed at %d", call.Status, call.Ended, call.Expires)
	}
	if _, err := s.Action(call.Room, "callee", "accept", call.Expires+1); !errors.Is(err, ErrState) {
		t.Errorf("late accept error = %v, want ErrState", err)
	}
	if _, err := s.Action(call.Room, "caller", "cancel", call.Expires+2); err != nil {
		t.Errorf("idempotent terminal cancel error = %v", err)
	}
	if call.Status != "missed" || call.Ended != call.Expires {
		t.Fatalf("late event changed terminal state: status=%q ended=%d", call.Status, call.Ended)
	}
}
