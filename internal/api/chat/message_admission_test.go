package chat

import (
	"context"
	"testing"
	"time"
)

func TestReaderAdmission(t *testing.T) {
	g := newRequestAdmission(2)
	if !g.enter("a") || g.enter("a") || !g.enter("b") || g.enter("c") {
		t.Fatal("admission bound failed")
	}
	g.leave("a")
	if !g.enter("c") {
		t.Fatal("slot leaked")
	}
	g.leave("b")
	g.leave("c")
}
func TestReaderProgressCancellation(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if acquireProgressSlot(ctx, slots) == nil {
		t.Fatal("canceled admitted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancel blocked")
	}
}
