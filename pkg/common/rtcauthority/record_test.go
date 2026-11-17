package rtcauthority

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTerminalRecordRetryAndLegacy(t *testing.T) {
	st := State{Calls: map[string]*Call{}}
	c, _ := st.Create("new", "a", "b", "", 1000)
	c.ServerRecordDelivery = true
	legacy, _ := st.Create("old", "c", "d", "", 1000)
	st.Sweep(1000 + RingMS)
	if c.Record == nil || c.Record.EventID != "rtc-terminal:new" || c.DurationSeconds == nil || *c.DurationSeconds != 0 {
		t.Fatal("expiry must queue a zero-duration terminal record")
	}
	if legacy.Record != nil || legacy.DurationSeconds != nil {
		t.Fatal("legacy call was backfilled")
	}
	c.MediaClosed = true
	st.Sweep(1000 + 48*60*60*1000)
	if st.Calls["new"] == nil {
		t.Fatal("pending record pruned")
	}
	repo := newMemoryStateRepository(st)
	count := 0
	svc := Service{Store: repo, SendRecord: func(context.Context, TerminalRecord) error {
		count++
		if count == 1 {
			return errors.New("offline")
		}
		return nil
	}}
	svc.deliverRecords(context.Background())
	svc.deliverRecords(context.Background())
	svc.deliverRecords(context.Background())
	if count != 2 || !repo.call("new").Record.Delivered {
		t.Fatalf("retry count %d", count)
	}
}

func TestMediaDurationIgnoresStaleDuplicateAndForeign(t *testing.T) {
	now := time.Now().UnixMilli()
	st := State{Calls: map[string]*Call{}}
	c, _ := st.Create("r", "a", "b", "", now)
	c.ServerRecordDelivery = true
	st.Action("r", "b", "accept", now)
	a, b := c.MediaIdentity("a"), c.MediaIdentity("b")
	c.mediaEvent("participant_joined", a, "a1", now/1000, now)
	c.mediaEvent("participant_joined", "foreign", "x", now/1000, now)
	c.mediaEvent("participant_joined", b, "b1", now/1000-100, now)
	if c.ConnectedAt != 0 {
		t.Fatal("invalid media event started duration")
	}
	c.mediaEvent("participant_joined", b, "b1", now/1000, now+2000)
	start := c.ConnectedAt
	c.mediaEvent("participant_left", a, "a1", now/1000+3, now+3000)
	c.mediaEvent("participant_joined", a, "a1", now/1000, now+4000)
	if c.Members["a"].LeftAt == 0 {
		t.Fatal("duplicate join cleared leave")
	}
	c.mediaEvent("participant_joined", a, "a2", now/1000+4, now+4000)
	if c.ConnectedAt != start {
		t.Fatal("reconnect reset duration")
	}
	c.end("ended", start+5500)
	if c.DurationSeconds == nil || *c.DurationSeconds != 5 {
		t.Fatal("duration included ringing")
	}
}

func TestTerminalRecordConcurrentWorkers(t *testing.T) {
	st := State{Calls: map[string]*Call{}}
	c, _ := st.Create("r", "a", "b", "", 1000)
	c.ServerRecordDelivery = true
	c.end("canceled", 2000)
	repo := newMemoryStateRepository(st)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	svc := Service{Store: repo, SendRecord: func(context.Context, TerminalRecord) error { close(started); <-release; return nil }}
	go func() { svc.deliverRecords(context.Background()); close(done) }()
	<-started
	svc.deliverRecords(context.Background())
	close(release)
	<-done
	if !repo.call("r").Record.Delivered {
		t.Fatal("delivery not acknowledged")
	}
}

func TestDisconnectedRecordAndExpiredMedia(t *testing.T) {
	st := State{Calls: map[string]*Call{}}
	c, _ := st.Create("r", "a", "b", "", 100000)
	c.ServerRecordDelivery = true
	st.Action("r", "b", "accept", 100000)
	c.mediaEvent("participant_joined", c.MediaIdentity("a"), "a", 140, 140000)
	if c.ConnectedAt != 0 || c.Members["a"].SID != "" {
		t.Fatal("expired admission accepted")
	}
	st.Sweep(140000)
	if c.Status != "disconnected" || c.Record == nil {
		t.Fatal("offline accepted call did not queue record")
	}
	c.end("ended", 150000)
	if c.Status != "disconnected" || c.Ended != 140000 {
		t.Fatal("duplicate end changed result")
	}
}

