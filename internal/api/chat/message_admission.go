package chat

import (
	"context"
	"sync"
)

// Limits are per API process. Deployment replica counts must be included in capacity planning.
type requestAdmission struct {
	mu     sync.Mutex
	active map[string]bool
	limit  int
}

func newRequestAdmission(limit int) *requestAdmission {
	return &requestAdmission{active: map[string]bool{}, limit: limit}
}
func (g *requestAdmission) enter(actor string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active[actor] || len(g.active) >= g.limit {
		return false
	}
	g.active[actor] = true
	return true
}
func (g *requestAdmission) leave(actor string) { g.mu.Lock(); delete(g.active, actor); g.mu.Unlock() }

var readersAdmission = newRequestAdmission(4)
var extrasSlots = make(chan struct{}, 8)
var readerProgressSlots = make(chan struct{}, 8)

func acquireProgressSlot(ctx context.Context, slots chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
