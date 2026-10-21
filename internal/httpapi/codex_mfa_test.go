package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestCodexTOTPProviderBoundAccount(t *testing.T) {
	s := sessionImportTestServer(t, "http://unused.example.test")
	seed := "JBSWY3DPEHPK3PXP" // public fixture seed, never a real account
	_, _, _, err := s.store.AddAccounts(nil, []map[string]any{
		{"id": "bound", "access_token": "fixture-bound-token", "email": "bound@example.test", "two_factor_secret": seed},
		{"id": "other", "access_token": "fixture-other-token", "email": "other@example.test", "two_factor_secret": "invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := s.accountByRef("bound")
	if err != nil {
		t.Fatal(err)
	}
	s.codexProgress["session"] = &codexOAuthProgress{SessionID: "session", AccountRef: "bound", Stage: "verifying_email_code", Status: "running"}
	generate := s.codexTOTPProvider(bound)
	before, _ := registerruntime.TOTPCode(seed, time.Now())
	code, err := generate("session")
	after, _ := registerruntime.TOTPCode(seed, time.Now())
	if err != nil || (code != before && code != after) {
		t.Fatal("saved seed did not generate current code")
	}
	progress := s.codexStoredProgress("session")
	raw, _ := json.Marshal(codexProgressJSON(progress))
	if progress.Stage != "verifying_2fa" || bytes.Contains(raw, []byte(seed)) || bytes.Contains(raw, []byte(code)) {
		t.Fatal("progress stage/secret redaction incorrect")
	}
	// Read the current stored seed, not a snapshot captured at OAuth start.
	replacement := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if _, _, err := s.store.UpdateAccount("fixture-bound-token", map[string]any{"two_factor_secret": replacement}); err != nil {
		t.Fatal(err)
	}
	before, _ = registerruntime.TOTPCode(replacement, time.Now())
	code, err = generate("session")
	after, _ = registerruntime.TOTPCode(replacement, time.Now())
	if err != nil || (code != before && code != after) {
		t.Fatal("provider used stale seed")
	}
	if _, _, err := s.store.UpdateAccount("fixture-bound-token", map[string]any{"two_factor_secret": "", "two_factor_pending_secret": seed}); err != nil {
		t.Fatal(err)
	}
	code, err = generate("session")
	if err != nil || code != "" {
		t.Fatal("pending seed must not be used for OAuth")
	}
	if _, _, err := s.store.UpdateAccount("fixture-bound-token", map[string]any{"two_factor_secret": "invalid-seed!"}); err != nil {
		t.Fatal(err)
	}
	code, err = generate("session")
	if code != "" || err == nil || strings.Contains(err.Error(), "invalid-seed!") {
		t.Fatal("invalid seed must fail without exposing it")
	}
	if _, _, err := s.store.UpdateAccount("fixture-bound-token", map[string]any{"email": "changed@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = generate("session"); err == nil {
		t.Fatal("changed account identity accepted")
	}
	if s.codexTOTPProvider(nil) != nil {
		t.Fatal("unbound OAuth must not look up a seed by email")
	}
}

func TestCodexEmailContinuationFailureStops(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		err         error
		stop        bool
	}{
		{"MFA parse", "verifying_email_code", fmt.Errorf("wrapped: %w", oauth.ErrCodexMFA), true},
		{"after auto MFA", "verifying_2fa", errors.New("workspace request failed"), true},
		{"email rejection", "verifying_email_code", errors.New("email rejected"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sessionImportTestServer(t, "http://unused.example.test")
			s.codexProgress["session"] = &codexOAuthProgress{SessionID: "session", Stage: tc.stage, Status: "running"}
			if got := s.stopCodexEmailContinuation("session", tc.err); got != tc.stop {
				t.Fatalf("stop=%v want=%v", got, tc.stop)
			}
			progress := s.codexStoredProgress("session")
			if tc.stop && (progress.Stage != "failed" || progress.Status != "failed" || progress.FinishedAt == "") {
				t.Fatal("MFA continuation left running")
			}
		})
	}
}

func TestCodexOAuthRejectsChangingBoundAccount(t *testing.T) {
	for _, path := range []string{"/api/accounts/codex/oauth/email-code", "/api/accounts/codex/oauth/finish"} {
		t.Run(path, func(t *testing.T) {
			s := sessionImportTestServer(t, "http://unused.example.test")
			s.codexProgress["session"] = &codexOAuthProgress{SessionID: "session", AccountRef: "original", Stage: "waiting_email_code", Status: "running"}
			body := `{"session_id":"session","account_ref":"other","value":"123456","callback":"code"}`
			req := httptest.NewRequest("POST", path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer fixture-admin")
			out := httptest.NewRecorder()
			// Call handlers directly so this specifically tests session binding.
			if strings.HasSuffix(path, "/finish") {
				s.codexOAuthFinish(out, req)
			} else {
				s.codexOAuthSubmitEmail(out, req)
			}
			if out.Code != http.StatusBadRequest || !strings.Contains(out.Body.String(), "不能更换绑定账号") {
				t.Fatalf("binding switch accepted: %d %s", out.Code, out.Body.String())
			}
		})
	}
}

func TestCodexOAuthRejectsEmailSeedMismatch(t *testing.T) {
	s := sessionImportTestServer(t, "http://unused.example.test")
	_, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"id": "bound", "access_token": "fixture-bound-token", "email": "bound@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/accounts/codex/oauth/start", strings.NewReader(`{"account_ref":"bound","email_hint":"other@example.test"}`))
	req.Header.Set("Authorization", "Bearer fixture-admin")
	out := httptest.NewRecorder()
	s.codexOAuthStart(out, req)
	if out.Code != http.StatusBadRequest || !strings.Contains(out.Body.String(), "授权邮箱与绑定账号不一致") {
		t.Fatalf("mismatched seed binding accepted: %d %s", out.Code, out.Body.String())
	}
}
