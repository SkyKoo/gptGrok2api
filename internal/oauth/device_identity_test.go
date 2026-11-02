package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestBoundDeviceRejectsWrongOrMissingIdentityBeforeImport(t *testing.T) {
	jwt := func(email string) string {
		raw, _ := json.Marshal(map[string]string{"email": email})
		return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
	}
	for _, tc := range []struct {
		name, accessEmail, idEmail string
		guard                      bool
		success                    bool
	}{
		{"match", "selected@example.test", "", false, true},
		{"id-token-fallback", "", "selected@example.test", false, true},
		{"wrong", "other@example.test", "", false, false},
		{"conflict", "selected@example.test", "other@example.test", false, false},
		{"missing", "", "", false, false},
		{"account-changed", "selected@example.test", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/device" {
					_, _ = w.Write([]byte(`{"device_code":"secret","user_code":"ABCD-EFGH"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": jwt(tc.accessEmail), "id_token": jwt(tc.idEmail), "refresh_token": "refresh"})
			}))
			defer upstream.Close()
			store := NewStore(filepath.Join(t.TempDir(), "oauth.enc"), "test")
			service := NewDeviceService(upstream.Client(), upstream.URL+"/device", upstream.URL+"/token", store)
			start, err := service.StartForAccount(context.Background(), "", "selected@example.test", func() error {
				if tc.guard {
					return errors.New("oauth_account_changed")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Poll(context.Background(), stringValue(start["id"]))
			rows, _ := store.List()
			if tc.success && (err != nil || len(rows) != 1 || rows[0].Email != "selected@example.test") {
				t.Fatalf("expected saved credential: %v", err)
			}
			if !tc.success && (err == nil || len(rows) != 0) {
				t.Fatal("unverified credential persisted")
			}
		})
	}
}

func TestBrowserDeviceRejectsUntrustedURLAndRedirect(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "https://untrusted.invalid/", 307)
	}))
	defer upstream.Close()
	for _, url := range []string{"https://accounts.x.ai.evil.invalid/oauth2/device?user_code=ABCD-EFGH", "http://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH", "https://accounts.x.ai/oauth2/device?user_code=WRNG-CODE", "https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH&user_code=ABCD-EFGH"} {
		_, err := ConfirmDeviceInBrowser(context.Background(), upstream.Client(), upstream.URL, "", BrowserDeviceRequest{VerificationURL: url, UserCode: "ABCD-EFGH", SSO: "secret", Email: "selected@example.test"})
		if err == nil {
			t.Fatal("accepted untrusted authorization URL")
		}
	}
	if calls != 0 {
		t.Fatal("sent credentials before validating destination")
	}
	_, err := ConfirmDeviceInBrowser(context.Background(), upstream.Client(), upstream.URL, "", BrowserDeviceRequest{VerificationURL: "https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH", UserCode: "ABCD-EFGH", SSO: "secret", Email: "selected@example.test"})
	if err == nil || err.Error() != "oauth_worker_rejected" || calls != 1 {
		t.Fatal("worker redirect was followed or not rejected")
	}
}
