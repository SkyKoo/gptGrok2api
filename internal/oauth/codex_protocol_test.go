package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCodexProtocolManualPhoneSMSAnd2FA(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the Sentinel runner test")
	}
	runner, err := filepath.Abs("../register/sentinel-runner.js")
	if err != nil {
		t.Fatal(err)
	}
	sdk := filepath.Join(t.TempDir(), "probe.js")
	if err := os.WriteFile(sdk, []byte(`var SentinelSDK = {
 token: async () => JSON.stringify({p:"probe",c:"challenge-token"}),
 sessionObserverToken: async () => "probe-so"
};`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CFM_SENTINEL_NODE", node)
	t.Setenv("CFM_SENTINEL_RUNNER", runner)
	t.Setenv("CFM_SENTINEL_SDK", sdk)
	phoneSends, smsValidations := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/authorize":
			w.WriteHeader(http.StatusOK)
		case "/log-in", "/backend-api/sentinel/frame.html":
			w.WriteHeader(http.StatusOK)
		case "/backend-api/sentinel/req":
			http.SetCookie(w, &http.Cookie{Name: "oai-sc", Value: "challenge-session", Path: "/"})
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "challenge-token", "so": map[string]any{"required": false}})
		case "/api/accounts/authorize/continue":
			_ = json.NewEncoder(w).Encode(map[string]any{"page": map[string]any{"type": "add_phone"}})
		case "/api/accounts/add-phone/send":
			phoneSends++
			if phoneSends == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "phone_number_already_used", "message": "phone number already used"}})
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		case "/api/accounts/phone-otp/validate":
			smsValidations++
			_ = json.NewEncoder(w).Encode(map[string]any{"page": map[string]any{"type": "mfa_challenge"}, "factor_id": "factor-1", "metadata": "phone_otp"})
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
	protocol.sentinelOrigin = server.URL
	protocol.sentinelVersion = "test-version"
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
	if phone["stage"] != "waiting_phone_retry" {
		t.Fatalf("stage after rejected phone = %q", phone["stage"])
	}
	phone, err = protocol.SubmitPhone(ctx, id, "+8613800138001")
	if err != nil {
		t.Fatal(err)
	}
	if phone["stage"] != "waiting_sms_code" {
		t.Fatalf("stage after replacement phone = %q", phone["stage"])
	}
	if _, err := protocol.SubmitPhone(ctx, id, "+8613800138001"); err == nil || !strings.Contains(err.Error(), "已提交过该手机号") {
		t.Fatalf("same accepted phone must be rejected locally, got %v", err)
	}
	if phoneSends != 2 {
		t.Fatalf("same accepted phone must not reach upstream again, sends=%d", phoneSends)
	}
	twoFA, err := protocol.SubmitSMSCode(ctx, id, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if twoFA["stage"] != "waiting_2fa" {
		t.Fatalf("stage after accepted SMS = %q", twoFA["stage"])
	}
	tokens, err := protocol.Submit2FA(ctx, id, "654321")
	if err != nil {
		t.Fatal(err)
	}
	if tokens["access_token"] != "access-1" || tokens["refresh_token"] != "refresh-1" {
		t.Fatalf("unexpected tokens: %#v", tokens)
	}
	if phoneSends != 2 || smsValidations != 1 {
		t.Fatalf("unexpected upstream phone flow counts: sends=%d sms=%d", phoneSends, smsValidations)
	}
}

