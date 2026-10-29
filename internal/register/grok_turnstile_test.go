package register

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestHTTPGrokBrowserSolverReturnsContext(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/turnstile/session" || req.Method != http.MethodPost {
			t.Fatalf("unexpected worker request: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer worker-secret" {
			t.Fatalf("missing worker authorization")
		}
		body, _ := io.ReadAll(req.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("invalid worker payload: %v", err)
		}
		if payload["sitekey"] != "site-key" || payload["url"] != "https://accounts.x.ai/sign-up" {
			t.Fatalf("unexpected worker payload: %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"session_id":"session-1","token":"turnstile-token","cookies":[{"name":"cf_clearance","value":"cookie","domain":"accounts.x.ai","path":"/"}],"expires_at":"2030-01-01T00:00:00Z"}`)), Header: make(http.Header)}, nil
	}), Timeout: time.Second}
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", APIKey: "worker-secret", Client: client, Timeout: 60 * time.Second}
	result, err := solver.SolveContext(context.Background(), "site-key", "https://accounts.x.ai/sign-up", "direct")
	if err != nil {
		t.Fatalf("SolveContext() error = %v", err)
	}
	if result.Token != "turnstile-token" || result.SessionID != "session-1" || len(result.Cookies) != 1 {
		t.Fatalf("unexpected browser context: %+v", result)
	}
}

func TestHTTPGrokBrowserSolverPassesProfileEnvironment(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("invalid worker payload: %v", err)
		}
		if payload["timezone"] != "America/Phoenix" || payload["locale"] != "en-US" {
			t.Fatalf("profile environment was not forwarded: %s", body)
		}
		workerProfile, ok := payload["browser_profile"].(map[string]any)
		if !ok {
			t.Fatalf("full browser profile was not forwarded: %s", body)
		}
		if workerProfile["navigator_language"] != "en-US" || workerProfile["accept_language"] != "en-US,en;q=0.9" {
			t.Fatalf("old task language was not normalized: %v", workerProfile)
		}
		if workerProfile["user_agent"] != "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36" {
			t.Fatalf("worker profile lost user agent: %v", workerProfile)
		}
		if workerProfile["inner_width"] != float64(1440) || workerProfile["device_pixel_ratio"] != float64(2) {
			t.Fatalf("worker profile lost viewport fields: %v", workerProfile)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"session_id":"session-1","token":"turnstile-token"}`)), Header: make(http.Header)}, nil
	}), Timeout: time.Second}
	profile := NewBrowserProfile()
	profile.TimezoneIANA = "America/Phoenix"
	profile.NavigatorLanguage = "zh-CN"
	profile.NavigatorLanguages = []string{"zh-CN", "zh"}
	profile.AcceptLanguage = "zh-CN,zh;q=0.9"
	profile.UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36"
	profile.InnerWidth = 1440
	profile.DevicePixelRatio = 2
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", Client: client}
	solver.SetBrowserProfile(profile)
	if _, err := solver.SolveContext(context.Background(), "site-key", "https://accounts.x.ai/sign-up", "direct"); err != nil {
		t.Fatalf("SolveContext() error = %v", err)
	}
	if _, err := solver.StartSignup(context.Background(), "https://accounts.x.ai/sign-up", "direct"); err != nil {
		t.Fatalf("StartSignup() error = %v", err)
	}
}

func TestHTTPGrokBrowserSolverRejectsMissingToken(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"blocked"}`)), Header: make(http.Header)}, nil
	})}
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", Client: client}
	if _, err := solver.Solve(context.Background(), "site-key", "https://accounts.x.ai/sign-up", "direct"); err == nil {
		t.Fatal("Solve() accepted a missing token")
	}
}

func TestHTTPGrokBrowserSolverClosesSession(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/v1/session/close" {
			t.Fatalf("unexpected close request: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer worker-secret" {
			t.Fatalf("missing worker authorization")
		}
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), `"session_id":"session-1"`) {
			t.Fatalf("unexpected close payload: %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: make(http.Header)}, nil
	}), Timeout: time.Second}
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", APIKey: "worker-secret", Client: client}
	if err := solver.CloseSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("CloseSession() error = %v", err)
	}
}