func TestRecordCapabilityOnlyNewConfiguredSingleCalls(t *testing.T) {
	st := State{Calls: map[string]*Call{}}
	old, _ := st.Create("old", "old-a", "old-b", "", time.Now().UnixMilli())
	old.RequestID = "03838866-2578-414e-8d5c-3e5cd85f8a91"
	repo := newMemoryStateRepository(st)
	svc := newRecoveryService(repo, "")
	svc.SendRecord = func(context.Context, TerminalRecord) error { return nil }
	calls, err := svc.Execute(context.Background(), "a", Request{Action: "create", Peer: "b", RequestID: "03838866-2578-414e-8d5c-3e5cd85f8a92"})
	if err != nil || !calls[0].ServerRecordDelivery {
		t.Fatal("new single call lacks capability", err)
	}
	calls, err = svc.Execute(context.Background(), "old-a", Request{Action: "create", Peer: "old-b", RequestID: old.RequestID})
	if err != nil || calls[0].ServerRecordDelivery {
		t.Fatal("legacy retry enabled backfill", err)
	}
	calls, err = svc.Execute(context.Background(), "owner", Request{Action: "create", GroupID: "g", RequestID: "03838866-2578-414e-8d5c-3e5cd85f8a93"})
	if err != nil || calls[0].ServerRecordDelivery {
		t.Fatal("group incorrectly advertised delivery", err)
	}
}

func TestSameSecondRejoinRetiresPreviousSID(t *testing.T) {
	st := State{Calls: map[string]*Call{}}
	c, _ := st.Create("r", "a", "b", "", 100000)
	st.Action("r", "b", "accept", 100000)
	id := c.MediaIdentity("a")
	c.mediaEvent("participant_joined", id, "old", 100, 100100)
	c.mediaEvent("participant_left", id, "old", 100, 100200)
	c.mediaEvent("participant_joined", id, "new", 100, 100300)
	if c.Members["a"].SID != "new" || c.Members["a"].LeftAt != 0 {
		t.Fatal("valid same-second rejoin rejected")
	}
	c.mediaEvent("participant_joined", id, "old", 101, 101000)
	c.mediaEvent("participant_left", id, "old", 101, 101000)
	if c.Members["a"].SID != "new" || c.Members["a"].LeftAt != 0 {
		t.Fatal("retired SID replay changed current connection")
	}
}

func TestDurationExcludesFinalDisconnectGrace(t *testing.T) {
	st := State{Calls: map[string]*Call{}}
	c, _ := st.Create("r", "a", "b", "", 100000)
	c.ServerRecordDelivery = true
	st.Action("r", "b", "accept", 100000)
	c.mediaEvent("participant_joined", c.MediaIdentity("a"), "a", 100, 100000)
	c.mediaEvent("participant_joined", c.MediaIdentity("b"), "b", 100, 100000)
	c.mediaEvent("participant_left", c.MediaIdentity("a"), "a", 105, 105000)
	st.Sweep(120000)
	if c.DurationSeconds == nil || *c.DurationSeconds != 5 {
		t.Fatalf("trailing grace counted: %v", c.DurationSeconds)
	}
}

func TestLeaseOnlyDisconnectDurationIsUnknown(t *testing.T) {
	st := State{Calls: map[string]*Call{}}
	c, _ := st.Create("r", "a", "b", "", 100000)
	c.ServerRecordDelivery = true
	st.Action("r", "b", "accept", 100000)
	c.mediaEvent("participant_joined", c.MediaIdentity("a"), "a", 100, 100000)
	c.mediaEvent("participant_joined", c.MediaIdentity("b"), "b", 100, 100000)
	st.Sweep(140000)
	if c.DurationSeconds != nil {
		t.Fatal("lease-only disconnect manufactured precise duration")
	}
}