func TestPhoneStepRequiresStructuredPhonePage(t *testing.T) {
	if isPhoneInputStep(map[string]any{"metadata": "phone_otp", "next": "https://auth.openai.com/phone-verification"}) {
		t.Fatal("phone OTP metadata must not be treated as a new phone input step")
	}
	if !isPhoneInputStep(map[string]any{"page": map[string]any{"type": "add_phone"}}) {
		t.Fatal("structured add_phone page must be treated as a phone input step")
	}
	if !isExplicitPhoneRejectionResult(map[string]any{"error": map[string]any{"code": "phone_number_already_used"}}) {
		t.Fatal("explicit phone rejection must be detected")
	}
	if !isExplicitPhoneRejectionResult(map[string]any{"error": map[string]any{
		"code":    "fraud_guard",
		"message": "We've detected suspicious behavior from phone numbers similar to yours.",
	}}) {
		t.Fatal("fraud_guard phone rejection must be detected")
	}
	if isExplicitPhoneRejectionResult(map[string]any{"page": map[string]any{"type": "add_phone"}, "message": "enter your phone number"}) {
		t.Fatal("ordinary add_phone prompt must not be treated as a phone rejection")
	}
	if !isEmailInputStep(map[string]any{"page": map[string]any{"type": "email_verification"}}) {
		t.Fatal("structured email verification page must be treated as an email input step")
	}
	if isEmailInputStep(map[string]any{"metadata": "email_otp", "message": "phone OTP accepted"}) {
		t.Fatal("email OTP metadata must not reset a later phone/SMS step")
	}
}

func TestProtocolBodyErrorDetectsInvalidState(t *testing.T) {
	err := protocolBodyError(http.StatusConflict, map[string]any{
		"error": map[string]any{"code": "invalid_state", "message": "Your sign-in session is no longer valid"},
	}, nil)
	if !errors.Is(err, ErrCodexOAuthSessionInvalid) {
		t.Fatalf("expected invalid session error, got %v", err)
	}
}

func TestCodexProtocolManualEmailCode(t *testing.T) {
	testCodexProtocolEmailCode(t, false)
}

func TestCodexProtocolAutomatic2FAAfterEmail(t *testing.T) {
	testCodexProtocolEmailCode(t, true)
}

