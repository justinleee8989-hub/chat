package rtcauthority

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"
	"sort"
	"time"
)

type Request struct {
	Room      string `json:"room"`
	Peer      string `json:"peer"`
	GroupID   string `json:"groupID"`
	Action    string `json:"action"`
	RequestID string `json:"requestID"`
}
type StateRepository interface {
	Update(context.Context, func(*State) error) error
}

type Service struct {
	Store                               StateRepository
	Key, Secret, PublicURL, InternalURL string
	GroupMember                         func(context.Context, string, string) error
	UserExists                          func(context.Context, string) error
}

func (s *Service) Execute(ctx context.Context, user string, req Request) ([]*Call, error) {
	if user == "" {
		return nil, ErrPermission
	}
	if req.Action == "inspect" {
		var found *Call
		err := s.Store.Update(ctx, func(st *State) error {
			st.Sweep(time.Now().UnixMilli())
			c := st.Calls[req.Room]
			if c == nil {
				return ErrState
			}
			copy := *c
			found = &copy
			return nil
		})
		if err != nil {
			return nil, err
		}
		if found.GroupID != "" {
			if err = s.GroupMember(ctx, found.GroupID, user); err != nil {
				return nil, err
			}
		} else if !found.Participant(user) {
			return nil, ErrPermission
		}
		return []*Call{{Room: found.Room, GroupID: found.GroupID, Status: found.Status, Expires: found.Expires}}, nil
	}
	if req.Action == "create" {
		if _, err := uuid.Parse(req.RequestID); err != nil {
			return nil, errors.New("缺少有效请求编号")
		}
		if req.GroupID != "" {
			if req.Peer != "" {
				return nil, ErrPermission
			}
			if err := s.GroupMember(ctx, req.GroupID, user); err != nil {
				return nil, err
			}
		} else {
			if err := s.UserExists(ctx, req.Peer); err != nil {
				return nil, err
			}
		}
	}
	// Membership is fetched from OpenIM, never from client-supplied participant lists.
	if req.Action == "join" || req.Action == "heartbeat" {
		var group string
		err := s.Store.Update(ctx, func(st *State) error {
			if c := st.Calls[req.Room]; c != nil {
				group = c.GroupID
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if group != "" {
			if err := s.GroupMember(ctx, group, user); err != nil {
				return nil, err
			}
		}
	}
	var result []*Call
	now := time.Now().UnixMilli()
	room := "imcall-" + uuid.NewString()
	err := s.Store.Update(ctx, func(st *State) error {
		result = nil
		st.Sweep(now)
		if req.Action == "list" {
			for _, c := range st.Calls {
				if c.Participant(user) {
					result = append(result, c)
				}
			}
			sort.Slice(result, func(i, j int) bool { return result[i].Created > result[j].Created })
			if len(result) > 100 {
				result = result[:100]
			}
			return nil
		}
		if req.Action == "create" {
			for _, c := range st.Calls {
				if c.Caller == user && c.RequestID == req.RequestID {
					if c.Callee != req.Peer || c.GroupID != req.GroupID {
						return ErrPermission
					}
					result = []*Call{c}
					return nil
				}
			}
			c, err := st.Create(room, user, req.Peer, req.GroupID, now)
			if err != nil {
				return err
			}
			c.RequestID = req.RequestID
			result = []*Call{c}
			return nil
		}
		c, err := st.Action(req.Room, user, req.Action, now)
		if err != nil {
			return err
		}
		result = []*Call{c}
		return nil
	})
	if err == nil && len(result) == 1 && result[0].Terminal() {
		_ = s.media(ctx, "DeleteRoom", result[0].Room, "")
	}
	if err == nil && len(result) == 1 {
		s.cleanupRemovals(ctx, result[0])
	}
	return result, err
}
func (s *Service) authorizedIdentity(ctx context.Context, room, user string) (string, error) {
	var group, identity string
	err := s.Store.Update(ctx, func(st *State) error {
		st.Sweep(time.Now().UnixMilli())
		c := st.Calls[room]
		if c == nil || !c.Allows(user, time.Now().UnixMilli()) {
			return ErrPermission
		}
		group = c.GroupID
		identity = c.MediaIdentity(user)
		return nil
	})
	if err != nil {
		return "", err
	}
	if group != "" {
		if err = s.GroupMember(ctx, group, user); err != nil {
			return "", err
		}
	}
	return identity, nil
}
func (s *Service) Authorized(ctx context.Context, room, user string) error {
	_, err := s.authorizedIdentity(ctx, room, user)
	return err
}
func (s *Service) AuthorizedMedia(ctx context.Context, room, identity string) error {
	var user string
	err := s.Store.Update(ctx, func(st *State) error {
		c := st.Calls[room]
		if c == nil {
			return ErrPermission
		}
		user = c.UserForIdentity(identity)
		if user == "" {
			return ErrPermission
		}
		return nil
	})
	if err != nil {
		return err
	}
	current, err := s.authorizedIdentity(ctx, room, user)
	if err != nil {
		return err
	}
	if current != identity {
		return ErrPermission
	}
	return nil
}
func (s *Service) Token(ctx context.Context, room, user string) (string, error) {
	identity, err := s.authorizedIdentity(ctx, room, user)
	if err != nil {
		return "", err
	}
	return auth.NewAccessToken(s.Key, s.Secret).SetIdentity(identity).SetName(user).SetValidFor(30 * time.Second).AddGrant(&auth.VideoGrant{RoomJoin: true, Room: room}).ToJWT()
}
