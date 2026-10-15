package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/browserprofile"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	fhttp "github.com/bogdanfinn/fhttp"
)

func TestBrowserEnvironmentSharedByHeadersRequirementsAndConversation(t *testing.T) {
	for _, p := range browserprofile.Candidates() {
		t.Run(p.Name, func(t *testing.T) {
			fields := map[string]any{"browser_profile": p, "fp": map[string]any{"oai-device-id": "saved-device", "user-agent": "stale-other-browser"}}
			req, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/conversation", nil)
			setOpenAIHeaders(req, req.URL.Path, "token", fields)
			for key, want := range p.Headers() {
				if req.Header.Get(key) != want {
					t.Errorf("%s differs from login", key)
				}
			}
			loginHeaders := fhttp.Header{}
			for key, value := range p.Headers() {
				loginHeaders.Set(key, value)
			}
			browserprofile.SetHeaderOrder(loginHeaders)
			businessHeaders := http.Header{}
			for key, value := range p.Headers() {
				businessHeaders.Set(key, value)
			}
			if !reflect.DeepEqual(loginHeaders[fhttp.HeaderOrderKey], cloneFHTTPHeader(businessHeaders)[fhttp.HeaderOrderKey]) {
				t.Fatal("login and business header ordering differs")
			}
			if req.Header.Get("Oai-Device-Id") != "saved-device" || req.Header.Get("Oai-Language") != p.NavigatorLanguage {
				t.Fatal("identity/language changed")
			}
			first := req.Header.Get("Oai-Session-Id")
			setOpenAIHeaders(req, req.URL.Path, "token", fields)
			if first == "" || first != req.Header.Get("Oai-Session-Id") {
				t.Fatal("session changes between requests")
			}
			setOpenAIHeaders(req, req.URL.Path, "rotated-token", fields)
			if first == req.Header.Get("Oai-Session-Id") {
				t.Fatal("new credentials reused old session")
			}
			zone := openAITimezoneSnapshot{location: time.FixedZone("Phoenix", -7*3600), at: time.Now()}
			for _, token := range []string{buildLegacyRequirementsToken(p, nil, "build", zone), buildProofToken("seed", "ff", p, nil, "build", zone)} {
				raw, err := base64.StdEncoding.DecodeString(token[7:])
				if err != nil {
					t.Fatal(err)
				}
				var values []any
				if json.Unmarshal(raw, &values) != nil {
					t.Fatal("bad requirements")
				}
				if values[4] != p.UserAgent || values[7] != p.NavigatorLanguage || values[8] != p.NavigatorLanguagesValue() || values[16] != float64(p.HardwareConcurrency) || values[2] != float64(p.JSHeapSizeLimit) {
					t.Fatal("requirements differ from saved environment")
				}
			}
			payload := map[string]any{"client_contextual_info": map[string]any{"app_name": "chatgpt.com", "screen_width": 1}}
			applyOpenAIBrowserContext(fields, payload)
			info := payload["client_contextual_info"].(map[string]any)
			if info["screen_width"] != p.ScreenWidth || info["page_height"] != p.InnerHeight || info["pixel_ratio"] != p.DevicePixelRatio || info["app_name"] != "chatgpt.com" {
				t.Fatal("conversation dimensions differ")
			}
			vm := sentinelVM{environment: sentinelBrowserProperties(p)}
			if vm.property("window.navigator", "userAgent") != p.UserAgent || vm.property("window.screen", "height") != float64(p.ScreenHeight) {
				t.Fatal("VM environment differs")
			}
		})
	}
}

func TestBrowserClientsIsolateAccountsEgressAndCredentialGenerations(t *testing.T) {
	client := NewOpenAIImage("https://chatgpt.com", nil, nil, time.Second)
	p := browserprofile.Default()
	a := accounts.Account{Token: "a", Fields: map[string]any{"browser_profile": p, "web_cookies_token_hash": browserprofile.TokenBinding("a"), "web_cookies": []browserprofile.Cookie{{Name: "session", Value: "a-login", Path: "/", Secure: true}}}}
	b := accounts.Account{Token: "b", Fields: map[string]any{"browser_profile": p}}
	first, err := client.browserFor("", a)
	if err != nil {
		t.Fatal(err)
	}
	other, err := client.browserFor("", b)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://chatgpt.com/")
	if first == other || len(other.client.GetCookies(u)) != 0 || len(first.client.GetCookies(u)) != 1 {
		t.Fatal("login cookies mixed between accounts")
	}
	if first.profile.TLSProfile != p.TLSProfile {
		t.Fatal("TLS profile differs from login")
	}
	first.client.SetCookies(u, []*fhttp.Cookie{{Name: "runtime", Value: "a-runtime", Path: "/"}})
	again, _ := client.browserFor("", a)
	if again != first || len(again.client.GetCookies(u)) != 2 {
		t.Fatal("same account lost in-session cookies")
	}
	rotated := a
	rotated.Token = "rotated"
	newSession, _ := client.browserFor("", rotated)
	if newSession == first || len(newSession.client.GetCookies(u)) != 0 {
		t.Fatal("rotated token reused stale jar")
	}
	differentEgress, err := client.browserFor("http://127.0.0.1:1", a)
	if err != nil || differentEgress == first {
		t.Fatal("egresses share a browser")
	}
	changed := a
	changed.Fields = map[string]any{"browser_profile": browserprofile.Candidates()[0]}
	differentProfile, _ := client.browserFor("", changed)
	if differentProfile == first {
		t.Fatal("profile update reused old TLS state")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			same, e := client.browserFor("", a)
			if e != nil || same != first {
				t.Error("parallel requests did not reuse account client")
			}
		}()
	}
	wg.Wait()
}