func TestHTTPGrokBrowserSolverNativeSignupFlow(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Fatalf("unexpected native method: %s", req.Method)
		}
		body, _ := io.ReadAll(req.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("invalid native payload: %v", err)
		}
		var response string
		switch req.URL.Path {
		case "/v1/session/start":
			if payload["url"] != "https://accounts.x.ai/sign-up?redirect=grok-com" {
				t.Fatalf("unexpected start payload: %s", body)
			}
			response = `{"ok":true,"session_id":"native-1","url":"https://accounts.x.ai/sign-up?redirect=grok-com","user_agent":"Chrome"}`
		case "/v1/session/send-email":
			if payload["session_id"] != "native-1" || payload["email"] != "person@example.com" {
				t.Fatalf("unexpected send payload: %s", body)
			}
			response = `{"ok":true}`
		case "/v1/session/verify-email":
			if payload["code"] != "123456" {
				t.Fatalf("unexpected verify payload: %s", body)
			}
			response = `{"ok":true,"session_id":"native-1","cookies":[],"user_agent":"Chrome"}`
		case "/v1/session/signup":
			if payload["password"] != "secret-pass" || payload["given_name"] != "Grok" {
				t.Fatalf("unexpected signup payload: %s", body)
			}
			response = `{"ok":true,"session_id":"native-1","sso":"session-token","sso_rw":"session-token-rw","redirect_url":"https://grok.com/","user_agent":"Chrome"}`
		default:
			t.Fatalf("unexpected native path: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	}), Timeout: time.Second}
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", Client: client}
	started, err := solver.StartSignup(context.Background(), "https://accounts.x.ai/sign-up?redirect=grok-com", "direct")
	if err != nil || started.SessionID != "native-1" {
		t.Fatalf("StartSignup() = %+v, %v", started, err)
	}
	if err := solver.SendEmailCode(context.Background(), started.SessionID, "person@example.com"); err != nil {
		t.Fatalf("SendEmailCode() error = %v", err)
	}
	if _, err := solver.VerifyEmailCode(context.Background(), started.SessionID, "person@example.com", "123456"); err != nil {
		t.Fatalf("VerifyEmailCode() error = %v", err)
	}
	completed, err := solver.CompleteSignup(context.Background(), started.SessionID, "person@example.com", "secret-pass", "123456", "Grok", "User", "")
	if err != nil || completed.SSO != "session-token" || completed.SSORW != "session-token-rw" {
		t.Fatalf("CompleteSignup() = %+v, %v", completed, err)
	}
}

func TestHTTPGrokBrowserSolverPreservesSafeWorkerReason(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusGatewayTimeout,
			Body:       io.NopCloser(strings.NewReader(`{"detail":{"stage":"send_code","error":"signup_rate_limited"}}`)),
			Header:     make(http.Header),
		}, nil
	})}
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", Client: client}
	err := solver.SendEmailCode(context.Background(), "native-1", "person@example.com")
	if err == nil {
		t.Fatal("SendEmailCode() accepted a worker rejection")
	}
	got := err.Error()
	if !strings.Contains(got, "browser_worker_rejected (HTTP 504)") || !strings.Contains(got, "upstream_stage=send_code") || !strings.Contains(got, "operation=worker_error=signup_rate_limited") {
		t.Fatalf("worker detail was not preserved safely: %s", got)
	}
	if strings.Contains(got, "page") || strings.Contains(got, "email@example") {
		t.Fatalf("worker detail leaked unexpected content: %s", got)
	}
}

func TestHTTPGrokBrowserSolverPreservesSafeWorkerDiagnostic(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusGatewayTimeout,
			Body:       io.NopCloser(strings.NewReader(`{"detail":{"stage":"send_code","error":"verification_page_missing","diagnostic":{"reason":"cloudflare_challenge","url":"https://accounts.x.ai/sign-up?email=secret@example.com","title":"Attention Required! | Cloudflare","http_status":403}}}`)),
			Header:     make(http.Header),
		}, nil
	}), Timeout: time.Second}
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", Client: client}
	err := solver.SendEmailCode(context.Background(), "native-1", "person@example.com")
	if err == nil {
		t.Fatal("SendEmailCode() accepted a worker rejection")
	}
	got := err.Error()
	if !strings.Contains(got, "operation=worker_error=verification_page_missing;diagnostic=cloudflare_challenge") {
		t.Fatalf("safe diagnostic was not preserved: %s", got)
	}
	if strings.Contains(got, "secret@example.com") || strings.Contains(got, "Attention Required") || strings.Contains(got, "accounts.x.ai") {
		t.Fatalf("worker diagnostic leaked sensitive page details: %s", got)
	}
}

