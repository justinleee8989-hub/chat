package rtcauthority

import (
	"context"
	"errors"
	"testing"
)

func TestInspectAllowsOnlyCurrentGroupMemberAndReturnsMinimalCall(t *testing.T) {
	state, _ := newGroupState(t, "group-inspect")
	repo := newMemoryStateRepository(state)
	var checks int
	service := &Service{
		Store: repo,
		GroupMember: func(_ context.Context, groupID, user string) error {
			checks++
			if groupID != "group-test" {
				t.Fatalf("checked group %q, want group-test", groupID)
			}
			if user != "member" {
				return ErrPermission
			}
			return nil
		},
	}

	calls, err := service.Execute(context.Background(), "member", Request{Room: "group-inspect", Action: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	if checks != 1 || len(calls) != 1 {
		t.Fatalf("membership checks=%d result count=%d, want 1 each", checks, len(calls))
	}
	if calls[0].Room != "group-inspect" || calls[0].GroupID != "group-test" || calls[0].Status != "accepted" {
		t.Fatalf("unexpected inspected summary: %+v", calls[0])
	}
	if calls[0].Members != nil || calls[0].Removals != nil || calls[0].Participants != nil || calls[0].Caller != "" {
		t.Fatalf("inspect returned internal participant state: %+v", calls[0])
	}

	if _, err := service.Execute(context.Background(), "stranger", Request{Room: "group-inspect", Action: "inspect"}); !errors.Is(err, ErrPermission) {
		t.Fatalf("nonmember inspect error = %v, want ErrPermission", err)
	}
	if checks != 2 {
		t.Fatalf("membership checks after denied user=%d, want 2", checks)
	}
}

func TestInspectMissingRoomDoesNotCallGroupMembership(t *testing.T) {
	repo := newMemoryStateRepository(State{Calls: map[string]*Call{}})
	groupChecks := 0
	service := &Service{
		Store: repo,
		GroupMember: func(context.Context, string, string) error {
			groupChecks++
			return nil
		},
	}

	if _, err := service.Execute(context.Background(), "user", Request{Room: "missing", Action: "inspect"}); !errors.Is(err, ErrState) {
		t.Fatalf("inspect missing room error = %v, want ErrState", err)
	}
	if groupChecks != 0 {
		t.Fatalf("checked membership %d times for missing room, want 0", groupChecks)
	}
}
