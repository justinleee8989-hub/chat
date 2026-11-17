package messageextras

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ReactionPolicy is immutable after construction. A nil policy uses product defaults.
type ReactionPolicy struct{ allowed map[string]bool }

var defaultReactionPolicy = &ReactionPolicy{allowed: map[string]bool{"👍": true, "❤️": true, "😂": true, "😮": true, "😢": true, "🙏": true, "🔥": true, "🎉": true}}

func NewReactionPolicy(values []string) (*ReactionPolicy, error) {
	if len(values) > 64 {
		return nil, errors.New("too many reaction choices")
	}
	p := &ReactionPolicy{allowed: map[string]bool{}}
	for _, v := range values {
		if strings.TrimSpace(v) == "" || utf8.RuneCountInString(v) > 16 || strings.ContainsAny(v, "<>\r\n\t") || p.allowed[v] {
			return nil, errors.New("invalid reaction choice")
		}
		p.allowed[v] = true
	}
	return p, nil
}
func (p *ReactionPolicy) allows(value string) bool {
	if p == nil {
		p = defaultReactionPolicy
	}
	return p.allowed[value]
}
