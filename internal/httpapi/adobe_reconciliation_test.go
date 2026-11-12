package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// Alter only the test journal's reservation timestamp. Relogin/UpdatedAt must
// not reset this original task age, and tests must not wait thirty real minutes.
func ageAdobeReconciliationFixture(t *testing.T, s *Server, id string, age time.Duration) {
	t.Helper()
	editAdobeReconciliationFixture(t, s, id, func(a *adobe.Account) { a.LastUsedAt = time.Now().Add(-age).Unix() })
}

func editAdobeReconciliationFixture(t *testing.T, s *Server, id string, edit func(*adobe.Account)) {
	t.Helper()
	path := filepath.Join(s.cfg.DataDir, "adobe_accounts.json.enc")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := firstNonEmpty(s.cfg.AdminKey, s.cfg.APIKey, "gptgrok2api")
	plain, err := adobe.Open(secret, raw)
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]json.RawMessage
	if err = json.Unmarshal(plain, &disk); err != nil {
		t.Fatal(err)
	}
	var accounts []adobe.Account
	if err = json.Unmarshal(disk["accounts"], &accounts); err != nil {
		t.Fatal(err)
	}
	for i := range accounts {
		if accounts[i].ID == id {
			edit(&accounts[i])
		}
	}
	disk["accounts"], err = json.Marshal(accounts)
	if err != nil {
		t.Fatal(err)
	}
	plain, err = json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = adobe.Seal(secret, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func seedAdobeReconciliationImage(t *testing.T, s *Server, age time.Duration) (adobe.Account, *imageTaskState) {
	t.Helper()
	a := adobeFixtureAccount(t, s)
	key := "image-auto-reconcile"
	if _, err := s.adobeStore.Reserve(key); err != nil {
		t.Fatal(err)
	}
	if err := s.adobeStore.MarkUnknown(key); err != nil {
		t.Fatal(err)
	}
	task := &imageTaskState{ID: "auto-reconcile", OwnerID: "owner", Status: "error", Error: "original upstream timeout", Model: "gemini-3.1-flash-image", Mode: "generate", N: 1, Size: "1024x1024", CreatedAt: time.Now().Add(-age).UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339), Route: &mediaRoute{Channel: "adobe", AccountID: a.ID, JobKey: key, Stage: "submit_started"}}
	s.imageTasks[imageTaskKey(task.OwnerID, task.ID)] = task
	if err := s.saveImageTaskLocked(task); err != nil {
		t.Fatal(err)
	}
	ageAdobeReconciliationFixture(t, s, a.ID, age)
	a, err := s.adobeStore.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a, task
}

func adobeReconciliationQuotaClient(t *testing.T, calls *atomic.Int32, fail bool) *adobe.Client {
	t.Helper()
	failPath := ""
	if fail {
		failPath = "*"
	}
	return adobeReconciliationQuotaClientFailingPath(t, calls, failPath)
}

