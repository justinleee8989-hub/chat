package rtcauthority

import (
	"errors"
	"testing"
)

func TestGroupExpiryClosesAdmissionButNotActiveLeasedCall(t *testing.T) {
	s := newState()
	call, err := s.Create("group-room", "owner", "", "group-id", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Action(call.Room, "member", "join", 1001); err != nil {
		t.Fatal(err)
	}
	originalIdentity := call.Members["member"].Identity
	call.Members["owner"].Lease = call.Expires + LeaseMS
	call.Members["member"].Lease = call.Expires + LeaseMS

	s.Sweep(call.Expires)
	if call.Status != "accepted" {
		t.Fatalf("active group call ended at join-window expiry: status=%q", call.Status)
	}
	if _, err := s.Action(call.Room, "owner", "heartbeat", call.Expires+1); err != nil {
		t.Fatalf("active member could not heartbeat after join-window expiry: %v", err)
	}
	if _, err := s.Action(call.Room, "new-member", "join", call.Expires+2); !errors.Is(err, ErrState) {
		t.Fatalf("new join after group expiry error = %v, want ErrState", err)
	}
	if _, err := s.Action(call.Room, "member", "join", call.Expires+3); err != nil {
		t.Fatalf("existing member could not reconnect after group expiry: %v", err)
	}
	if call.Members["member"].Identity != originalIdentity {
		t.Fatal("reconnecting existing member changed its media identity")
	}
}

func TestLeftGroupMemberRemainsReadableButNotAuthorizedOrBusy(t *testing.T) {
	s := newState()
	call, err := s.Create("group-room", "owner", "", "group-id", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Action(call.Room, "member", "join", 1001); err != nil {
		t.Fatal(err)
	}
	identity := call.Members["member"].Identity
	if _, err := s.Action(call.Room, "member", "leave", 1002); err != nil {
		t.Fatal(err)
	}

	if !call.Participant("member") {
		t.Fatal("departed member cannot read/list the call")
	}
	if _, err := s.Action(call.Room, "member", "get", 1003); err != nil {
		t.Fatalf("departed member get error = %v", err)
	}
	if call.Allows("member", 1003) || call.UserForIdentity(identity) != "" {
		t.Fatal("departed member or old media identity remains authorized")
	}
	if s.Busy("member") {
		t.Fatal("departed member remains busy")
	}
	if _, err := s.Create("direct-room", "member", "another-user", "", 1004); err != nil {
		t.Fatalf("departed member could not start a new call: %v", err)
	}
	if call.Participant("unknown") {
		t.Fatal("unknown user became a call participant")
	}
}

func TestLegacySnapshotMigratesOnlyKnownCurrentMembers(t *testing.T) {
	s := newState()
	call := &Call{
		Room:    "legacy-group",
		Caller:  "owner",
		GroupID: "group-id",
		Status:  "accepted",
		Created: 1000,
		Expires: 901000,
		Members: map[string]*Member{
			"current-member": {Identity: "legacy-current-identity", Lease: 10000},
		},
	}
	s.Calls[call.Room] = call

	if _, err := s.Action(call.Room, "current-member", "get", 1001); err != nil {
		t.Fatalf("legacy current member get error = %v", err)
	}
	if !call.Participant("current-member") || !call.Participant("owner") {
		t.Fatal("legacy caller/current member missing from migrated participant history")
	}
	if _, exists := call.Participants["former-member-without-history"]; exists || call.Participant("former-member-without-history") {
		t.Fatal("migration invented a departed member absent from the legacy snapshot")
	}
	if _, err := s.Action(call.Room, "former-member-without-history", "get", 1002); !errors.Is(err, ErrPermission) {
		t.Fatalf("unknown former member get error = %v, want ErrPermission", err)
	}
}
