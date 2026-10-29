package register

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExplicitRefusalsRemainManualAcrossLegacyAndNewStatuses(t *testing.T) {
	for _, status := range []string{"504", "422"} {
		for reason, want := range map[string]string{"signup_unavailable": "upstream_signup_refused", "turnstile_incomplete": "human_verification_required", "email_invalid": "email_invalid", "cloudflare_challenge": "human_verification_required"} {
			j := compensationFixture(time.Now())
			j.Error = "send_code: browser_worker_rejected (HTTP " + status + ") operation=worker_error=" + reason
			planRegistrationCompensation(&j, nil, time.Now())
			if j.AutoRetry.State != "manual" || j.AutoRetry.Reason != want || j.AutoRetry.NextAttemptAt != "" {
				t.Fatalf("%s: %+v", j.Error, j.AutoRetry)
			}
			if transientRegistrationFailure(j.Error) {
				t.Fatal("explicit refusal scheduled again")
			}
		}
	}
}

func TestBrowserNetworkDiagnosticsRejectSecretsAndLimitEntries(t *testing.T) {
	events := []browserNetworkFailure{
		{"accounts.x.ai.evil", "http_error", 400},
		{"accounts.x.ai", "secret-password", 400},
		{"accounts.x.ai", "http_error", 200},
	}
	for i := 0; i < 100; i++ {
		events = append(events, browserNetworkFailure{"brunhild.challenges.cloudflare.com", "proxy_tunnel_failed", 0})
	}
	got := safeBrowserNetworkFailures(events)
	if strings.Contains(got, "secret") || strings.Contains(got, "evil") || strings.Count(got, "proxy_tunnel_failed") != 8 {
		t.Fatal(got)
	}
}

func TestWorkerNetworkEvidenceSurvivesTaskJournalWithoutSecrets(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 422, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"detail":{"stage":"turnstile","error":"turnstile_incomplete","diagnostic":{"network_failures":[{"host":"brunhild.challenges.cloudflare.com","kind":"proxy_tunnel_failed","status":0},{"host":"secret.invalid","kind":"http_error","status":403}]}}}`))}, nil
	})}
	solver := &HTTPGrokBrowserSolver{BaseURL: "http://worker.test", Client: client}
	_, err := solver.workerPost(context.Background(), "/v1/session/signup", map[string]any{}, "signup")
	var failure *Failure
	if !errors.As(err, &failure) || failure.Stage != "turnstile" || failure.HTTPStatus != 422 {
		t.Fatalf("wrong phase/status: %v", err)
	}
	if !strings.Contains(safeFailure(err), "network_failures=brunhild.challenges.cloudflare.com|proxy_tunnel_failed|0") || strings.Contains(safeFailure(err), "secret") {
		t.Fatal(err)
	}
	j := compensationFixture(time.Now())
	j.Error = safeFailure(err)
	planRegistrationCompensation(&j, err, time.Now())
	if j.AutoRetry.State != "manual" {
		t.Fatal("challenge error must not schedule another registration")
	}
}

func TestGrokSnapshotExposesManualReviewInsteadOfHidingRetryState(t *testing.T) {
	for _, errorText := range []string{"task_timeout", "send_code: browser_worker_rejected (HTTP 504) operation=worker_error=signup_unavailable"} {
		job := compensationFixture(time.Now())
		job.Error = errorText
		engine := &GrokEngine{jobs: []RegistrationJob{job}, active: make(map[string]*activeRegistration)}
		snapshot := engine.Snapshot()
		jobs := snapshot["jobs"].([]any)
		retry := jobs[0].(map[string]any)["auto_retry"].(map[string]any)
		if retry["state"] != "manual" || retry["next_attempt_at"] != "" {
			t.Fatalf("misleading Grok schedule: %+v", retry)
		}
		if engine.jobs[0].AutoRetry.State != "waiting" {
			t.Fatal("snapshot modified stored task")
		}
		if strings.Contains(errorText, "signup_unavailable") && retry["reason"] != "upstream_signup_refused" {
			t.Fatal(retry)
		}
	}
}
