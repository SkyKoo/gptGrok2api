package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func TestModelObserverOnlyStructuralMetadata(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"message":{"author":{"role":"assistant"},"metadata":{"model_slug":"gpt-6"}}}`, "gpt-6"},
		{`{"message":{"author":{"role":"user"},"metadata":{"model_slug":"gpt-fake"}}}`, ""},
		{`{"message":{"author":{"role":"assistant"},"content":{"model_slug":"gpt-fake","parts":["gpt-fake"]}}}`, ""},
		{`{"p":"/message/metadata/model_slug","o":"replace","v":"gpt-6"}`, "gpt-6"},
		{`{"p":"","o":"patch","v":[{"p":"/message/metadata","v":{"model_slug":"gpt-6"}}]}`, "gpt-6"},
		{`{"v":{"message":{"author":{"role":"assistant"},"metadata":{"model_slug":"gpt-6"}}}}`, "gpt-6"},
		{`{"p":"/message/content/parts/0","v":{"model_slug":"gpt-fake"}}`, ""},
		{`{"p":"/message/metadata/model_slug","v":"Bearer secret"}`, ""},
	} {
		var v any
		_ = json.Unmarshal([]byte(tc.raw), &v)
		got := ""
		notifyUpstreamModel(WithModelObserver(context.Background(), func(s string) { got = s }), v)
		if got != tc.want {
			t.Fatalf("%s: %s", tc.raw, got)
		}
	}
}
func TestOnlyDefiniteModelRejection(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{404, `{"error":{"code":"model_not_found"}}`, true},
		{400, `{"detail":{"code":"unsupported_model"}}`, true},
		{404, `{"error":{"code":"file_not_found"}}`, false},
		{400, `{"message":"model_not_found"}`, false},
		{504, `{"error":{"code":"model_not_found"}}`, false},
	} {
		if IsModelRejected(&protocol.UpstreamError{Status: tc.status, Body: tc.body}) != tc.want {
			t.Fatalf("bad classification %+v", tc)
		}
	}
	if IsModelRejected(errors.New("model_not_found")) {
		t.Fatal("unstructured error accepted")
	}
}

func TestTextModelRejectionRequiresConversationEndpoint(t *testing.T) {
	for _, tc := range []struct {
		path, body string
		status     int
		want       bool
	}{
		{"/backend-api/conversation", `{"detail":"Model not found"}`, 404, true},
		{"/backend-api/f/conversation", `{"detail":"Model not found"}`, 404, true},
		{"/backend-api/f/conversation/prepare", `{"detail":"Model not found"}`, 400, true},
		{"/backend-api/files/file-test/uploaded", `{"detail":"Model not found"}`, 404, false},
		{"/backend-api/conversation/private-id", `{"detail":"Model not found"}`, 404, false},
		{"/backend-api/conversation", `{"detail":"Conversation not found"}`, 404, false},
		{"/backend-api/conversation", `{"detail":"Model not found"}`, 500, false},
		{"/backend-api/conversation", `{"message":"Model not found"}`, 404, false},
	} {
		t.Run(tc.path+tc.body+http.StatusText(tc.status), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com"+tc.path+"?secret=hidden", nil)
			err := readOpenAIHTTPError(&http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, "other stage")
			var up *protocol.UpstreamError
			if !errors.As(err, &up) || up.Endpoint != tc.path || up.Body != tc.body {
				t.Fatalf("lost HTTP diagnostic: %v", err)
			}
			if got := IsModelRejected(err); got != tc.want {
				t.Fatalf("classification=%v want=%v", got, tc.want)
			}
			raw, _ := json.Marshal(up)
			if strings.Contains(string(raw), "Endpoint") || strings.Contains(string(raw), "secret") {
				t.Fatal("internal endpoint leaked")
			}
		})
	}
	if IsModelRejected(&protocol.UpstreamError{Status: 404, Body: `{"detail":"Model not found"}`}) {
		t.Fatal("unscoped text rejection accepted")
	}
}

func TestChatDiagnosticRetainsCauseWithoutCredentials(t *testing.T) {
	detail := ChatErrorDetails(errors.New(`{"detail":"Model not found","token":"opaque-key","email":"person@example.test"}`), accounts.Account{Token: "opaque-key"})
	if !strings.Contains(detail.Message, "Model not found") || strings.Contains(detail.Message, "opaque-key") || strings.Contains(detail.Message, "person@example.test") || strings.HasPrefix(detail.Message, imageTerminalPrefix) {
		t.Fatalf("bad diagnostic: %s", detail.Message)
	}
}
