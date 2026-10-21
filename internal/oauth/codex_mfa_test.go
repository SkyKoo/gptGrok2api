package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserprofile"
)

func TestCodexMFAFactorShapes(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		required        bool
	}{
		{"root factor", `{"page_type":"mfa_challenge","factor_id":"factor-1"}`, "factor-1", true},
		{"root id", `{"page":{"type":"mfa"},"id":"factor-2"}`, "factor-2", true},
		{"payload id", `{"page":{"type":"mfa_challenge","payload":{"id":"factor-3"}}}`, "factor-3", true},
		{"payload factor", `{"page":{"type":"totp","payload":{"factor_id":"factor-4"}}}`, "factor-4", true},
		{"url", `{"continue_url":"/mfa-challenge/factor-5?source=email#verify"}`, "factor-5", true},
		{"nested url", `{"page":{"url":"https://auth.example.test/mfa-challenge/factor-6/"}}`, "factor-6", true},
		{"malformed factor", `{"page_type":"mfa","id":"bad\u0000factor"}`, "", true},
		{"missing factor", `{"page_type":"mfa"}`, "", true},
		{"metadata only", `{"page_type":"add_phone","metadata":{"method":"totp","factor_id":"old"}}`, "", false},
		{"metadata url", `{"page_type":"consent","metadata":"https://auth.example.test/mfa-challenge/old"}`, "", false},
		{"unrelated route", `{"continue_url":"/something/mfa-challenge/old"}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &payload); err != nil {
				t.Fatal(err)
			}
			if got := factorID(payload); got != tc.want {
				t.Errorf("factor=%q want=%q", got, tc.want)
			}
			if got := codexMFARequired(payload); got != tc.required {
				t.Errorf("MFA required=%v want=%v", got, tc.required)
			}
		})
	}
}

func mfaTestSession(t *testing.T, handler http.HandlerFunc) (*CodexProtocolLogin, *codexProtocolSession) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	login := NewCodexProtocolLogin(NewCodexLogin(upstream.URL+"/authorize", upstream.URL+"/token", "client", "http://localhost:1455/auth/callback", "openid", upstream.Client()))
	profile := browserprofile.New()
	client, err := newProtocolClient(profile)
	if err != nil {
		t.Fatal(err)
	}
	session := &codexProtocolSession{ID: "mfa-test", AuthBase: upstream.URL, Client: client, Profile: profile, CreatedAt: time.Now(), Stage: "waiting_2fa"}
	login.sessions[session.ID] = session
	return login, session
}

func TestCodexMFAManualFallbackAndMetadata(t *testing.T) {
	login, session := mfaTestSession(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected upstream request")
		w.WriteHeader(500)
	})
	challenge := map[string]any{"page": map[string]any{"type": "mfa_challenge", "payload": map[string]any{"id": "saved-factor"}}}
	session.TOTPCode = func(string) (string, error) { return "", nil }
	result, err := login.advanceResult(context.Background(), session, challenge)
	if err != nil || result["stage"] != "waiting_2fa" {
		t.Fatalf("manual fallback=%v err=%v", result, err)
	}
	if session.MFAAttempted {
		t.Fatal("missing seed consumed verification attempt")
	}
	result, err = login.advanceResult(context.Background(), session, map[string]any{"page_type": "add_phone", "metadata": map[string]any{"method": "totp"}})
	if err != nil || result["stage"] != "waiting_phone" || session.FactorID != "saved-factor" {
		t.Fatalf("metadata re-entered MFA: %v %v", result, err)
	}
}

func TestCodexMFABadSeedMissingFactorAndForeignURL(t *testing.T) {
	for _, mode := range []string{"invalid seed", "missing factor", "foreign url"} {
		t.Run(mode, func(t *testing.T) {
			login, session := mfaTestSession(t, func(w http.ResponseWriter, r *http.Request) {
				t.Error("invalid challenge must stop before any upstream request")
				w.WriteHeader(500)
			})
			challenge := map[string]any{"page_type": "mfa", "id": "factor-1"}
			session.TOTPCode = func(string) (string, error) { return "", errors.New("seed must stay private") }
			if mode == "missing factor" {
				delete(challenge, "id")
			}
			if mode == "foreign url" {
				challenge["continue_url"] = "https://foreign.example.test/mfa-challenge/factor-1"
			}
			_, err := login.advanceResult(context.Background(), session, challenge)
			if !errors.Is(err, ErrCodexMFA) {
				t.Fatalf("expected safe MFA error, got %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "seed must stay private") {
				t.Fatal("provider error leaked")
			}
		})
	}
}

func TestCodexMFAStopsRepeatedOrRejectedChallenge(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{true: "upstream rejection", false: "repeated challenge"}[reject], func(t *testing.T) {
			var mu sync.Mutex
			challengeCalls, verifyCalls := 0, 0
			login, session := mfaTestSession(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/api/accounts/mfa/issue_challenge":
					challengeCalls++
					w.Write([]byte(`{}`))
				case "/api/accounts/mfa/verify":
					verifyCalls++
					if reject {
						w.WriteHeader(400)
						w.Write([]byte(`{"error":"private-code-654321"}`))
					} else {
						w.Write([]byte(`{"page_type":"mfa","id":"factor-1"}`))
					}
				default:
					t.Errorf("unexpected auth endpoint: %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})
			session.FactorID = "factor-1"
			for i := 0; i < 2; i++ {
				_, err := login.Submit2FA(context.Background(), session.ID, "654321")
				if !errors.Is(err, ErrCodexMFA) {
					t.Fatalf("expected MFA failure, got %v", err)
				}
				if strings.Contains(err.Error(), "654321") {
					t.Fatal("OTP leaked into error")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if challengeCalls != 1 || verifyCalls != 1 {
				t.Fatalf("retried MFA: challenge=%d verify=%d", challengeCalls, verifyCalls)
			}
		})
	}
}

func TestCodexMFAConcurrentManualSubmissions(t *testing.T) {
	var mu sync.Mutex
	verifications := 0
	login, session := mfaTestSession(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/accounts/mfa/verify" {
			verifications++
		}
		if r.URL.Path != "/api/accounts/mfa/verify" && r.URL.Path != "/api/accounts/mfa/issue_challenge" {
			t.Error("unexpected endpoint")
		}
		w.Write([]byte(`{"page_type":"add_phone"}`))
	})
	session.FactorID = "factor-1"
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); login.Submit2FA(context.Background(), session.ID, "654321") }()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if verifications != 1 {
		t.Fatalf("verification submitted %d times", verifications)
	}
}
