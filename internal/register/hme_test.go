package register

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHMEInternalSecureSessionIsPinnedAndRedirectsAreRejected(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "public_http", true: "pinned_internal_http"}[enabled], func(t *testing.T) {
			creates := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respond := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data}) }
				if r.URL.Path == "/api/auth/login" {
					http.SetCookie(w, &http.Cookie{Name: "hme_session", Value: "session", Path: "/api/", Secure: true, HttpOnly: true})
					respond(map[string]string{"csrf_token": "csrf"})
					return
				}
				cookie, err := r.Cookie("hme_session")
				if err != nil || cookie.Value != "session" {
					w.WriteHeader(401)
					_, _ = w.Write([]byte(`{"success":false,"code":"AUTH_REQUIRED"}`))
					return
				}
				if r.Header.Get("X-CSRF-Token") != "csrf" {
					t.Error("missing csrf")
				}
				creates++
				respond(map[string]string{"email": "alias@example.test", "account_id": "acc-test"})
			}))
			defer server.Close()
			// Use a Docker-style host, not loopback: newer Go releases may treat
			// localhost as a secure cookie context even when its scheme is HTTP.
			baseURL := "http://hme-internal.test:8081"
			pinned := ""
			if enabled {
				pinned = baseURL
			}
			t.Setenv("GO_HME_INTERNAL_BASE_URL", pinned)
			h, err := NewHME(HMEConfig{baseURL, "password", "acc-test"}, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			h.http.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
			}
			_, err = h.Acquire(context.Background(), "job")
			if enabled {
				if err != nil || creates != 1 {
					t.Fatalf("internal authentication failed: %v", err)
				}
				for _, raw := range []string{baseURL + "/outside-cookie-path", "http://other.invalid/api/create", "http://hme-internal.test:8082/api/create", strings.Replace(baseURL, "http:", "https:", 1) + "/api/create"} {
					u, _ := url.Parse(raw)
					if len(h.http.Jar.Cookies(u)) != 0 {
						t.Fatalf("session escaped scope: %s", raw)
					}
				}
			} else if err == nil || creates != 0 {
				t.Fatal("public HTTP unexpectedly received Secure cookie")
			}
		})
	}
	foreignCalls := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls++ }))
	defer foreign.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	t.Setenv("GO_HME_INTERNAL_BASE_URL", origin.URL)
	h, _ := NewHME(HMEConfig{origin.URL, "private-password", "acc-test"}, time.Millisecond)
	defer h.Close()
	if _, err := h.Acquire(context.Background(), "job"); err == nil || foreignCalls != 0 {
		t.Fatal("followed redirect with credentials")
	}
}

