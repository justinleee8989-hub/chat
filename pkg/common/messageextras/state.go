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
	Text      *string                    `json:"text,omitempty"`
	EditedAt  int64                      `json:"editedAt,omitempty"`
	Reactions map[string]map[string]bool `json:"reactions"`
	Revision  int64                      `json:"revision"`
}

var ErrPermission = errors.New("无权修改这条消息")
var ErrConflict = errors.New("消息已被更新，请刷新后重试")
var emojis = map[string]bool{"👍": true, "❤️": true, "😂": true, "😮": true, "😢": true, "🙏": true, "🔥": true, "🎉": true}

func (r *Record) Apply(actor, author string, kind int32, req Request, now int64) error {
	if actor == "" || kind >= 1000 {
		return ErrPermission
	}
	switch req.Action {
	case "edit":
		if actor != author || kind != 101 {
			return ErrPermission
		}
		if strings.TrimSpace(req.Text) == "" || utf8.RuneCountInString(req.Text) > 10000 {
			return errors.New("消息文字不能为空或过长")
		}
		if req.Revision != r.Revision {
			return ErrConflict
		}
		r.Text = &req.Text
		r.EditedAt = now
	case "react":
		if !emojis[req.Emoji] {
			return errors.New("不支持的表情")
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
		return errors.New("不支持的操作")
	}
	r.Revision++
	return nil
}
