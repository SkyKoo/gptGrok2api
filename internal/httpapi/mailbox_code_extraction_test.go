package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestMailboxResourceAdobeExtractionDoesNotChangeGPTOrGrok(t *testing.T) {
	for _, platform := range []string{"openai", "grok"} {
		for _, platformAware := range []bool{false, true} {
			name := platform + "/generic"
			if platformAware {
				name = platform + "/platform"
			}
			t.Run(name, func(t *testing.T) {
				const email = "fixture@example.test"
				sender := map[string]string{"openai": "noreply@openai.com", "grok": "noreply@x.ai"}[platform]
				var reads atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/api/inbox" || r.URL.Query().Get("alias") != email {
						t.Error("unexpected HME request")
						http.NotFound(w, r)
						return
					}
					reads.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"method": "imap", "messages": []map[string]string{{
						"from": sender, "to": email, "date": time.Now().Add(time.Second).UTC().Format(time.RFC3339),
						"preview": "Verification code: 123456. Reference 654321.",
					}}}})
				}))
				defer upstream.Close()
				manager := mailboxstore.NewResourceManager(nil, newMailboxProvider)
				manager.SetSourceLoader(func(context.Context) ([]mailboxstore.Source, error) {
					return []mailboxstore.Source{{ID: "fixture", Type: "icloud_hme", Enabled: true, Config: map[string]any{"api_base": upstream.URL, "admin_password": "fixture", "account_id": "fixture"}}}, nil
				})
				source, err := newMailboxResourceSource(context.Background(), manager, platform, "fixture", time.Millisecond)
				if err != nil {
					t.Fatal(err)
				}
				defer source.Close()
				box := registerruntime.Mailbox{Email: email, AccountID: "fixture", SourceID: "fixture"}
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				matcher := func(got string) bool { return got == sender }
				var code string
				if platformAware {
					code, err = source.session.WaitCodeForPlatform(ctx, mailboxFromRegister(box), time.Now(), platform, matcher)
				} else {
					code, err = source.WaitCodeFor(ctx, box, time.Now(), matcher)
				}
				if code != "" || !errors.Is(err, context.DeadlineExceeded) || reads.Load() == 0 {
					t.Fatalf("generic ambiguous-code behavior changed: code_present=%v error=%v reads=%d", code != "", err, reads.Load())
				}
			})
		}
	}
}
