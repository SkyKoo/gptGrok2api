package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCodexProtocolManualPhoneSMSAnd2FA(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/authorize":
			w.WriteHeader(http.StatusOK)
		case "/api/accounts/authorize/continue":
			_ = json.NewEncoder(w).Encode(map[string]any{"page": map[string]any{"type": "add_phone"}})
		case "/api/accounts/add-phone/send":
			w.WriteHeader(http.StatusNoContent)
		case "/api/accounts/phone-otp/validate":
			_ = json.NewEncoder(w).Encode(map[string]any{"page": map[string]any{"type": "mfa_challenge"}, "factor_id": "factor-1"})
		case "/api/accounts/mfa/issue_challenge":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/api/accounts/mfa/verify":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []any{map[string]any{"id": "workspace-1", "name": "Personal"}}})
		case "/api/accounts/workspace/select":
			_ = json.NewEncoder(w).Encode(map[string]any{"callback_url": "http://localhost:1455/auth/callback?code=code-1&state=" + r.URL.Query().Get("state")})
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "id_token": "id-1", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	standard := NewCodexLogin(server.URL+"/oauth/authorize", server.URL+"/oauth/token", "client", "http://localhost:1455/auth/callback", "openid offline_access", server.Client())
	protocol := NewCodexProtocolLogin(standard)
	ctx := context.Background()
	started, err := protocol.Start(ctx, "person@example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	if started["stage"] != "waiting_phone" {
		t.Fatalf("stage after start = %q", started["stage"])
	}
	id := started["session_id"]
	phone, err := protocol.SubmitPhone(ctx, id, "+8613800138000")
	if err != nil {
		t.Fatal(err)
	}
	if phone["stage"] != "waiting_sms_code" {
		t.Fatalf("stage after phone = %q", phone["stage"])
	}
	twoFA, err := protocol.SubmitSMSCode(ctx, id, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if twoFA["stage"] != "waiting_2fa" {
		t.Fatalf("stage after SMS = %q", twoFA["stage"])
	}
	tokens, err := protocol.Submit2FA(ctx, id, "654321")
	if err != nil {
		t.Fatal(err)
	}
	if tokens["access_token"] != "access-1" || tokens["refresh_token"] != "refresh-1" {
		t.Fatalf("unexpected tokens: %#v", tokens)
	}
}

func TestCodexProtocolManualEmailCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/authorize", "/log-in":
			w.WriteHeader(http.StatusOK)
		case "/api/accounts/authorize/continue":
			_ = json.NewEncoder(w).Encode(map[string]any{"page": map[string]any{"type": "email_verification"}})
		case "/api/accounts/email-otp/validate":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["code"] != "987654" {
				t.Fatalf("unexpected email code: %q", body["code"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []any{map[string]any{"id": "workspace-1", "name": "Personal"}}})
		case "/api/accounts/workspace/select":
			_ = json.NewEncoder(w).Encode(map[string]any{"callback_url": "http://localhost:1455/auth/callback?code=code-1"})
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "id_token": "id-1", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	standard := NewCodexLogin(server.URL+"/oauth/authorize", server.URL+"/oauth/token", "client", "http://localhost:1455/auth/callback", "openid offline_access", server.Client())
	protocol := NewCodexProtocolLogin(standard)
	started, err := protocol.Start(context.Background(), "person@example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	if started["stage"] != "waiting_email_code" {
		t.Fatalf("stage after start = %q", started["stage"])
	}
	tokens, err := protocol.SubmitEmailCode(context.Background(), started["session_id"], "987654")
	if err != nil {
		t.Fatal(err)
	}
	if tokens["access_token"] != "access-1" || tokens["refresh_token"] != "refresh-1" {
		t.Fatalf("unexpected tokens: %#v", tokens)
	}
}

func TestCodexProtocolRejectsPhoneAndCodeInput(t *testing.T) {
	standard := NewCodexLogin("https://auth.example.test/oauth/authorize", "https://auth.example.test/oauth/token", "client", "http://localhost:1455/auth/callback", "openid", nil)
	protocol := NewCodexProtocolLogin(standard)
	if _, err := protocol.SubmitPhone(context.Background(), "missing", "not-a-phone"); err == nil {
		t.Fatal("invalid phone must be rejected before a request")
	}
	if _, err := protocol.SubmitSMSCode(context.Background(), "missing", "bad"); err == nil {
		t.Fatal("invalid code must be rejected before a request")
	}
	if _, err := protocol.SubmitEmailCode(context.Background(), "missing", "bad"); err == nil {
		t.Fatal("invalid email code must be rejected before a request")
	}
}