func adobeReconciliationQuotaClientFailingPath(t *testing.T, calls *atomic.Int32, failPath string) *adobe.Client {
	t.Helper()
	return adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		status, body := http.StatusOK, `{"total":{"quota":{"total":10,"used":0,"available":10}}}`
		switch r.URL.Path {
		case "/v1/credits/balance":
			if r.Method != http.MethodGet {
				t.Error("balance was not read-only GET")
			}
		case "/v1/feature-auth-profile":
			profiles := []any{}
			for _, model := range adobe.FreeModels() {
				profiles = append(profiles, map[string]any{"featureId": model.FeatureID, "authorizationResult": map[string]any{"result": "authorized"}, "selectedResource": map[string]any{"resourceType": "firefly_complimentary_generations", "effectiveQuotaReferenceId": "fixture-pool", "computedCost": 1}})
			}
			raw, _ := json.Marshal(map[string]any{"featuresAuthorization": profiles, "effectiveQuotas": []any{map[string]any{"effectiveQuotaReferenceId": "fixture-pool", "resourceType": "firefly_complimentary_generations", "provisionedQuota": 3, "consumedQuota": 0}}})
			body = string(raw)
		default:
			t.Errorf("reconciliation contacted a non-quota endpoint: %s %s", r.Method, r.URL.Path)
			return nil, errors.New("unexpected reconciliation request")
		}
		if failPath == "*" || r.URL.Path == failPath {
			status, body = http.StatusUnauthorized, `{}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})})
}

func TestAdobeAutomaticReconciliationReleasesExpiredUnknownAndPreservesHistory(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "terminal_record"
		if missing {
			name = "missing_record"
		}
		t.Run(name, func(t *testing.T) {
			s := imageTaskTestServer(t)
			a, task := seedAdobeReconciliationImage(t, s, 31*time.Minute)
			if missing {
				delete(s.imageTasks, imageTaskKey(task.OwnerID, task.ID))
				if err := os.Remove(filepath.Join(s.imageTasksDir(), imageTaskKey(task.OwnerID, task.ID)+".json")); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			s.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
			s.reconcileAdobeSubmissions(context.Background(), time.Now())
			got, err := s.adobeStore.Get(a.ID)
			if err != nil || got.InFlight != "" || got.Status == "submission_unknown" || got.SettlementUntil <= time.Now().Unix() {
				t.Fatalf("did not release locally after grace: %+v %v", got, err)
			}
			if calls.Load() != 2 {
				t.Fatalf("query count=%d", calls.Load())
			}
			if task.Error != "original upstream timeout" || task.Status != "error" {
				t.Fatal("rewrote original result")
			}
			records, err := s.adobeStore.SubmissionResolutions(a.ID)
			wantState := "terminal"
			if missing {
				wantState = "missing"
			}
			if err != nil || len(records) != 1 || records[0].ReviewedBy != "system" || records[0].Outcome != "unknown" || records[0].QuotaStatus != "known" || records[0].TaskRecordState != wantState {
				t.Fatalf("wrong audit: %+v %v", records, err)
			}
			fresh := NewWithWorkers(s.cfg, false)
			fresh.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
			fresh.reconcileAdobeSubmissions(context.Background(), time.Now())
			if fresh.resumeAdobeImageTask(task) || calls.Load() != 2 {
				t.Fatal("restart or resolved task reissued upstream work")
			}
			records, _ = fresh.adobeStore.SubmissionResolutions(a.ID)
			if len(records) != 1 {
				t.Fatal("duplicate release audit")
			}
			if !missing {
				saved := fresh.imageTasks[imageTaskKey(task.OwnerID, task.ID)]
				if saved == nil || saved.Error != task.Error || saved.Status != "error" {
					t.Fatal("original history missing after restart")
				}
			}
		})
	}
}

func TestAdobeAutomaticReconciliationWaitsAndPersistsBackoff(t *testing.T) {
	s := imageTaskTestServer(t)
	a, _ := seedAdobeReconciliationImage(t, s, 29*time.Minute)
	var calls atomic.Int32
	s.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
	s.reconcileAdobeSubmissions(context.Background(), time.Now())
	got, _ := s.adobeStore.Get(a.ID)
	if got.InFlight != a.InFlight || got.TaskReconciliation == nil || got.TaskReconciliation.State != "waiting" || calls.Load() != 2 {
		t.Fatalf("unexpected waiting state: %+v", got)
	}
	fresh := NewWithWorkers(s.cfg, false)
	fresh.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
	fresh.reconcileAdobeSubmissions(context.Background(), time.Now())
	if calls.Load() != 2 {
		t.Fatal("restart lost reconciliation backoff")
	}
	records, _ := fresh.adobeStore.SubmissionResolutions(a.ID)
	if len(records) != 0 {
		t.Fatal("released before grace")
	}
}

func TestAdobeAutomaticReconciliationCannotReleaseBusyOrUnreadableTasks(t *testing.T) {
	for _, condition := range []string{"running", "recovering", "duplicate", "maintenance", "paused", "image_journal_error", "video_journal_error", "handle_corrupt"} {
		t.Run(condition, func(t *testing.T) {
			s := imageTaskTestServer(t)
			a, task := seedAdobeReconciliationImage(t, s, 31*time.Minute)
			switch condition {
			case "running":
				task.Status = "running"
			case "recovering":
				task.recovering = true
			case "duplicate":
				copy := *task
				copy.ID = "duplicate"
				s.imageTasks[imageTaskKey(copy.OwnerID, copy.ID)] = &copy
			case "maintenance":
				if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "cfm-maintenance"), []byte("test"), 0600); err != nil {
					t.Fatal(err)
				}
			case "paused":
				s.PauseDeployment(true)
			case "image_journal_error":
				s.imageTaskLoadErr = errors.New("test load failure")
			case "video_journal_error":
				s.videoLoadErr = errors.New("test load failure")
			case "handle_corrupt":
				if err := s.adobeStore.SaveJob(a.InFlight, adobe.Job{Kind: "image", Model: task.Model, Status: "SUBMISSION_UNKNOWN"}); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256([]byte(a.InFlight))
				if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "adobe_jobs", hex.EncodeToString(sum[:])+".json.enc"), []byte("corrupt fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			s.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
			s.reconcileAdobeSubmissions(context.Background(), time.Now())
			got, _ := s.adobeStore.Get(a.ID)
			records, _ := s.adobeStore.SubmissionResolutions(a.ID)
			if got.InFlight != a.InFlight || got.Status != "submission_unknown" || len(records) != 0 || calls.Load() != 0 {
				t.Fatalf("unsafe release or request under %s", condition)
			}
		})
	}
}

func TestAdobeAutomaticReconciliationQuotaFailureDoesNotClaimRefund(t *testing.T) {
	s := imageTaskTestServer(t)
	a, task := seedAdobeReconciliationImage(t, s, 31*time.Minute)
	var calls atomic.Int32
	s.adobeClient = adobeReconciliationQuotaClient(t, &calls, true)
	s.reconcileAdobeSubmissions(context.Background(), time.Now())
	got, _ := s.adobeStore.Get(a.ID)
	records, _ := s.adobeStore.SubmissionResolutions(a.ID)
	if got.InFlight != "" || got.Quota.Status != "error" || task.Error != "original upstream timeout" {
		t.Fatalf("quota error confused with generation result: %+v", got)
	}
	if calls.Load() != 2 || len(records) != 1 || records[0].Outcome != "unknown" || records[0].QuotaStatus != "error" {
		t.Fatalf("quota failure not preserved in automatic audit: %+v", records)
	}
}

func TestAdobeAutomaticReconciliationKnownImageHandleOnlyPollsAndDownloadsOnce(t *testing.T) {
	s := imageTaskTestServer(t)
	a, task := seedAdobeReconciliationImage(t, s, 31*time.Minute)
	if err := s.adobeStore.SaveJob(a.InFlight, adobe.Job{Kind: "image", Model: task.Model, PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: "SUBMITTED"}); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var closeOnce sync.Once
	t.Cleanup(func() { closeOnce.Do(func() { close(release) }) })
	var polls, downloads, submits atomic.Int32
	output := adobeMediaPNG(t, 1024, 1024)
	s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			submits.Add(1)
		}
		body := `{}`
		header := http.Header{"Content-Type": []string{"application/json"}}
		switch {
		case strings.Contains(r.URL.Path, "/v2/jobs/result/"):
			if polls.Add(1) == 1 {
				close(started)
			}
			<-release
			body = `{"status":"COMPLETED","outputs":[{"image":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/fixture.png"}}]}`
		case r.URL.Hostname() == "pre-signed-firefly-prod.s3-accelerate.amazonaws.com":
			downloads.Add(1)
			body = string(output)
			header.Set("Content-Type", "image/png")
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("credentials leaked to download")
			}
		default:
			t.Errorf("unexpected reconciliation URL: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})})
	s.reconcileAdobeSubmissions(context.Background(), time.Now())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("did not resume saved handle")
	}
	s.reconcileAdobeSubmissions(context.Background(), time.Now().Add(6*time.Minute))
	if polls.Load() != 1 || submits.Load() != 0 {
		t.Fatal("parallel check replayed existing task")
	}
	closeOnce.Do(func() { close(release) })
	got := waitAdobeImage(t, s, task.ID)
	if got.Status != "success" || len(got.Data) != 1 || downloads.Load() != 1 || polls.Load() != 1 || submits.Load() != 0 {
		t.Fatalf("recovery did not deliver original result: %+v polls=%d downloads=%d submits=%d", got, polls.Load(), downloads.Load(), submits.Load())
	}
	account, _ := s.adobeStore.Get(a.ID)
	if account.InFlight != "" {
		t.Fatal("completed known task remained occupied")
	}
}

func TestAdobeAutomaticReconciliationDiscardsObservationAfterConcurrentRelogin(t *testing.T) {
	s := imageTaskTestServer(t)
	a, _ := seedAdobeReconciliationImage(t, s, 31*time.Minute)
	started, release := make(chan struct{}), make(chan struct{})
	var closeOnce sync.Once
	t.Cleanup(func() { closeOnce.Do(func() { close(release) }) })
	s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/credits/balance" {
			close(started)
			<-release
		}
		return &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})})
	done := make(chan struct{})
	go func() { defer close(done); s.reconcileAdobeSubmissions(context.Background(), time.Now()) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("quota read did not begin")
	}
	s.adobeStore.SetBrowserRefresher(func(_ context.Context, current adobe.Account) (adobe.Account, error) {
		current.MediaSessionRefreshed = true
		current.MediaSessionStatus = "complete"
		return current, nil
	})
	fresh, err := s.adobeStore.Relogin(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	closeOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reconciliation did not finish")
	}
	got, _ := s.adobeStore.Get(a.ID)
	records, _ := s.adobeStore.SubmissionResolutions(a.ID)
	if got.Revision != fresh.Revision || got.InFlight != a.InFlight || got.Status != "submission_unknown" || len(records) != 0 {
		t.Fatal("stale observation released a renewed account")
	}
}

