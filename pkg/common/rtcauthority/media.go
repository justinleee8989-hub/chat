package rtcauthority

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/webhook"
	"net/http"
	"time"
)

func (s *Service) media(ctx context.Context, method, room, user string) error {
	token, err := auth.NewAccessToken(s.Key, s.Secret).SetValidFor(time.Minute).AddGrant(&auth.VideoGrant{Room: room, RoomAdmin: true, RoomCreate: true}).ToJWT()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"room": room, "identity": user})
	req, err := http.NewRequestWithContext(ctx, "POST", s.InternalURL+"/twirp/livekit.RoomService/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 404 {
		return fmt.Errorf("media cleanup status %d", resp.StatusCode)
	}
	return nil
}
func (s *Service) Webhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	event, err := webhook.ReceiveWebhookEvent(r, auth.NewSimpleKeyProvider(s.Key, s.Secret))
	if err != nil {
		http.Error(w, "invalid signature", 401)
		return
	}
	if event.Room == nil || event.Participant == nil {
		w.WriteHeader(200)
		return
	}
	room, identity := event.Room.Name, event.Participant.Identity
	if event.Event == "participant_joined" {
		if err = s.AuthorizedMedia(r.Context(), room, identity); err != nil {
			if !errors.Is(err, ErrPermission) {
				http.Error(w, "authority unavailable", 503)
				return
			}
			if err = s.media(r.Context(), "RemoveParticipant", room, identity); err != nil {
				http.Error(w, "cleanup retry", 503)
				return
			}
			w.WriteHeader(200)
			return
		}
	}
	if event.Event == "participant_joined" || event.Event == "participant_left" {
		err = s.Store.Update(r.Context(), func(st *State) error {
			c := st.Calls[room]
			if c == nil || c.Terminal() {
				return nil
			}
			now := time.Now().UnixMilli()
			st.Sweep(now)
			c.mediaEvent(event.Event, identity, event.Participant.Sid, event.CreatedAt, now)
			return nil
		})
		if err != nil {
			http.Error(w, "state unavailable", 503)
			return
		}
	}
	w.WriteHeader(200)
}
func (s *Service) Run(ctx context.Context) {
	go s.runRecords(ctx)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var last []*Call
	lastOK := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UnixMilli()
			var calls []*Call
			err := s.Store.Update(ctx, func(st *State) error {
				st.Sweep(now)
				calls = nil
				for _, c := range st.Calls {
					calls = append(calls, c)
				}
				return nil
			})
			if err != nil {
				if time.Since(lastOK) > 35*time.Second {
					for _, c := range last {
						if !c.Terminal() {
							_ = s.media(ctx, "DeleteRoom", c.Room, "")
						}
					}
				}
				continue
			}
			lastOK = time.Now()
			last = calls
			for _, c := range calls {
				if c.Terminal() {
					if now-c.Ended < 120000 || !c.MediaClosed {
						if s.media(ctx, "DeleteRoom", c.Room, "") == nil {
							_ = s.Store.Update(ctx, func(st *State) error {
								if record := st.Calls[c.Room]; record != nil && record.Terminal() {
									record.MediaClosed = true
								}
								return nil
							})
						}
					}
					continue
				}
				s.cleanupRemovals(ctx, c)
				if c.GroupID != "" {
					for user := range c.Members {
						if errors.Is(s.GroupMember(ctx, c.GroupID, user), ErrPermission) {
							_ = s.Store.Update(ctx, func(st *State) error { _, err := st.Action(c.Room, user, "leave", time.Now().UnixMilli()); return err })
						}
					}
				}
			}
		}
	}
}

// Each queued identity belongs to one admission. Concurrent retries cannot target a rejoin.
func (s *Service) cleanupRemovals(ctx context.Context, c *Call) {
	for identity := range c.Removals {
		if s.media(ctx, "RemoveParticipant", c.Room, identity) == nil {
			_ = s.Store.Update(ctx, func(st *State) error {
				if current := st.Calls[c.Room]; current != nil {
					delete(current.Removals, identity)
				}
				return nil
			})
		}
	}
}
