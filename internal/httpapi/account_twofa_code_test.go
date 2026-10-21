package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestAccountTwoFACode(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir, cfg.DataDir = root, root
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "keys.json")
	s := New(cfg)
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	_, _, rows, err := s.store.AddAccounts(nil, []map[string]any{
		{"access_token": "configured-token", "email": "configured@example.invalid", "login_password": "private-password", "two_factor_secret": secret},
		{"access_token": "legacy-token", "totp_secret": secret},
		{"access_token": "pending-token", "twofa_pending": map[string]any{"secret": secret, "session_id": "private-enrollment"}},
		{"access_token": "invalid-token", "two_factor_secret": "bad-secret!"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := accountPublicRef(rows[0])
	for _, tc := range []struct {
		name, method, token, ref string
		status                   int
	}{
		{"configured", "POST", "admin-secret", ref, 200},
		{"legacy", "POST", "admin-secret", "legacy-token", 200},
		{"pending", "POST", "admin-secret", "pending-token", 409},
		{"invalid", "POST", "admin-secret", "invalid-token", 422},
		{"missing", "POST", "admin-secret", "", 400},
		{"unknown", "POST", "admin-secret", "absent-account", 404},
		{"method", "GET", "admin-secret", ref, 405},
		{"anonymous", "POST", "", ref, 401},
		{"api_only", "POST", "api-secret", ref, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"account_ref": tc.ref})
			req := httptest.NewRequest(tc.method, "/api/accounts/twofa/code", strings.NewReader(string(body)))
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			res := httptest.NewRecorder()
			s.Handler().ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("got %d: %s", res.Code, res.Body.String())
			}
			if res.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("response is cacheable")
			}
			for _, private := range []string{secret, "configured-token", "legacy-token", "pending-token", "invalid-token", "private-password", "private-enrollment"} {
				if strings.Contains(res.Body.String(), private) {
					t.Fatal("credential leaked")
				}
			}
			if tc.status != 200 {
				return
			}
			var reply struct {
				Code       string `json:"code"`
				ServerTime int64  `json:"server_time"`
				ExpiresAt  int64  `json:"expires_at"`
			}
			if err := json.Unmarshal(res.Body.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
			expected, _ := registerruntime.TOTPCode(secret, time.UnixMilli(reply.ServerTime))
			if reply.Code != expected || len(reply.Code) != 6 {
				t.Fatal("incorrect TOTP")
			}
			if reply.ExpiresAt != (reply.ServerTime/30000+1)*30000 {
				t.Fatal("incorrect expiry window")
			}
		})
	}
}
