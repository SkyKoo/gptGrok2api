package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
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

func TestGrokOAuthWorkerDiagnosticPreservesSafeCauseAndSubmissionState(t *testing.T) {
	old := log.Writer()
	defer log.SetOutput(old)
	var output bytes.Buffer
	log.SetOutput(&output)
	result := oauth.BrowserDeviceResult{
		Stage: "page_observation", Status: "uncertain", Code: "oauth_page_observation_failed",
		Detail: "observation_failed", Operation: "button_enabled", ExceptionType: "navigation",
		PagePath: "/oauth2/device/consent", ConsentState: "attempted",
	}
	logGrokOAuthWorkerDiagnostic("task_test", result)
	for _, want := range []string{"detail=observation_failed", "operation=button_enabled", "exception_type=navigation", "consent_state=attempted", "consent_submitted=true"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("lost diagnostic %q: %s", want, output.String())
		}
	}
	output.Reset()
	result.Operation = "SSO-SECRET"
	result.ExceptionType = "https://accounts.x.ai/?user_code=DEVICE-SECRET"
	result.ConsentState = "PRIVATE-STATE"
	result.Detail = "EMAIL-SECRET"
	logGrokOAuthWorkerDiagnostic("task_test", result)
	for _, secret := range []string{"SSO-SECRET", "DEVICE-SECRET", "PRIVATE-STATE", "EMAIL-SECRET"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("worker diagnostic leaked %q", secret)
		}
	}
	for _, want := range []string{"operation=unknown", "exception_type=unknown", "consent_state=unknown"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing fallback %q: %s", want, output.String())
		}
	}
}

func TestGrokOAuthGrantDeadlineRetainsBrowserFailure(t *testing.T) {
	for _, code := range []string{"oauth_worker_result_unknown", "oauth_page_observation_failed", "oauth_consent_click_timeout", "oauth_callback_timeout", "oauth_authorization_timeout"} {
		outcome := oauth.BrowserDeviceResult{Status: "uncertain", Code: code, ConsentState: "attempted"}
		for _, deadline := range []error{errors.New("oauth_deadline"), fmt.Errorf("token query: %w", context.DeadlineExceeded)} {
			if got := grokOAuthGrantError(outcome, deadline); got == nil || got.Error() != code {
				t.Fatalf("lost original failure %q: %v", code, got)
			}
		}
		for _, failure := range []string{"oauth_cancelled", "oauth_account_changed", "oauth_identity_mismatch", "oauth_storage_failed", "xAI OAuth returned HTTP 403"} {
			err := errors.New(failure)
			if got := grokOAuthGrantError(outcome, err); got != err {
				t.Fatalf("browser error replaced %q: %v", failure, got)
			}
		}
	}
	for _, outcome := range []oauth.BrowserDeviceResult{
		{Status: "approved"}, {Status: "uncertain"}, {Status: "uncertain", Code: "SECRET"},
	} {
		if got := grokOAuthGrantError(outcome, context.DeadlineExceeded); got.Error() != "oauth_deadline" {
			t.Fatalf("unexpected deadline fallback: %v", got)
		}
	}
}

func TestGrokOAuthErrorCodePreservesBrowserStartupCode(t *testing.T) {
	for _, code := range []string{"browser_launch_timeout", "browser_launch_proxy_error", "browser_launch_license_denied"} {
		if got := grokOAuthErrorCode(&browserqueue.StartupError{Code: code}); got != code {
			t.Fatalf("grokOAuthErrorCode(%q) = %q", code, got)
		}
	}
}