func TestAdobeAutomaticReconciliationKnownHandleMissingTaskPersistsBackoff(t *testing.T) {
	s := imageTaskTestServer(t)
	a, task := seedAdobeReconciliationImage(t, s, 31*time.Minute)
	delete(s.imageTasks, imageTaskKey(task.OwnerID, task.ID))
	if err := s.adobeStore.SaveJob(a.InFlight, adobe.Job{Kind: "image", Model: task.Model, PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: "SUBMITTED"}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
	s.reconcileAdobeSubmissions(context.Background(), time.Now())
	got, _ := s.adobeStore.Get(a.ID)
	if got.TaskReconciliation == nil || got.TaskReconciliation.State != "retry_wait" || got.TaskReconciliation.LastError != "task_record_missing" || got.InFlight != a.InFlight || calls.Load() != 0 {
		t.Fatalf("missing owner could not record recovery backoff: %+v", got.TaskReconciliation)
	}
}

func TestAdobeAutomaticReconciliationQuotaRetryAfterReleaseIsBoundedAndIndependent(t *testing.T) {
	s := imageTaskTestServer(t)
	if _, err := s.store.UpdateConfig("adobe_maintenance", map[string]any{"enabled": false}); err != nil {
		t.Fatal(err)
	}
	a, _ := seedAdobeReconciliationImage(t, s, 31*time.Minute)
	var calls atomic.Int32
	s.adobeClient = adobeReconciliationQuotaClient(t, &calls, true)
	s.refreshDueAdobeAccounts(context.Background())
	got, _ := s.adobeStore.Get(a.ID)
	if got.InFlight != "" || calls.Load() != 2 {
		t.Fatal("credential maintenance toggle disabled task housekeeping")
	}
	editAdobeReconciliationFixture(t, s, a.ID, func(a *adobe.Account) { a.SettlementUntil = time.Now().Add(-time.Second).Unix() })
	s.refreshDueAdobeAccounts(context.Background())
	if calls.Load() != 2 {
		t.Fatal("retried failed quota before persisted backoff")
	}
	editAdobeReconciliationFixture(t, s, a.ID, func(a *adobe.Account) {
		a.Quota.CheckedAt = time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339Nano)
	})
	s.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
	s.refreshDueAdobeAccounts(context.Background())
	got, _ = s.adobeStore.Get(a.ID)
	if calls.Load() != 4 || got.Quota.Status != "known" {
		t.Fatal("automatic cleanup did not retry failed quota after backoff")
	}
	editAdobeReconciliationFixture(t, s, a.ID, func(a *adobe.Account) {
		a.LastUsedAt = time.Now().Add(time.Second).Unix()
		a.Quota.Status = "error"
		a.Quota.CheckedAt = time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339Nano)
	})
	s.refreshDueAdobeAccounts(context.Background())
	if calls.Load() != 4 {
		t.Fatal("old resolution changed housekeeping for a newer task")
	}
}

