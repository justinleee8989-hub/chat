package chat

import "testing"

func TestResolveVerificationModes(t *testing.T) {
	tests := []struct {
		name       string
		phone      string
		mail       string
		legacyMail bool
		superCode  string
		wantPhone  string
		wantMail   string
		wantErr    bool
	}{
		{name: "defaults disabled", wantPhone: "", wantMail: ""},
		{name: "normalizes phone", phone: "  AlI  ", wantPhone: "ali"},
		{name: "normalizes mail", mail: "  MAIL ", wantMail: "mail"},
		{name: "normalizes supercode on both", phone: " SUPERCode ", mail: " SuperCode ", superCode: "x", wantPhone: "supercode", wantMail: "supercode"},
		{name: "legacy mail default", legacyMail: true, wantMail: "mail"},
		{name: "explicit mail disabled overrides legacy", mail: " disabled ", legacyMail: true, wantMail: ""},
		{name: "phone disabled", phone: " disabled ", wantPhone: ""},
		{name: "unknown phone mode", phone: "carrier", wantErr: true},
		{name: "unknown mail mode", mail: "carrier", wantErr: true},
		{name: "phone supercode requires code", phone: "supercode", wantErr: true},
		{name: "mail supercode requires code", mail: "supercode", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPhone, gotMail, err := resolveVerificationModes(tt.phone, tt.mail, tt.legacyMail, tt.superCode)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveVerificationModes() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && (gotPhone != tt.wantPhone || gotMail != tt.wantMail) {
				t.Fatalf("resolveVerificationModes() = (%q, %q), want (%q, %q)", gotPhone, gotMail, tt.wantPhone, tt.wantMail)
			}
		})
	}
}
