package chat

import (
	"sync"
	"time"
)

type sequenceHintKey struct{ server, conversation, message string }
type sequenceHint struct {
	seq     int64
	expires time.Time
}
type sequenceHints struct {
	mu    sync.Mutex
	items map[sequenceHintKey]sequenceHint
}

var extraSequenceHints = sequenceHints{items: map[sequenceHintKey]sequenceHint{}}

func (h *sequenceHints) get(server, conversation, id string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := sequenceHintKey{server, conversation, id}
	v := h.items[key]
	if time.Now().After(v.expires) {
		delete(h.items, key)
		return 0
	}
	return v.seq
}
func (h *sequenceHints) put(server, conversation, id string, seq int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := sequenceHintKey{server, conversation, id}
	if _, ok := h.items[key]; !ok && len(h.items) >= 4096 {
		for k := range h.items {
			delete(h.items, k)
			break
		}
	}
	h.items[key] = sequenceHint{seq, time.Now().Add(time.Hour)}
}

// Legacy scan cursors contain no content or authorization. They only prevent
// restarting an expensive old-record lookup on every background poll.
type legacyScanKey struct {
	server, actor, conversation, message string
	lower                                int64
}
type legacyScan struct {
	next    int64
	expires time.Time
}
type legacyScans struct {
	mu    sync.Mutex
	items map[legacyScanKey]legacyScan
}

var extraLegacyScans = legacyScans{items: map[legacyScanKey]legacyScan{}}

func (s *legacyScans) get(key legacyScanKey, upper int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[key]
	if !ok || time.Now().After(v.expires) {
		delete(s.items, key)
		return upper
	}
	return min(v.next, upper)
}
func (s *legacyScans) put(key legacyScanKey, next int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[key]; !ok && len(s.items) >= 4096 {
		for k := range s.items {
			delete(s.items, k)
			break
		}
	}
	ttl := time.Minute
	if next < key.lower {
		ttl = 5 * time.Second
	}
	s.items[key] = legacyScan{next, time.Now().Add(ttl)}
}
