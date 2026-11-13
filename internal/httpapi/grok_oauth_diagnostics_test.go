package httpapi

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
)

func TestLogGrokOAuthDiagnosticAllowListsWorkerFields(t *testing.T) {
	old := log.Writer()
	defer log.SetOutput(old)
	var output bytes.Buffer
	log.SetOutput(&output)

	logGrokOAuthDiagnosticWithNetwork(
		"task_public",
		"consent_callback",
		"uncertain",
		"oauth_callback_timeout",
		"PRIVATE-DETAIL",
		"https://accounts.x.ai/oauth2/device?user_code=DEVICE-SECRET",
		181200,
		true,
		3,
		"ERR_TUNNEL_CONNECTION_FAILED,TIMED_OUT",
	)
	value := output.String()
	if !strings.Contains(value, "stage=consent_callback") || !strings.Contains(value, "code=oauth_callback_timeout") {
		t.Fatalf("missing fixed diagnostics: %s", value)
	}
	for _, secret := range []string{"PRIVATE-DETAIL", "DEVICE-SECRET", "https://accounts.x.ai"} {
		if strings.Contains(value, secret) {
			t.Fatalf("diagnostic leaked %q: %s", secret, value)
		}
	}
	if !strings.Contains(value, "detail=") || !strings.Contains(value, "page_path=") {
		t.Fatalf("unallowlisted fields were not reduced: %s", value)
	}
	if !strings.Contains(value, "elapsed_ms=181200") || !strings.Contains(value, "network_failures=3") || !strings.Contains(value, "network_error_codes=ERR_TUNNEL_CONNECTION_FAILED,TIMED_OUT") {
		t.Fatalf("bounded diagnostics were lost: %s", value)
	}

	output.Reset()
	logGrokOAuthDiagnosticWithNetwork("task_public", "browser_context", "uncertain", "oauth_worker_result_unknown", "PRIVATE-DETAIL", "", 1200, false, 1, "ERR_SECRET;PRIVATE")
	value = output.String()
	if strings.Contains(value, "ERR_SECRET") || !strings.Contains(value, "network_error_codes=") {
		t.Fatalf("network diagnostic was not reduced: %s", value)
	}

	output.Reset()
	logGrokOAuthDiagnostic("task_public", "browser_exception", "uncertain", "oauth_worker_result_unknown", "browser_exception", "", 1200, false)
	value = output.String()
	if !strings.Contains(value, "stage=browser_exception") || !strings.Contains(value, "detail=browser_exception") {
		t.Fatalf("browser exception stage was reduced: %s", value)
	}
}

func TestGrokOAuthErrorCodePreservesBrowserStartupCode(t *testing.T) {
	for _, code := range []string{"browser_launch_timeout", "browser_launch_proxy_error", "browser_launch_license_denied"} {
		if got := grokOAuthErrorCode(&browserqueue.StartupError{Code: code}); got != code {
			t.Fatalf("grokOAuthErrorCode(%q) = %q", code, got)
		}
	}
}
