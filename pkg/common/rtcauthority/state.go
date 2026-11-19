// Package rtcauthority owns call permissions and lifecycle independently of HTTP and media.
package rtcauthority

import (
	"errors"
	"github.com/google/uuid"
)

const RingMS int64 = 45000
const LeaseMS int64 = 35000
const DisconnectGraceMS int64 = 15000

var ErrPermission = errors.New("call permission denied")
var ErrState = errors.New("call state changed or ended")
var ErrBusy = errors.New("user is in another call")
var ErrCapacity = errors.New("call record capacity reached")
var ErrStoreBusy = errors.New("call state store busy")
var ErrRequestID = errors.New("missing a valid request ID")

type Member struct {
	RetiredSIDs map[string]bool `json:"retiredSIDs,omitempty"`
	Identity    string          `json:"identity"`
	Lease       int64           `json:"lease"`
	LeftAt      int64           `json:"leftAt,omitempty"`
	SID         string          `json:"sid,omitempty"`
	JoinedAt    int64           `json:"joinedAt,omitempty"`
}
type Call struct {
	ConnectedAt          int64              `json:"connectedAt,omitempty"`
	DurationSeconds      *int64             `json:"durationSeconds,omitempty"`
	ServerRecordDelivery bool               `json:"serverRecordDelivery,omitempty"`
	Record               *RecordDelivery    `json:"recordDelivery,omitempty"`
	Room                 string             `json:"room"`
	Caller               string             `json:"caller"`
	Callee               string             `json:"callee,omitempty"`
	GroupID              string             `json:"groupID,omitempty"`
	RequestID            string             `json:"requestID,omitempty"`
	Status               string             `json:"status"`
	Created              int64              `json:"created"`
	Expires              int64              `json:"expires"`
	Ended                int64              `json:"ended,omitempty"`
	Members              map[string]*Member `json:"members"`
	Participants         map[string]bool    `json:"participants,omitempty"`
	Removals             map[string]bool    `json:"removals,omitempty"`
	MediaClosed          bool               `json:"mediaClosed,omitempty"`
}
type State struct {
	Calls map[string]*Call `json:"calls"`
}