func testCodexProtocolEmailCode(t *testing.T, automatic2FA bool) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the Sentinel runner test")
	}
	runner, err := filepath.Abs("../register/sentinel-runner.js")
	if err != nil {
		t.Fatal(err)
	}
	sdk := filepath.Join(t.TempDir(), "probe.js")
	if err := os.WriteFile(sdk, []byte(`var SentinelSDK = {
 token: async () => JSON.stringify({p:"probe",c:"challenge-token"}),
 sessionObserverToken: async () => "probe-so"
};`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CFM_SENTINEL_NODE", node)
	t.Setenv("CFM_SENTINEL_RUNNER", runner)
	t.Setenv("CFM_SENTINEL_SDK", sdk)
	var sentinelRequirements []string
	var mu sync.Mutex
	var paths []string
	var generated, verified int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/authorize", "/log-in":
			w.WriteHeader(http.StatusOK)
		case "/api/accounts/authorize/continue":
			if !strings.Contains(r.Header.Get("Openai-Sentinel-Token"), `"p":"probe"`) {
				t.Fatalf("initial authorize_continue request is missing the bound Sentinel header: %q", r.Header.Get("Openai-Sentinel-Token"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"page": map[string]any{"type": "email_verification"}})
		case "/backend-api/sentinel/frame.html":
			_, _ = w.Write([]byte("sentinel-frame"))
		case "/backend-api/sentinel/req":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["flow"] != "authorize_continue" || !strings.HasSuffix(body["p"], "~S") {
				t.Fatalf("unexpected initial/OTP Sentinel request: %#v", body)
			}
			sentinelRequirements = append(sentinelRequirements, body["p"])
			http.SetCookie(w, &http.Cookie{Name: "oai-sc", Value: "challenge-session", Path: "/"})
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "challenge-token", "so": map[string]any{"required": false}})
		case "/api/accounts/email-otp/validate":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["code"] != "987654" {
				t.Fatalf("unexpected email code: %q", body["code"])
			}
			sentinel := r.Header.Get("Openai-Sentinel-Token")
			if !strings.Contains(sentinel, `"p":"probe"`) || !strings.Contains(sentinel, `"c":"challenge-token"`) {
				t.Fatalf("email OTP request is missing the bound Sentinel header: %q", sentinel)
			}
			if automatic2FA {
				http.SetCookie(w, &http.Cookie{Name: "email-accepted", Value: "yes", Path: "/"})
				_ = json.NewEncoder(w).Encode(map[string]any{"page": map[string]any{"type": "mfa_challenge", "payload": map[string]any{"id": "factor-email"}}, "continue_url": "/mfa-challenge/factor-email"})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"page": map[string]any{"type": "add_phone"}, "metadata": "email_otp accepted"})
			}
		case "/api/accounts/mfa/issue_challenge", "/api/accounts/mfa/verify":
			if !automatic2FA {
				t.Error("MFA must not run unless the upstream requests it")
			}
			cookie, err := r.Cookie("email-accepted")
			if err != nil || cookie.Value != "yes" {
				t.Error("MFA lost the email verification cookie jar")
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["id"] != "factor-email" || body["type"] != "totp" {
				t.Errorf("MFA factor/type incorrect: %#v", body)
			}
			if r.URL.Path == "/api/accounts/mfa/verify" {
				verified++
				if body["code"] != "654321" {
					t.Error("automatic code was not submitted")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"page": map[string]any{"type": "add_phone"}, "metadata": map[string]any{"authentication_method": "totp"}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}
		case "/api/accounts/add-phone/send":
			w.WriteHeader(http.StatusNoContent)
		case "/api/accounts/phone-otp/validate":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []any{map[string]any{"id": "workspace-1", "name": "Personal"}}, "metadata": "email_otp is not a new email step"})
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
	protocol.sentinelOrigin = server.URL
	protocol.sentinelVersion = "test-version"
	var generate func(string) (string, error)
	if automatic2FA {
		generate = func(id string) (string, error) {
			if id == "" {
				t.Error("missing session id")
			}
			mu.Lock()
			generated++
			mu.Unlock()
			return "654321", nil
		}
	}
	started, err := protocol.StartWithTOTP(context.Background(), "person@example.test", "", generate)
	if err != nil {
		t.Fatal(err)
	}
	if started["stage"] != "waiting_email_code" {
		t.Fatalf("stage after start = %q", started["stage"])
	}
	phone, err := protocol.SubmitEmailCode(context.Background(), started["session_id"], "987654")
	if err != nil {
		t.Fatal(err)
	}
	if phone["stage"] != "waiting_phone" {
		t.Fatalf("stage after email code = %q", phone["stage"])
	}
	if _, err := protocol.SubmitEmailCode(context.Background(), started["session_id"], "987654"); err == nil {
		t.Fatal("email OTP cannot be resubmitted after advancing to phone")
	}
	sms, err := protocol.SubmitPhone(context.Background(), started["session_id"], "+8613800138000")
	if err != nil {
		t.Fatal(err)
	}
	if sms["stage"] != "waiting_sms_code" {
		t.Fatalf("stage after phone = %q", sms["stage"])
	}
	tokens, err := protocol.SubmitSMSCode(context.Background(), started["session_id"], "123456")
	if err != nil {
		t.Fatal(err)
	}
	if tokens["access_token"] != "access-1" || tokens["refresh_token"] != "refresh-1" {
		t.Fatalf("unexpected tokens: %#v", tokens)
	}
	mu.Lock()
	defer mu.Unlock()
	if automatic2FA {
		if generated != 1 || verified != 1 {
			t.Fatalf("MFA attempts: generated=%d verified=%d", generated, verified)
		}
		var authPaths []string
		for _, path := range paths {
			if strings.HasPrefix(path, "/api/accounts/") {
				authPaths = append(authPaths, path)
			}
		}
		expected := "/api/accounts/authorize/continue,/api/accounts/email-otp/validate,/api/accounts/mfa/issue_challenge,/api/accounts/mfa/verify,/api/accounts/add-phone/send,/api/accounts/phone-otp/validate,/api/accounts/workspace/select"
		if strings.Join(authPaths, ",") != expected {
			t.Fatalf("unexpected authorization sequence: %v", authPaths)
		}
	}
	if len(sentinelRequirements) != 2 || sentinelRequirements[0] != sentinelRequirements[1] {
		t.Fatalf("initial and OTP Sentinel requests must reuse one top-level p: %#v", sentinelRequirements)
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