func TestAdobeAutomaticReconciliationRetriesFailedGenerationPermissionsAfterRelease(t *testing.T) {
	s := imageTaskTestServer(t)
	if _, err := s.store.UpdateConfig("adobe_maintenance", map[string]any{"enabled": false}); err != nil {
		t.Fatal(err)
	}
	a, _ := seedAdobeReconciliationImage(t, s, 31*time.Minute)
	var calls atomic.Int32
	s.adobeClient = adobeReconciliationQuotaClientFailingPath(t, &calls, "/v1/feature-auth-profile")
	s.refreshDueAdobeAccounts(context.Background())
	got, _ := s.adobeStore.Get(a.ID)
	if got.InFlight != "" || got.Quota.Generation == nil || got.Quota.Generation.Status != "error" || calls.Load() != 2 {
		t.Fatal("generation-permission error blocked local cleanup or was discarded")
	}
	editAdobeReconciliationFixture(t, s, a.ID, func(a *adobe.Account) { a.SettlementUntil = time.Now().Add(-time.Second).Unix() })
	s.refreshDueAdobeAccounts(context.Background())
	got, _ = s.adobeStore.Get(a.ID)
	if got.Quota.Status != "known" || got.Quota.Generation.Status != "error" || calls.Load() != 4 {
		t.Fatal("settlement query did not retain independent permission failure")
	}
	s.refreshDueAdobeAccounts(context.Background())
	if calls.Load() != 4 {
		t.Fatal("generation permissions retried before five-minute backoff")
	}
	editAdobeReconciliationFixture(t, s, a.ID, func(a *adobe.Account) {
		a.Quota.Generation.CheckedAt = time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339Nano)
	})
	s.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
	s.refreshDueAdobeAccounts(context.Background())
	got, _ = s.adobeStore.Get(a.ID)
	if calls.Load() != 6 || got.Quota.Status != "known" || got.Quota.Generation.Status != "known" {
		t.Fatal("known credit balance prevented retry of failed generation permissions")
	}
}

