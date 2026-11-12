package register

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdobeRegistrationDiagnosticsAllowlist(t *testing.T) {
	var stages []string
	progress := func(stage string) error { stages = append(stages, stage); return nil }
	result := adobeBrowserResult{Stage: "needs_manual", Code: "human_verification_required", SubmissionStarted: true,
		Diagnostics: adobeRegistrationDiagnostics{VerificationSource: "visible_challenge_frame", VerificationNetworkCode: "captcha_required"}}
	if err := adobeVerificationProgress(result, "", progress); err != nil {
		t.Fatal(err)
	}
	want := []string{"adobe_verification_visible_challenge_frame", "adobe_captcha_required", "adobe_creation_submitted", "adobe_registration_needs_review"}
	if !reflect.DeepEqual(stages, want) {
		t.Fatalf("unexpected evidence %v", stages)
	}
	stages = nil
	result.Code = "PRIVATE_SECRET"
	result.SubmissionStarted = false
	result.Diagnostics = adobeRegistrationDiagnostics{VerificationSource: "PRIVATE_SECRET", VerificationNetworkCode: "PRIVATE_SECRET"}
	if err := adobeVerificationProgress(result, "", progress); err != nil || len(stages) != 0 {
		t.Fatal("untrusted diagnostics were logged")
	}
	if adobeRegistrationReviewHint("PRIVATE_SECRET") != "" {
		t.Fatal("unexpected hint")
	}
}

func TestAdobeChallengeEndsAttemptWithoutEmailPollingOrRepeatingSignup(t *testing.T) {
	var runs, releases, emails int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/browser/reservations":
			json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/browser/reservations/"):
			releases++
			json.NewEncoder(w).Encode(map[string]any{"status": "closed"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/adobe/run":
			runs++
			json.NewEncoder(w).Encode(map[string]any{"stage": "needs_manual", "code": "human_verification_required", "submission_started": true,
				"diagnostics": map[string]any{"verification_source": "visible_challenge_frame", "submission_started": true}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	flow := AdobeBrowserFlow{Client: server.Client()}
	cfg := AdobeConfig{WorkerURL: server.URL, Profile: map[string]any{}}
	var stages []string
	_, err := flow.Register(context.Background(), "fixture", Mailbox{Email: "fixture@example.test"}, cfg,
		func(context.Context, time.Time) (string, error) { emails++; return "", nil },
		func(stage string) error { stages = append(stages, stage); return nil })
	flow.Close()
	if err == nil || safeFailure(err) != "adobe: human_verification_required" {
		t.Fatalf("unexpected failure %v", err)
	}
	if runs != 1 || releases != 1 || emails != 0 {
		t.Fatalf("runs=%d releases=%d emails=%d", runs, releases, emails)
	}
	if stages[len(stages)-1] != "adobe_registration_needs_review" {
		t.Fatalf("wrong terminal stage %v", stages)
	}
}

func TestAdobeChallengeReviewHintSurvivesReloadWithoutMakingRegistrationRetryable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	var calls atomic.Int32
	factory := func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		return adobeTestMail{}, adobeTestFlow{calls: &calls, failure: fail("adobe", "human_verification_required")}, nil
	}
	engine := NewAdobeEngine(path, "test-key", factory, nil, nil)
	id, err := engine.Start(adobeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	waitAdobe(t, engine)
	engine = NewAdobeEngine(path, "test-key", factory, nil, nil)
	job := engine.Snapshot()["jobs"].([]any)[0].(map[string]any)
	if job["status"] != "registration_pending" || job["can_retry"] != false || job["can_retry_registration"] != false {
		t.Fatalf("challenge became retryable: %v", job)
	}
	if !strings.Contains(job["review_hint"].(string), "本次注册失败；缺少 Adobe 创建结果，已停止自动重试") {
		t.Fatal("missing actionable paused-state explanation")
	}
	if engine.RetrySavedResult(id) == nil || calls.Load() != 1 {
		t.Fatal("challenge retry replayed signup")
	}
}
