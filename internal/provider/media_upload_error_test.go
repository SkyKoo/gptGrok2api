package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func TestMediaUploadErrorsPreserveClassificationWithoutResponseSecrets(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		status                          int
		contentType, header, body, kind string
	}{
		{"challenge header", 403, "text/html", "challenge", "SECRET signed URL https://private.invalid/?token=SECRET", "verification_required"},
		{"challenge html", 403, "text/html", "", "<title>Just a moment...</title> SECRET", "verification_required"},
		{"generic denial", 403, "application/json", "", `{"message":"SECRET"}`, "upload_forbidden"},
		{"region denial", 403, "text/html", "", "not available in your region SECRET", "region_restricted"},
		{"expired login", 401, "application/json", "", `{"token":"SECRET"}`, "authentication_required"},
		{"upload quota", 429, "application/json", "", `{"quota":"SECRET"}`, "upload_rate_limited"},
		{"too large", 413, "text/html", "", "SECRET", "upload_too_large"},
		{"wrong mime", 415, "text/plain", "", "SECRET", "invalid_upload"},
		{"upstream failure", 503, "text/plain", "", "SECRET", "upstream_unavailable"},
		{"invalid json", 200, "text/plain", "", "SECRET", "invalid_upload_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Cf-Mitigated", tc.header)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer up.Close()
			m := NewMedia(up.Client(), "", "", up.URL, "", 0)
			_, _, err := m.Upload(context.Background(), accounts.Account{Token: "session-secret"}, "private-name.png", "image/png", base64.StdEncoding.EncodeToString([]byte("test")))
			if calls != 1 || err == nil {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			d := MediaUploadDiagnostics(err)
			if d["upload_error_kind"] != tc.kind {
				t.Fatalf("diagnostics=%v", d)
			}
			var upstream *protocol.UpstreamError
			if !errors.As(err, &upstream) {
				t.Fatal("HTTP status lost")
			}
			want := tc.status
			if want == 200 {
				want = 502
			}
			if upstream.Status != want {
				t.Fatalf("status=%d want=%d", upstream.Status, want)
			}
			diagnosticJSON, _ := json.Marshal(d)
			for _, value := range []string{err.Error(), upstream.Body, string(diagnosticJSON)} {
				for _, secret := range []string{"SECRET", "private.invalid", "session-secret", "private-name.png", "<title>"} {
					if strings.Contains(value, secret) {
						t.Fatalf("secret exposed: %q", secret)
					}
				}
			}
			if !strings.Contains(err.Error(), "file_upload") {
				t.Fatal("quota group lost")
			}
		})
	}
}

func TestMediaUploadTransportErrorsAreSafe(t *testing.T) {
	for _, tc := range []struct {
		err  error
		kind string
	}{
		{fmt.Errorf("POST https://secret.invalid/?token=SECRET failed"), "upload_network_error"},
		{fmt.Errorf("SECRET: %w", context.DeadlineExceeded), "upload_timeout"},
	} {
		err := safeMediaUploadError(tc.err)
		if strings.Contains(err.Error(), "SECRET") || MediaUploadDiagnostics(err)["upload_error_kind"] != tc.kind {
			t.Fatalf("unsafe error %v", err)
		}
	}
}