func TestAdobeAutomaticReconciliationProxyFailurePersistsSharedQuotaBackoff(t *testing.T) {
	s := imageTaskTestServer(t)
	if _, err := s.store.UpdateConfig("adobe_maintenance", map[string]any{"enabled": false}); err != nil {
		t.Fatal(err)
	}
	a, task := seedAdobeReconciliationImage(t, s, 31*time.Minute)
	var calls atomic.Int32
	s.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
	s.refreshDueAdobeAccounts(context.Background())
	editAdobeReconciliationFixture(t, s, a.ID, func(a *adobe.Account) { a.SettlementUntil = time.Now().Add(-time.Second).Unix() })
	if _, err := s.adobeStore.BindProxy(a.ID, proxyruntime.Binding{Reference: "node:missing/node", Platform: "adobe", Purpose: "account"}, "fixture missing route"); err != nil {
		t.Fatal(err)
	}
	s.refreshDueAdobeAccounts(context.Background())
	failed, _ := s.adobeStore.Get(a.ID)
	if calls.Load() != 2 || failed.Quota.Status != "error" || failed.Quota.Error != "adobe_routing_proxy_unavailable" || failed.Quota.CheckedAt == "" || failed.Quota.Generation.Status != "error" || failed.Quota.Generation.CheckedAt != failed.Quota.CheckedAt {
		t.Fatal("pre-request proxy error did not save a failed quota observation")
	}
	if failed.InFlight != "" || failed.Token != a.Token || failed.Revision != a.Revision || task.Status != "error" || task.Error != "original upstream timeout" {
		t.Fatal("routing failure changed credentials or replayed the original task")
	}
	// Even an older independent permissions error cannot bypass a recent
	// route failure that prevented both upstream observations.
	editAdobeReconciliationFixture(t, s, a.ID, func(a *adobe.Account) {
		a.Quota.Generation.CheckedAt = time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339Nano)
	})
	restarted := NewWithWorkers(s.cfg, false)
	restarted.PauseDeployment(false)
	restarted.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
	restarted.reconcileAdobeSubmissions(context.Background(), time.Now().Add(30*time.Second))
	unchanged, _ := restarted.adobeStore.Get(a.ID)
	if unchanged.QuotaRevision != failed.QuotaRevision || unchanged.Quota.CheckedAt != failed.Quota.CheckedAt || calls.Load() != 2 {
		t.Fatal("restart or older generation error bypassed routing cooldown")
	}
	editAdobeReconciliationFixture(t, restarted, a.ID, func(a *adobe.Account) {
		a.Quota.CheckedAt = time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339Nano)
	})
	restarted.refreshDueAdobeAccounts(context.Background())
	retried, _ := restarted.adobeStore.Get(a.ID)
	if retried.QuotaRevision != failed.QuotaRevision+1 || retried.Quota.Error != "adobe_routing_proxy_unavailable" || retried.Quota.CheckedAt == unchanged.Quota.CheckedAt || calls.Load() != 2 {
		t.Fatal("unavailable proxy was not retried after persisted backoff")
	}
	if _, err := restarted.adobeStore.BindProxy(a.ID, proxyruntime.Binding{Reference: "direct", Platform: "adobe", Purpose: "account"}, "fixture repaired route"); err != nil {
		t.Fatal(err)
	}
	editAdobeReconciliationFixture(t, restarted, a.ID, func(a *adobe.Account) {
		a.Quota.CheckedAt = time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339Nano)
		a.Quota.Generation.CheckedAt = a.Quota.CheckedAt
	})
	restarted.refreshDueAdobeAccounts(context.Background())
	got, _ := restarted.adobeStore.Get(a.ID)
	audits, _ := restarted.adobeStore.SubmissionResolutions(a.ID)
	if calls.Load() != 4 || got.Quota.Status != "known" || got.Quota.Generation.Status != "known" || len(audits) != 1 || audits[0].Outcome != "unknown" {
		t.Fatal("repaired proxy did not recover read-only accounting independently")
	}
}
