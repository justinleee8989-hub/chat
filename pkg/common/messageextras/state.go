package messageextras

import (
	"errors"
	"strings"
	"unicode/utf8"
)

type Request struct {
	Action         string   `json:"action"`
	ConversationID string   `json:"conversationID"`
	MessageID      string   `json:"messageID"`
	MessageIDs     []string `json:"messageIDs"`
	Seq            int64    `json:"seq"`
	Text           string   `json:"text"`
	Emoji          string   `json:"emoji"`
	Enabled        bool     `json:"enabled"`
	Revision       int64    `json:"revision"`
}
type Record struct {
	Seq       int64                      `json:"seq,omitempty"`
	Text      *string                    `json:"text,omitempty"`
	EditedAt  int64                      `json:"editedAt,omitempty"`
	Reactions map[string]map[string]bool `json:"reactions"`
	Revision  int64                      `json:"revision"`
}

var ErrPermission = errors.New("no permission to change this message")
var ErrConflict = errors.New("message changed since it was read")

// ErrInvalid: an edit or reaction the server does not accept (empty or too long text, an emoji or action it does not know).
var ErrInvalid = errors.New("invalid message edit or reaction")

func (r *Record) Apply(actor, author string, kind int32, req Request, now int64) error {
	return r.ApplyWithPolicy(actor, author, kind, req, now, nil)
}

func (r *Record) ApplyWithPolicy(actor, author string, kind int32, req Request, now int64, policy *ReactionPolicy) error {
	if actor == "" || kind >= 1000 {
		return ErrPermission
	}
	switch req.Action {
	case "edit":
		if actor != author || kind != 101 {
			return ErrPermission
		}
		if strings.TrimSpace(req.Text) == "" || utf8.RuneCountInString(req.Text) > 10000 {
			return ErrInvalid
		}
		if req.Revision != r.Revision {
			return ErrConflict
		}
		r.Text = &req.Text
		r.EditedAt = now
	case "react":
		if !policy.allows(req.Emoji) && (req.Enabled || !r.Reactions[req.Emoji][actor]) {
			return ErrInvalid
		}
		if r.Reactions == nil {
			r.Reactions = map[string]map[string]bool{}
		}
		if r.Reactions[req.Emoji] == nil {
			r.Reactions[req.Emoji] = map[string]bool{}
		}
		if req.Enabled {
			r.Reactions[req.Emoji][actor] = true
		} else {
			delete(r.Reactions[req.Emoji], actor)
		}
		if len(r.Reactions[req.Emoji]) == 0 {
			delete(r.Reactions, req.Emoji)
		}
	default:
		return ErrInvalid
	}
	r.Revision++
	return nil
}
