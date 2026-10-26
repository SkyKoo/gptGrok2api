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
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"session_id":"session-1","token":"turnstile-token"}`)), Header: make(http.Header)}, nil
	}), Timeout: time.Second}
	profile := NewBrowserProfile()
	profile.TimezoneIANA = "America/Phoenix"
	profile.NavigatorLanguage = "en-US"
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", Client: client}
	solver.SetBrowserProfile(profile)
	if _, err := solver.SolveContext(context.Background(), "site-key", "https://accounts.x.ai/sign-up", "direct"); err != nil {
		t.Fatalf("SolveContext() error = %v", err)
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
