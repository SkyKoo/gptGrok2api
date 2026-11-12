package register

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdobeForwardedSenderFormats(t *testing.T) {
	for _, sender := range []string{
		"Adobe <noreply_at_adobe_com_a1b2c3d4e5f6g7_h8i9j0k1@icloud.com>",
		"noreply_at_mail_adobe_com_sample-suffix@icloud.com",
		"account-noreply_at_adobe_com_a1b2c3d4e5f6g7_h8i9j0k1@icloud.com",
		"NOREPLY_AT_ADOBE_COM_A1B2C3D4E5F6G7_H8I9J0K1@ICLOUD.COM",
	} {
		if !isAdobeSender(sender) {
			t.Errorf("rejected supported Adobe sender %q", sender)
		}
	}
	for _, sender := range []string{
		"noreply_at_adobe_com_sample_suffix@icloud.com.attacker.test",
		"noreply_at_adobe_com_sample_suffix@attacker.test",
		"noreply_at_adobee_com_sample_suffix@icloud.com",
		"noreply_at_adobe_com_@icloud.com",
		"someone_at_adobe_com_sample_suffix@icloud.com",
		"Adobe <someone@icloud.com>",
		"noreply_at_openai_com_sample_suffix@icloud.com",
		"noreply_at_adobe_com_attacker_com_opaque123@icloud.com",
		"noreply_at_adobe_com_attacker_opaque123@icloud.com",
		"noreply_at_adobe_com_a1b2c3d4e5f6g7_h8i9j0k1_extra@icloud.com",
	} {
		if isAdobeSender(sender) {
			t.Errorf("accepted unrelated sender %q", sender)
		}
	}
}

func TestAdobeMailCodeSelectsExplicitLabel(t *testing.T) {
	for _, tt := range []struct{ name, text, want string }{
		{"label_with_footer", "Your verification code is:\n123456\nReference 654321", "123456"},
		{"footer_before_label", "Reference 654321\nYour verification code is: 123456", "123456"},
		{"capitalization", "VERIFICATION CODE: 123456. Reference 654321", "123456"},
		{"security", "Security code is: 123456. Reference 654321", "123456"},
		{"same_label_repeated", "Verification code: 123456\nVerification code: 123456", "123456"},
		{"conflicting_labels", "Verification code: 123456\nVerification code: 654321", ""},
		{"plain_unique", "Your code is 123456.", "123456"},
		{"plain_ambiguous", "123456 or 654321", ""},
		{"long_number", "Verification code: 1234567", ""},
		{"no_code", "Your verification code will arrive soon.", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := adobeMailCode(tt.text); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
	if mailCode("Your verification code is:\n123456\nReference 654321") != "" {
		t.Fatal("changed generic GPT/Grok ambiguous-code behavior")
	}
}

func TestAdobeHMEReadThroughCoordinatorUsesFreshTrustedLabeledCode(t *testing.T) {
	for _, detail := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "detail"}[detail], func(t *testing.T) {
			listed, read := 0, 0
			at := time.Now().UTC()
			box := Mailbox{Email: "adobe@example.test", AccountID: "test"}
			valid := hmeMessage{ID: "valid", Folder: "junk", From: "noreply_at_adobe_com_a1b2c3d4e5f6g7_h8i9j0k1@icloud.com", To: box.Email, Date: at.Format(time.RFC3339)}
			body := "Your verification code is:\n123456\nReference 654321"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respond := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data}) }
				switch r.URL.Path {
				case "/api/inbox":
					listed++
					if r.Method != http.MethodGet || r.URL.Query().Get("alias") != box.Email || r.URL.Query().Get("folder") != "all" {
						t.Error("lost mailbox or read-only boundary")
					}
					old, other, foreign, target := valid, valid, valid, valid
					old.ID, old.Date, old.Subject = "old", at.Add(-time.Hour).Format(time.RFC3339), "111111"
					other.ID, other.To, other.Subject = "other", "other@example.test", "222222"
					foreign.ID, foreign.From, foreign.Subject = "foreign", "noreply_at_openai_com_sample_suffix@icloud.com", "333333"
					if !detail {
						target.Preview = body
					}
					respond(map[string]any{"method": "imap", "messages": []hmeMessage{old, other, foreign, target}})
				case "/api/inbox/valid":
					read++
					if r.Method != http.MethodGet || r.URL.Query().Get("method") != "imap" || r.URL.Query().Get("folder") != "junk" {
						t.Error("lost detail source")
					}
					full := valid
					full.Body = body
					respond(full)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			h, err := NewHME(HMEConfig{BaseURL: server.URL, Password: "test", AccountID: box.AccountID}, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			coordinated := NewCoordinatedMailSource("adobe", "test", h, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := coordinated.WaitAdobeCode(ctx, box, at.Add(-time.Second))
			if err != nil || got != "123456" {
				t.Fatalf("code result %q, error %v", got, err)
			}
			if listed != 1 || read != map[bool]int{false: 0, true: 1}[detail] {
				t.Fatalf("requests listed=%d, read=%d", listed, read)
			}
			if !h.consumed[box.Email+"|junk|valid"] {
				t.Fatal("message not consumed")
			}
		})
	}
}