func TestHTTPGrokBrowserSolverPreservesTurnstileState(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusGatewayTimeout,
			Body:       io.NopCloser(strings.NewReader(`{"detail":{"stage":"turnstile","error":"turnstile_incomplete","diagnostic":{"reason":"turnstile_incomplete","turnstile_clicks":4,"turnstile_frames":2,"turnstile_visible_frames":1,"turnstile_widgets":1,"turnstile_ready":true,"turnstile_callback_seen":false,"turnstile_token_present":false}}}`)),
			Header:     make(http.Header),
		}, nil
	})}
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", Client: client}
	_, err := solver.CompleteSignup(context.Background(), "native-1", "person@example.com", "secret-pass", "123456", "Grok", "User", "")
	if err == nil {
		t.Fatal("CompleteSignup() accepted an incomplete Turnstile response")
	}
	got := err.Error()
	for _, want := range []string{
		"browser_worker_rejected (HTTP 504)",
		"upstream_stage=turnstile",
		"operation=worker_error=turnstile_incomplete;turnstile_clicks=4;turnstile_frames=2;turnstile_visible_frames=1;turnstile_widgets=1;turnstile_ready=true;turnstile_callback=false;turnstile_token=false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Turnstile state missing %q: %s", want, got)
		}
	}
}

func TestHTTPGrokBrowserSolverPreservesUpstreamEmailAvailabilityError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader(`{"detail":{"stage":"send_code","error":"signup_unavailable","diagnostic":{"reason":"signup_unavailable","upstream_code":"email_signup_unavailable","upstream_status":400}}}`)),
			Header:     make(http.Header),
		}, nil
	})}
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", Client: client}
	err := solver.SendEmailCode(context.Background(), "native-1", "person@example.com")
	if err == nil {
		t.Fatal("SendEmailCode() accepted a worker rejection")
	}
	got := err.Error()
	for _, want := range []string{
		"browser_worker_rejected (HTTP 502)",
		"upstream_stage=send_code",
		"operation=worker_error=signup_unavailable;upstream_code=email_signup_unavailable;upstream_status=400",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("worker upstream detail missing %q: %s", want, got)
		}
	}
}

func TestHTTPGrokBrowserSolverResinTaskSession(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Fatalf("unexpected native method: %s", req.Method)
		}
		body, _ := io.ReadAll(req.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("invalid native payload: %v", err)
		}
		if req.URL.Path != "/v1/session/start" && payload["session_id"] != "native-1" {
			t.Fatal("registration step changed browser session")
		}
		var response string
		switch req.URL.Path {
		case "/v1/session/start":
			if payload["proxy"] != "http://CFM-Grok.task-fixture:fixture-token@resin.invalid:2260" {
				t.Fatal("Worker lost task proxy identity")
			}
			if payload["url"] != "https://accounts.x.ai/sign-up?redirect=grok-com" {
				t.Fatalf("unexpected start payload: %s", body)
			}
			response = `{"ok":true,"session_id":"native-1","url":"https://accounts.x.ai/sign-up?redirect=grok-com","user_agent":"Chrome"}`
		case "/v1/session/send-email":
			if payload["session_id"] != "native-1" || payload["email"] != "person@example.com" {
				t.Fatalf("unexpected send payload: %s", body)
			}
			response = `{"ok":true}`
		case "/v1/session/verify-email":
			if payload["code"] != "123456" {
				t.Fatalf("unexpected verify payload: %s", body)
			}
			response = `{"ok":true,"session_id":"native-1","cookies":[],"user_agent":"Chrome"}`
		case "/v1/session/signup":
			if payload["password"] != "secret-pass" || payload["given_name"] != "Grok" {
				t.Fatalf("unexpected signup payload: %s", body)
			}
			response = `{"ok":true,"session_id":"native-1","sso":"session-token","sso_rw":"session-token-rw","redirect_url":"https://grok.com/","user_agent":"Chrome"}`
		default:
			t.Fatalf("unexpected native path: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	}), Timeout: time.Second}
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", Client: client}
	started, err := solver.StartSignup(context.Background(), "https://accounts.x.ai/sign-up?redirect=grok-com", "http://CFM-Grok.task-fixture:fixture-token@resin.invalid:2260")
	if err != nil || started.SessionID != "native-1" {
		t.Fatalf("StartSignup() = %+v, %v", started, err)
	}
	if err := solver.SendEmailCode(context.Background(), started.SessionID, "person@example.com"); err != nil {
		t.Fatalf("SendEmailCode() error = %v", err)
	}
	if _, err := solver.VerifyEmailCode(context.Background(), started.SessionID, "person@example.com", "123456"); err != nil {
		t.Fatalf("VerifyEmailCode() error = %v", err)
	}
	completed, err := solver.CompleteSignup(context.Background(), started.SessionID, "person@example.com", "secret-pass", "123456", "Grok", "User", "")
	if err != nil || completed.SSO != "session-token" || completed.SSORW != "session-token-rw" {
		t.Fatalf("CompleteSignup() = %+v, %v", completed, err)
	}
}