func (c *Call) Terminal() bool { return c.Status != "ringing" && c.Status != "accepted" }
func (c *Call) Participant(user string) bool {
	return user == c.Caller || user == c.Callee || c.Members[user] != nil || c.Participants[user]
}
func (c *Call) Allows(user string, now int64) bool {
	m := c.Members[user]
	return c.Status == "accepted" && m != nil && m.Lease > now && (m.LeftAt == 0 || now-m.LeftAt < DisconnectGraceMS)
}
func newMember(now int64) *Member {
	return &Member{Identity: "rtc-" + uuid.NewString(), Lease: now + LeaseMS}
}
func (c *Call) MediaIdentity(user string) string {
	if m := c.Members[user]; m != nil {
		if m.Identity != "" {
			return m.Identity
		}
		return user
	}
	return ""
}
func (c *Call) UserForIdentity(identity string) string {
	for user := range c.Members {
		if c.MediaIdentity(user) == identity {
			return user
		}
	}
	return ""
}
func (c *Call) remove(user string) {
	if identity := c.MediaIdentity(user); identity != "" {
		if c.Removals == nil {
			c.Removals = map[string]bool{}
		}
		c.Removals[identity] = true
	}
	delete(c.Members, user)
}
func (c *Call) end(status string, now int64) {
	if c.Terminal() {
		return
	}
	c.Status = status
	c.Ended = now
	// A terminal leave is observed before disconnect grace elapses. Exclude
	// that trailing wait; successful reconnects have already cleared LeftAt.
	durationEnd := now
	hasLeave := false
	if c.GroupID == "" {
		for _, m := range c.Members {
			if m.LeftAt > 0 {
				hasLeave = true
				if m.LeftAt < durationEnd {
					durationEnd = m.LeftAt
				}
			}
		}
	}
	// A lease is only an authorization deadline, not evidence of when media
	// disconnected. Preserve unknown duration rather than count the lease/grace.
	durationKnown := !(status == "disconnected" && c.ConnectedAt > 0 && !hasLeave)
	if durationKnown && (c.ConnectedAt > 0 || c.ServerRecordDelivery) {
		seconds := int64(0)
		if c.ConnectedAt > 0 && durationEnd > c.ConnectedAt {
			seconds = (durationEnd - c.ConnectedAt) / 1000
		}
		c.DurationSeconds = &seconds
	}

	if c.ServerRecordDelivery && c.GroupID == "" {
		c.Record = &RecordDelivery{EventID: "rtc-terminal:" + c.Room}
	}
}
func (c *Call) migrateParticipants() {
	if c.Participants == nil {
		c.Participants = map[string]bool{}
	}
	if c.Caller != "" {
		c.Participants[c.Caller] = true
	}
	if c.Callee != "" {
		c.Participants[c.Callee] = true
	}
	for user := range c.Members {
		c.Participants[user] = true
	}
}
func (s *State) Busy(user string) bool {
	for _, c := range s.Calls {
		if !c.Terminal() && (c.GroupID == "" && (c.Caller == user || c.Callee == user) || c.Members[user] != nil) {
			return true
		}
	}
	return false
}
func (s *State) Create(room, caller, callee, groupID string, now int64) (*Call, error) {
	s.Sweep(now)
	if caller == "" || (groupID == "" && (callee == "" || caller == callee)) {
		return nil, ErrPermission
	}
	if s.Busy(caller) || (callee != "" && s.Busy(callee)) {
		return nil, ErrBusy
	}
	if len(s.Calls) >= 4096 {
		return nil, ErrCapacity
	}
	participants := map[string]bool{caller: true}
	if callee != "" {
		participants[callee] = true
	}
	c := &Call{Room: room, Caller: caller, Callee: callee, GroupID: groupID, Status: "ringing", Created: now, Expires: now + RingMS, Members: map[string]*Member{}, Participants: participants}
	if groupID != "" {
		c.Status = "accepted"
		c.Expires = now + 15*60*1000
		c.Members[caller] = newMember(now)
	}
	s.Calls[room] = c
	return c, nil
}
func (s *State) Action(room, user, action string, now int64) (*Call, error) {
	s.Sweep(now)
	c := s.Calls[room]
	if c == nil {
		return nil, ErrPermission
	}
	if action == "join" && c.GroupID != "" && !c.Terminal() {
		// The HTTP boundary must first verify current group membership.
		if c.Members[user] != nil {
			return c, nil
		}
		if now >= c.Expires {
			return nil, ErrState
		}
		if s.Busy(user) {
			return nil, ErrBusy
		}
		c.Members[user] = newMember(now)
		c.migrateParticipants()
		return c, nil
	}
	if !c.Participant(user) {
		return nil, ErrPermission
	}
	if action == "get" {
		return c, nil
	}
	if c.Terminal() {
		if action == "end" || action == "cancel" || action == "reject" || action == "leave" {
			return c, nil
		}
		return nil, ErrState
	}
	switch action {
	case "accept":
		if c.GroupID != "" || user != c.Callee {
			return nil, ErrPermission
		}
		if c.Status == "accepted" {
			return c, nil
		}
		c.Status = "accepted"
		c.Members[c.Caller] = newMember(now)
		c.Members[c.Callee] = newMember(now)
		c.migrateParticipants()
	case "cancel":
		if c.Status != "ringing" || user != c.Caller {
			return nil, ErrPermission
		}
		c.end("canceled", now)
	case "reject":
		if c.Status != "ringing" || user != c.Callee {
			return nil, ErrPermission
		}
		c.end("rejected", now)
	case "heartbeat":
		if c.Status != "accepted" || c.Members[user] == nil {
			return nil, ErrState
		}
		c.Members[user].Lease = now + LeaseMS
	case "end", "leave":
		if c.Status != "accepted" {
			return nil, ErrState
		}
		if c.GroupID != "" {
			c.remove(user)
			if len(c.Members) == 0 {
				c.end("ended", now)
			}
		} else {
			c.end("ended", now)
		}
	default:
		return nil, ErrState
	}
	return c, nil
}
func (s *State) Sweep(now int64) {
	if s.Calls == nil {
		s.Calls = map[string]*Call{}
	}
	for room, c := range s.Calls {
		if c == nil {
			delete(s.Calls, room)
			continue
		}
		c.migrateParticipants()
		if c.Terminal() {
			if now-c.Ended > 24*60*60*1000 && c.MediaClosed && (c.Record == nil || c.Record.Delivered) {
				delete(s.Calls, room)
			}
			continue
		}
		if c.Status == "ringing" && now >= c.Expires {
			c.end("missed", now)
			continue
		}
		if c.Status != "accepted" {
			continue
		}
		for user, m := range c.Members {
			if now >= m.Lease || m.LeftAt > 0 && now-m.LeftAt >= DisconnectGraceMS {
				if c.GroupID == "" {
					c.end("disconnected", now)
					break
				}
				c.remove(user)
			}
		}
		if c.GroupID != "" && len(c.Members) == 0 {
			c.end("ended", now)
		}
	}
}
