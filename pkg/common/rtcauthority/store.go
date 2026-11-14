package rtcauthority

import (
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
			data, err = json.Marshal(state)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error { pipe.Set(ctx, s.Key, data, 48*time.Hour); return nil })
			return err
		}, s.Key)
		if !errors.Is(err, redis.TxFailedErr) {
			return err
		}
	}
	return errors.New("通话状态繁忙，请重试")
}