func TestHMEAuthenticatesCreatesAliasAndReadsFreshJunkCode(t *testing.T) {
	created, detail := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data}) }
		if r.URL.Path == "/api/auth/login" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["password"] != " test-password " {
				t.Error("password was modified")
			}
			http.SetCookie(w, &http.Cookie{Name: "hme_session", Value: "fake-session", Path: "/"})
			respond(map[string]string{"csrf_token": "fake-csrf"})
			return
		}
		if c, err := r.Cookie("hme_session"); err != nil || c.Value != "fake-session" {
			t.Error("session not retained")
		}
		switch r.URL.Path {
		case "/api/create":
			created++
			if r.Header.Get("X-CSRF-Token") != "fake-csrf" {
				t.Error("missing CSRF")
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["account_id"] != "acc-test" || body["label"] != "CFM Free job-test" {
				t.Error("wrong alias input")
			}
			respond(map[string]string{"email": "alias@example.test", "account_id": "acc-test"})
		case "/api/inbox":
			if r.URL.Query().Get("alias") != "alias@example.test" || r.URL.Query().Get("folder") != "all" {
				t.Error("missing alias/folder filter")
			}
			now := time.Now().UTC().Format(time.RFC3339)
			respond(map[string]any{"method": "web_api", "messages": []map[string]string{
				{"id": "1", "folder": "inbox", "from": "noreply@openai.com", "to": "alias@example.test", "subject": "111111", "date": time.Now().Add(-time.Hour).Format(time.RFC3339)},
				{"id": "2", "folder": "inbox", "from": "noreply@openai.com", "to": "other@example.test", "subject": "222222", "date": now},
				{"id": "3", "folder": "inbox", "from": "OpenAI <spam@example.test>", "to": "alias@example.test", "subject": "333333", "date": now},
				{"id": "4", "folder": "junk", "from": "OpenAI <noreply@tm.openai.com>", "to": "Alias <alias@example.test>", "subject": "Verify your email", "date": now},
			}})
		case "/api/inbox/4":
			detail++
			if r.URL.Query().Get("folder") != "junk" || r.URL.Query().Get("method") != "web_api" {
				t.Error("lost message source")
			}
			respond(map[string]string{"from": "noreply@tm.openai.com", "to": "alias@example.test", "date": time.Now().UTC().Format(time.RFC3339), "body": "Your code is 987654."})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	h, err := NewHME(HMEConfig{BaseURL: server.URL, Password: " test-password ", AccountID: "acc-test"}, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	box, err := h.Acquire(context.Background(), "job-test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	code, err := h.WaitCode(ctx, box, time.Now().Add(-10*time.Second))
	if err != nil || code != "987654" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	if created != 1 || detail != 1 {
		t.Fatalf("created=%d detail=%d", created, detail)
	}
}

func TestHMETimeoutRedactionAndNoMutationRetry(t *testing.T) {
	t.Run("creation_failure", func(t *testing.T) {
		creates := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/auth/login" {
				_, _ = w.Write([]byte(`{"success":true,"data":{"csrf_token":"csrf"}}`))
				return
			}
			creates++
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"success":false,"code":"unknown SECRET","message":"password=SECRET"}`))
		}))
		defer server.Close()
		h, _ := NewHME(HMEConfig{server.URL, "password", "acc-test"}, time.Millisecond)
		defer h.Close()
		_, err := h.Acquire(context.Background(), "job")
		if err == nil || strings.Contains(err.Error(), "SECRET") || creates != 1 {
			t.Fatalf("unsafe error or retry: %v creates=%d", err, creates)
		}
	})
	t.Run("cancel_poll", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"success":true,"data":{"messages":[],"method":"imap"}}`))
		}))
		defer server.Close()
		h, _ := NewHME(HMEConfig{server.URL, "password", "acc-test"}, time.Second)
		defer h.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := h.WaitCode(ctx, Mailbox{Email: "alias@example.test", AccountID: "acc-test"}, time.Now())
		if err != context.DeadlineExceeded {
			t.Fatalf("expected cancellation, got %v", err)
		}
	})
}

func TestMailCodeRejectsAmbiguousAndEmbeddedDigits(t *testing.T) {
	for _, input := range []string{"1234567", "one 123456 two 654321", "no code"} {
		if mailCode(input) != "" {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestHMEAcceptsOpenAIForwarderAddress(t *testing.T) {
	after := time.Now().UTC().Add(-time.Minute)
	box := Mailbox{Email: "alias@example.test", AccountID: "acc-test"}
	for _, sender := range []string{
		"noreply@tm.openai.com",
		"noreply_at_tm_openai_com_88czx49dw80135_51446d5b@icloud.com",
		"noreply_at_chatgpt_com_opaque@icloud.com",
	} {
		if !suitableMessage(hmeMessage{From: sender, To: box.Email, Date: time.Now().UTC().Format(time.RFC3339)}, box, after) {
			t.Errorf("accepted sender was rejected: %s", sender)
		}
	}
	for _, sender := range []string{"random@icloud.com", "noreply_at_example_com@icloud.com"} {
		if suitableMessage(hmeMessage{From: sender, To: box.Email, Date: time.Now().UTC().Format(time.RFC3339)}, box, after) {
			t.Errorf("unrelated sender was accepted: %s", sender)
		}
	}
}
