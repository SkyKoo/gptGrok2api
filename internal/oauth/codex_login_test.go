package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCodexLoginStartFinishUsesPKCEAndRequiresRefreshToken(t *testing.T) {
	var received url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		received = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"id_token":      "id-token",
			"token_type":    "Bearer",
			"expires_in":    "3600",
		})
	}))
	defer upstream.Close()

	login := NewCodexLogin("https://auth.example.test/authorize", upstream.URL+"/token", "codex-client", "http://localhost:1455/auth/callback", "openid offline_access", upstream.Client())
	started, err := login.Start("person@example.test")
	if err != nil {
		t.Fatal(err)
	}
	authorize, err := url.Parse(started["authorize_url"])
	if err != nil {
		t.Fatal(err)
	}
	query := authorize.Query()
	for _, key := range []string{"client_id", "state", "code_challenge", "code_challenge_method", "redirect_uri", "scope"} {
		if strings.TrimSpace(query.Get(key)) == "" {
			t.Fatalf("authorize URL missing %s: %s", key, authorize.String())
		}
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("login_hint") != "person@example.test" {
		t.Fatalf("unexpected authorize query: %s", authorize.RawQuery)
	}
	callback := "http://localhost:1455/auth/callback?code=one-time-code&state=" + url.QueryEscape(query.Get("state"))
	tokens, err := login.Finish(context.Background(), started["session_id"], callback)
	if err != nil {
		t.Fatal(err)
	}
	if tokens["access_token"] != "access-token" || tokens["refresh_token"] != "refresh-token" || received.Get("code_verifier") == "" {
		t.Fatalf("unexpected token exchange: tokens=%#v form=%#v", tokens, received)
	}
	if _, err := login.Finish(context.Background(), started["session_id"], "one-time-code"); err == nil {
		t.Fatal("expected consumed OAuth session to be rejected")
	}
}

func TestCodexLoginFinishRejectsOAuthErrorAndStateMismatch(t *testing.T) {
	login := NewCodexLogin("https://auth.example.test/authorize", "https://token.example.test", "codex-client", "http://localhost:1455/auth/callback", "openid", http.DefaultClient)
	started, err := login.Start("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := login.Finish(context.Background(), started["session_id"], "http://localhost:1455/auth/callback?error=access_denied&error_description=cancelled"); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected OAuth denial, got %v", err)
	}
	if _, err := login.Finish(context.Background(), started["session_id"], "http://localhost:1455/auth/callback?code=code&state=wrong"); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("expected state mismatch, got %v", err)
	}
}