func TestLegacyProfilesAndPrivateCookieDiagnostics(t *testing.T) {
	p := browserprofile.Candidates()[2]
	recovered, err := accountBrowserProfile(map[string]any{"fp": p.FingerprintMetadata()})
	if err != nil || !reflect.DeepEqual(p, recovered) {
		t.Fatal("known legacy profile changed")
	}
	legacy := buildOpenAIFingerprint(map[string]any{"oai_device_id": "legacy-device", "oai_session_id": "legacy-session"}, "token")
	if legacy.DeviceID != "legacy-device" || legacy.SessionID != "legacy-session" {
		t.Fatal("lost imported stable identity")
	}
	for _, fields := range []map[string]any{{"browser_profile": map[string]any{"tls_profile": "unknown"}}, {"fp": map[string]any{"profile": "unknown"}}} {
		if _, err := accountBrowserProfile(fields); err == nil {
			t.Fatal("silently replaced a saved profile")
		}
	}
	fields := map[string]any{"web_cookies": []browserprofile.Cookie{{Name: "session", Value: "private-cookie-value", Path: "/"}}}
	err = errors.New("upstream echoed private-cookie-value")
	if strings.Contains(ChatErrorDetails(err, accounts.Account{Fields: fields}).Message, "private-cookie-value") || strings.Contains(accountEndpointError("/backend-api/me", 502, "", err, fields).Message, "private-cookie-value") {
		t.Fatal("cookie value leaked into diagnostics")
	}
}

func TestRestoredLoginCookieUsedByAccountAndConversationClients(t *testing.T) {
	p := browserprofile.Default()
	seen := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen++
		c, err := r.Cookie("session")
		if err != nil || c.Value != "fresh-login" {
			t.Error("missing restored login cookie")
		}
		for key, want := range p.Headers() {
			if r.Header.Get(key) != want {
				t.Errorf("%s not shared", key)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer s.Close()
	fields := map[string]any{"browser_profile": p, "web_cookies_token_hash": browserprofile.TokenBinding("token"), "web_cookies": []browserprofile.Cookie{{Name: "session", Value: "fresh-login", Path: "/"}}}
	accountClient := NewOpenAIAccountClient(s.URL, "", s.Client(), nil)
	if _, err := accountClient.getMe(context.Background(), "token", fields); err != nil {
		t.Fatal(err)
	}
	imageClient := NewOpenAIImage(s.URL, s.Client(), nil, time.Second)
	r, err := imageClient.do(context.Background(), "POST", "/backend-api/f/conversation", accounts.Account{Token: "token", Fields: fields}, map[string]any{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if seen != 2 {
		t.Fatal("missing requests")
	}
}

func TestWebCookieCannotFollowManuallyReplacedToken(t *testing.T) {
	fields := map[string]any{"web_cookies": []browserprofile.Cookie{{Name: "session", Value: "old-session", Path: "/"}}, "web_cookies_token_hash": browserprofile.TokenBinding("old-token")}
	for _, token := range []string{"old-token", "new-token", ""} {
		req, _ := http.NewRequest("GET", "https://chatgpt.com/backend-api/me", nil)
		applySavedWebCookies(req, "https://chatgpt.com", fields, token)
		if got := req.Header.Get("Cookie") != ""; got != (token == "old-token") {
			t.Fatal("cookie did not respect login credential binding")
		}
	}
}

func TestConversationHTTPErrorRedactsLoginCookies(t *testing.T) {
	const privateCookie = "opaque-private-web-session"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"detail":"upstream echoed ` + privateCookie + `"}`))
	}))
	defer s.Close()
	client := NewOpenAIImage(s.URL, s.Client(), nil, time.Second)
	a := accounts.Account{Token: "access-token", Fields: map[string]any{"web_cookies": []browserprofile.Cookie{{Name: "session", Value: privateCookie, Path: "/"}}, "web_cookies_token_hash": browserprofile.TokenBinding("access-token")}}
	_, err := client.do(context.Background(), "POST", "/backend-api/f/conversation", a, map[string]any{}, nil, false)
	var upstream *protocol.UpstreamError
	if !errors.As(err, &upstream) || upstream.Status != 502 || upstream.Endpoint != "/backend-api/f/conversation" {
		t.Fatal("lost upstream status or endpoint")
	}
	if strings.Contains(err.Error()+upstream.Body, privateCookie) || !strings.Contains(upstream.Body, "[REDACTED]") {
		t.Fatal("HTTP diagnostic leaked a saved login cookie")
	}
}
