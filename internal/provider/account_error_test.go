package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAccountEndpointDiagnostics(t *testing.T) {
	for _, code := range []int{401, 403, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "600")
				w.WriteHeader(code)
				fmt.Fprint(w, `{"message":"backend busy","access_token":"opaque-secret","cookie":"private-cookie","email":"person@example.test","url":"https://proxy:password@example.test/path?token=secret"}`)
			}))
			defer upstream.Close()
			client := NewOpenAIAccountClient(upstream.URL, "", upstream.Client(), nil)
			_, err := client.RefreshAccount(context.Background(), map[string]any{"access_token": "opaque-secret"})
			var detail *AccountEndpointError
			if !errors.As(err, &detail) || detail.Endpoint != "/backend-api/me" || detail.Stage != "user_info" || detail.Status != code {
				t.Fatalf("missing diagnostic: %v", err)
			}
			if detail.Retryable != (code == 429 || code == 503) || detail.RetryAfterSeconds != 600 {
				t.Fatal("incorrect retry classification")
			}
			if errors.Is(err, ErrInvalidAccessToken) != (code == 401) {
				t.Fatal("lost auth sentinel")
			}
			raw, _ := json.Marshal(detail)
			for _, secret := range []string{"opaque-secret", "private-cookie", "person@example.test", "proxy:password"} {
				if strings.Contains(string(raw), secret) || strings.Contains(err.Error(), secret) {
					t.Fatal("credential leaked")
				}
			}
			if !strings.Contains(detail.Message, "backend busy") {
				t.Fatal("lost original reason")
			}
		})
	}
	timeout := &url.Error{Op: "Get", URL: "https://user:pass@example.test/?token=private", Err: context.DeadlineExceeded}
	detail := accountEndpointError("/backend-api/conversation/init", 0, "", timeout, nil)
	if !detail.Retryable || detail.Kind != "timeout" || !errors.Is(detail, context.DeadlineExceeded) || strings.Contains(detail.Message, "pass") {
		t.Fatalf("bad transport diagnostic: %v", detail)
	}
	cancelled := accountEndpointError("/backend-api/me", 0, "", context.Canceled, nil)
	if cancelled.Retryable {
		t.Fatal("cancellation must not retry")
	}
}
