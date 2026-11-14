package chat

import (
	"fmt"
	"strings"
)

// resolveVerificationModes preserves the old mail.enable setting only when use is omitted.
func resolveVerificationModes(phone, mail string, legacyMail bool, superCode string) (string, string, error) {
	phone = strings.ToLower(strings.TrimSpace(phone))
	mail = strings.ToLower(strings.TrimSpace(mail))
	if mail == "" && legacyMail {
		mail = "mail"
	}
	if phone == "disabled" {
		phone = ""
	}
	if mail == "disabled" {
		mail = ""
	}
	switch phone {
	case "", "ali", "supercode":
	default:
		return "", "", fmt.Errorf("unsupported phone verification mode")
	}
	switch mail {
	case "", "mail", "supercode":
	default:
		return "", "", fmt.Errorf("unsupported email verification mode")
	}
	if (phone == "supercode" || mail == "supercode") && strings.TrimSpace(superCode) == "" {
		return "", "", fmt.Errorf("superCode must be configured when fixed-code verification is enabled")
	}
	return phone, mail, nil
}
