package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
)

func TestGrokOAuthCandidatesIncludePendingAndNeverExposeCredentials(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	for _, item := range []map[string]any{
		{"id": "pending", "email": "pending@example.test", "sso": "secret-sso", "web_cookies": "secret-cookies", "status": "active"},
		{"id": "authorized", "email": "Known@Example.Test", "status": "active"},
		{"id": "expired", "email": "expired@example.test", "status": "active"},
		{"id": "failed", "email": "failed@example.test", "status": "invalid"},
		{"id": "no-email", "status": "active"},
	} {
		if _, _, e := s.registerStore.AddAccount(item); e != nil {
			t.Fatal(e)
		}
	}
	_, _ = s.registerStore.SetOAuthAuthorization("failed", "failed", "private diagnostic")
	for _, c := range []oauth.Account{
		{Email: "known@example.test", AccessToken: "secret-access", RefreshToken: "secret-refresh", Status: "active", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		{Email: "expired@example.test", AccessToken: "expired-secret", Status: "active", ExpiresAt: time.Now().Add(-time.Hour).Unix()},
		{AccessToken: "unmatched-secret"},
	} {
		if _, e := s.oauthStore.Import(c); e != nil {
			t.Fatal(e)
		}
	}
	if _, _, _, e := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "secret-sso", "status": "异常"}}); e != nil {
		t.Fatal(e)
	}
	denied := httptest.NewRecorder()
	s.Handler().ServeHTTP(denied, httptest.NewRequest("GET", "/api/grok/oauth/candidates", nil))
	if denied.Code != 401 {
		t.Fatal("candidate inventory lacks admin auth")
	}
	w := adminRequest(s.Handler(), "GET", "/api/grok/oauth/candidates", nil)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"secret-sso", "secret-cookies", "secret-access", "secret-refresh", "expired-secret", "unmatched-secret", "private diagnostic"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
	var payload struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"oauth_status"`
			Login  string `json:"login_status"`
		}
	}
	if e := json.Unmarshal(w.Body.Bytes(), &payload); e != nil {
		t.Fatal(e)
	}
	states := map[string]string{}
	for _, v := range payload.Items {
		states[v.ID] = v.Status
		if v.ID == "pending" && v.Login != "异常" {
			t.Fatal("runtime state ignored")
		}
	}
	for id, want := range map[string]string{"pending": "pending", "authorized": "authorized", "expired": "requires_renewal", "failed": "failed", "no-email": "pending"} {
		if states[id] != want {
			t.Fatalf("%s=%s want %s", id, states[id], want)
		}
	}
}

func TestGrokDevicePollAcceptsFrontendSessionID(t *testing.T) {
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"device@example.test"}`)) + ".signature"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/device" {
			_, _ = w.Write([]byte(`{"device_code":"device-secret","user_code":"USER-CODE","verification_uri":"https://accounts.x.ai/oauth2/device"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "refresh_token": "device-refresh"})
	}))
	defer upstream.Close()
	s := New(adminTestConfig(t.TempDir()))
	s.deviceOAuth = oauth.NewDeviceService(upstream.Client(), upstream.URL+"/device", upstream.URL+"/token", s.oauthStore)
	start := adminRequest(s.Handler(), "POST", "/api/grok/oauth/device/start", strings.NewReader(`{}`))
	var session struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal(start.Body.Bytes(), &session); e != nil {
		t.Fatal(e)
	}
	if session.ID == "" {
		t.Fatalf("start: %s", start.Body.String())
	}
	result := adminRequest(s.Handler(), "POST", "/api/grok/oauth/device/poll", strings.NewReader(`{"session_id":"`+session.ID+`"}`))
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"authorized"`) {
		t.Fatalf("poll: %d %s", result.Code, result.Body.String())
	}
	if strings.Contains(result.Body.String(), token) || strings.Contains(result.Body.String(), "device-refresh") {
		t.Fatal("poll exposes credentials")
	}
}
