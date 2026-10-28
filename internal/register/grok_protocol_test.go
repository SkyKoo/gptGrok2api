package register

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
)

const syntheticGrokSSO = "eyJhbGciOiJub25lIn0.eyJzdWIiOiIxIn0.synthetic-session-signature"

func TestExtractGrokSessionTokenFromResponse(t *testing.T) {
	value := map[string]any{
		"session": map[string]any{
			"session_cookie": syntheticGrokSSO,
		},
	}
	if got := extractGrokSessionToken(value); got != syntheticGrokSSO {
		t.Fatalf("extractGrokSessionToken() = %q", got)
	}
}

func TestExtractGrokSSOCookieHeaders(t *testing.T) {
	headers := fhttp.Header{}
	headers.Add("Set-Cookie", "sso-rw="+syntheticGrokSSO+"; Path=/; HttpOnly; Secure")
	if got := extractGrokSSOCookieHeaders(headers); got != syntheticGrokSSO {
		t.Fatalf("extractGrokSSOCookieHeaders() = %q", got)
	}
}

func TestExtractGrokRedirectURLsFiltersUntrustedHosts(t *testing.T) {
	value := map[string]any{
		"redirectUrl": "https://accounts.x.ai/api/cookie-chain?q=" + syntheticGrokSSO,
		"nested": map[string]any{
			"redirectUrl": "https://example.test/steal",
		},
	}
	urls := extractGrokRedirectURLs(value)
	if len(urls) != 1 || urls[0] != value["redirectUrl"] {
		t.Fatalf("unexpected redirect URLs: %#v", urls)
	}
}

func TestExpandGrokSSOHopsIncludesCookieChainSuccessURL(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"config": map[string]any{"success_url": "https://grok.com/"}})
	bootstrap := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	hops := expandGrokSSOHops("https://accounts.x.ai/set-cookie?q=" + bootstrap)
	joined := strings.Join(hops, "\n")
	if !strings.Contains(joined, "https://auth.grokusercontent.com/set-cookie?q="+bootstrap) || !strings.Contains(joined, "https://grok.com/") {
		t.Fatalf("cookie hop expansion omitted expected routes: %#v", hops)
	}
}

func TestDiscoverGrokActionIDPrefersSignupServerReference(t *testing.T) {
	source := `const a=createServerReference)("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","validatePassword");const b=createServerReference)("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","createUserAndSession");`
	if got := discoverGrokActionID(source); got != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("discoverGrokActionID() = %q", got)
	}
}

func TestGRPCResponseStatusHeader(t *testing.T) {
	headers := fhttp.Header{}
	headers.Set("grpc-status", "7")
	headers.Set("grpc-message", "permission denied")
	status, message, found := grpcResponseStatus(nil, headers)
	if !found || status != "7" || message != "permission denied" {
		t.Fatalf("unexpected header status: %q %q %v", status, message, found)
	}
	if grpcOK(nil, headers) {
		t.Fatal("non-zero grpc header status accepted")
	}
}

func TestGRPCResponseStatusTrailerFrame(t *testing.T) {
	payload := []byte("grpc-status: 0\r\ngrpc-message: ok\r\n")
	body := make([]byte, 5+len(payload))
	body[0] = 0x80
	binary.BigEndian.PutUint32(body[1:5], uint32(len(payload)))
	copy(body[5:], payload)
	status, message, found := grpcResponseStatus(body, nil)
	if !found || status != "0" || message != "ok" {
		t.Fatalf("unexpected trailer status: %q %q %v", status, message, found)
	}
	if !grpcOK(body, nil) {
		t.Fatal("zero grpc trailer status rejected")
	}
}

func TestGRPCResponseStatusTrailerRejects(t *testing.T) {
	payload := []byte("grpc-status: 8\r\ngrpc-message: rate limited\r\n")
	body := make([]byte, 5+len(payload))
	body[0] = 0x80
	binary.BigEndian.PutUint32(body[1:5], uint32(len(payload)))
	copy(body[5:], payload)
	status, message, found := grpcResponseStatus(body, nil)
	if !found || status != "8" || message != "rate limited" {
		t.Fatalf("unexpected rejection trailer: %q %q %v", status, message, found)
	}
	if grpcOK(body, nil) {
		t.Fatal("non-zero grpc trailer status accepted")
	}
}

func TestGrokProtocolFlowNormalizesSavedLanguage(t *testing.T) {
	saved := NewBrowserProfile()
	saved.NavigatorLanguage = "zh-CN"
	saved.NavigatorLanguages = []string{"zh-CN", "zh"}
	saved.AcceptLanguage = "zh-CN,zh;q=0.9"
	saved.TimezoneIANA = "Asia/Shanghai"
	want := saved
	want.NavigatorLanguage = "en-US"
	want.NavigatorLanguages = []string{"en-US", "en"}
	want.AcceptLanguage = "en-US,en;q=0.9"
	solver := &HTTPGrokBrowserSolver{}
	flow, err := NewGrokProtocolFlow(GrokConfig{BrowserProfile: &saved}, solver)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	for name, got := range map[string]BrowserProfile{
		"saved flow":      flow.BrowserProfile(),
		"protocol client": flow.Client.BrowserProfile(),
		"browser worker":  *solver.Profile,
	} {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s changed account identity or did not normalize the language: got %+v want %+v", name, got, want)
		}
	}
	if saved.NavigatorLanguage != "zh-CN" || saved.NavigatorLanguages[0] != "zh-CN" || saved.AcceptLanguage != "zh-CN,zh;q=0.9" {
		t.Fatal("normalizing the Grok flow mutated the caller's saved profile")
	}
}
