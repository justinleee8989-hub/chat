package rtcauthority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/redis/go-redis/v9"
	"time"
)

// A bounded, isolated snapshot makes participant reservations and transitions atomic
// across API replicas. Redis WATCH retries conflicts; no user/chat data is modified.
type Store struct {
	Redis redis.UniversalClient
	Key   string
}

func (s *Store) Update(ctx context.Context, apply func(*State) error) error {
	for attempt := 0; attempt < 16; attempt++ {
		err := s.Redis.Watch(ctx, func(tx *redis.Tx) error {
			data, err := tx.Get(ctx, s.Key).Bytes()
			if err != nil && !errors.Is(err, redis.Nil) {
				return err
			}
			state := State{Calls: map[string]*Call{}}
			if len(data) > 0 {
				if err = json.Unmarshal(data, &state); err != nil {
					return err
				}
			}
			if err = apply(&state); err != nil {
				return err
			}
			previous := data
			data, err = json.Marshal(state)
			if err != nil {
				return err
			}
			ttl := 48 * time.Hour
			for _, c := range state.Calls {
				if c != nil && c.Record != nil && !c.Record.Delivered {
					ttl = 0
					break
				}
			}
			// Read-only updates retain optimistic validation without rewriting the payload.
			if bytes.Equal(previous, data) || (len(previous) == 0 && len(state.Calls) == 0) {
				if len(previous) == 0 || ttl == 0 {
					_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error { pipe.Get(ctx, s.Key); return nil })
					if errors.Is(err, redis.Nil) {
						return nil
					}
					return err
				}
				_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error { pipe.Expire(ctx, s.Key, ttl); return nil })
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error { pipe.Set(ctx, s.Key, data, ttl); return nil })
			return err
		}, s.Key)
		if !errors.Is(err, redis.TxFailedErr) {
			return err
		}
	}
	return ErrStoreBusy
}

// View returns a detached snapshot; it never renews a lease or writes Redis.
// It is only for lookup paths. Reservations/transitions continue through Update.
func (s *Store) View(ctx context.Context, read func(*State) error) error {
	data, err := s.Redis.Get(ctx, s.Key).Bytes()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	state := State{Calls: map[string]*Call{}}
	if len(data) > 0 {
		if err = json.Unmarshal(data, &state); err != nil {
			return err
		}
	}
	return read(&state)
}
