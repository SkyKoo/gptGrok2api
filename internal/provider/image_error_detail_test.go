package provider

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func TestImageErrorDetailPreservesCauseAndRedactsCredentials(t *testing.T) {
	const opaque = "opaque-account-credential-for-test"
	reason := `We experienced an error when generating images.
Authorization: Bearer header-secret
Cookie: session=cookie-secret; other=another-cookie-secret
headers Cookie: first=inline-cookie; second=inline-cookie-two
{"token":"conduit-secret", "access_token":"access-secret", "refresh_token":"refresh-secret", "password":"password with spaces", "Cookie":"session=json-cookie; x=two"}
Bearer inline-secret Basic c2VjcmV0
opaque-account-credential-for-test eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.signature sk-abcdefghijklmn sess-abcdefghijklm
person@example.test https://user:pass@example.test/signed/path?sig=query-secret
Reference: data:image/png;base64,aGVsbG8=
` + strings.Repeat("diagnostic context. ", 650) + "FINAL_CAUSE: temporary image renderer failed."
	for _, value := range []any{
		map[string]any{"error": map[string]any{"message": reason}},
		map[string]any{"error": reason},
		map[string]any{"author": map[string]any{"role": "tool"}, "metadata": map[string]any{"is_error": true}, "content": map[string]any{"content_type": "text", "parts": []any{reason}}},
	} {
		err := openAIImageTerminalError(value, opaque)
		detail := ImageErrorDetails(fmt.Errorf("wrapped: %w", err))
		if !IsImageTerminalError(err) || detail.Truncated || !strings.HasSuffix(detail.Message, "FINAL_CAUSE: temporary image renderer failed.") || len(detail.Message) < 8<<10 {
			t.Fatalf("diagnostic lost: truncated=%v length=%d", detail.Truncated, len(detail.Message))
		}
		if strings.Contains(err.Error(), "FINAL_CAUSE") || len([]rune(err.Error())) > len(imageTerminalPrefix)+161 {
			t.Fatal("API error no longer a short summary")
		}
		var upstream *protocol.UpstreamError
		if !errors.As(err, &upstream) || upstream.Status != 422 || upstream.Body != err.Error() {
			t.Fatal("error classification changed")
		}
		for _, secret := range []string{opaque, "header-secret", "cookie-secret", "inline-cookie", "inline-cookie-two", "conduit-secret", "another-cookie-secret", "access-secret", "refresh-secret", "password with spaces", "json-cookie", "inline-secret", "c2VjcmV0", "eyJhbGci", "sk-abcdefghijklmn", "sess-abcdefghijklm", "person@example.test", "query-secret", "user:pass", "aGVsbG8="} {
			if strings.Contains(detail.Message, secret) || strings.Contains(err.Error(), secret) {
				t.Errorf("secret leaked: %s", secret)
			}
		}
		if strings.Contains(detail.Message, "[REDACTED]]") {
			t.Fatal("redaction corrupts existing placeholder")
		}
	}
}

func TestImageErrorDetailLimitIsExplicitAndUTF8Safe(t *testing.T) {
	secret := "opaque-secret-crossing-limit"
	reason := strings.Repeat("错", maxImageErrorDetailRunes-4) + secret + strings.Repeat("误", 100)
	detail := imageErrorDetail(reason, secret)
	if !detail.Truncated || !utf8.ValidString(detail.Message) || !strings.Contains(detail.Message, "diagnostic truncated") || strings.Contains(detail.Message, "opaque") {
		t.Fatal("unsafe detail limit")
	}
	// Short/legacy error text retains its original contract and has no invented reason.
	if ImageErrorDetails(errors.New("network timeout")).Message != "" {
		t.Fatal("invented upstream diagnostic")
	}
	if got := openAIImageTerminalError(map[string]any{"error": "failed"}); got.Error() != imageTerminalPrefix+"failed" {
		t.Fatal(got)
	}
}
