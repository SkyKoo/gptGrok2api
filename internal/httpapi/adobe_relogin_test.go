package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

func adobeReloginBody(requestID string, ids ...string) string {
	raw, _ := json.Marshal(map[string]any{"request_id": requestID, "ids": ids})
	return string(raw)
}
func waitAdobeReloginTasks(t *testing.T, s *Server, requestID string) []adobe.ReloginTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := imageTaskCall(s, "GET", "/api/adobe/accounts/relogin?request_id="+requestID, "admin-secret", "")
		var result struct {
			Items []adobe.ReloginTask `json:"items"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatal("progress unreadable", response.Code)
		}
		done := len(result.Items) > 0
		for _, task := range result.Items {
			done = done && task.Done
		}
		if done {
			return result.Items
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("relogin did not complete")
	return nil
}
func TestAdobeReloginAdminIsolationAndUnknownRetention(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	var calls atomic.Int32
	s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
		calls.Add(1)
		current.MediaSessionRefreshed = true
		current.MediaSessionStatus = "complete"
		return current, nil
	})
	body := adobeReloginBody("request-unknown-fixture", a.ID, a.ID, "missing")
	for _, key := range []string{"", "api-secret"} {
		for _, method := range []string{"GET", "POST"} {
			response := imageTaskCall(s, method, "/api/adobe/accounts/relogin", key, body)
			if response.Code != 401 && response.Code != 403 {
				t.Fatalf("non-admin relogin: %d", response.Code)
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized request reached browser")
	}
	invalid := imageTaskCall(s, "POST", "/api/adobe/accounts/relogin", "admin-secret", `{"ids":[]}`)
	if invalid.Code != 400 {
		t.Fatal("empty relogin accepted")
	}
	if _, err := s.adobeStore.Reserve("original-task"); err != nil {
		t.Fatal(err)
	}
	active := imageTaskCall(s, "POST", "/api/adobe/accounts/relogin", "admin-secret", adobeReloginBody("request-active-fixture", a.ID))
	if active.Code != 202 || calls.Load() != 0 || !strings.Contains(active.Body.String(), adobe.ErrBusy.Error()) {
		t.Fatal("active task allowed relogin")
	}
	if err := s.adobeStore.MarkUnknown("original-task"); err != nil {
		t.Fatal(err)
	}
	response := imageTaskCall(s, "POST", "/api/adobe/accounts/relogin", "admin-secret", body)
	if response.Code != 202 {
		t.Fatalf("not accepted: %d %s", response.Code, response.Body.String())
	}
	tasks := waitAdobeReloginTasks(t, s, "request-unknown-fixture")
	if len(tasks) != 2 || calls.Load() != 1 {
		t.Fatal("invalid per-account or deduplication behavior")
	}
	for _, task := range tasks {
		if task.AccountRef == a.ID && (!task.SessionUpdated || !task.SubmissionUnknown || task.Stage != "completed") {
			t.Fatal("unknown task state lost")
		}
		if task.AccountRef == "missing" && (task.SessionUpdated || task.Error != adobe.ErrNotFound.Error()) {
			t.Fatal("partial failure concealed")
		}
	}
	response = imageTaskCall(s, "GET", "/api/adobe/accounts/relogin", "admin-secret", "")
	for _, secret := range []string{a.Token, a.Cookie, "sherlockToken", "original-task"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("private state leaked")
		}
	}
	saved, _ := s.adobeStore.Get(a.ID)
	if saved.Status != "submission_unknown" || saved.InFlight != "original-task" {
		t.Fatal("background login released unknown task")
	}
	if _, err := s.adobeStore.Reserve("new-task"); !errors.Is(err, adobe.ErrNoAccount) {
		t.Fatal("API relogin released held task")
	}
}

func TestAdobeReloginAcceptsQuicklyAndSurvivesRequestCancellation(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var calls atomic.Int32
	s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
		calls.Add(1)
		close(entered)
		<-release
		if ctx.Err() != nil {
			t.Error("HTTP cancellation reached background task")
		}
		current.MediaSessionRefreshed = true
		current.MediaSessionStatus = "complete"
		return current, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("POST", "/api/adobe/accounts/relogin", strings.NewReader(adobeReloginBody("request-detached-fixture", a.ID))).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer admin-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	cancel()
	if response.Code != 202 {
		t.Fatal("login waited for browser or was rejected")
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("background browser did not start")
	}
	var accepted struct {
		Items []adobe.ReloginTask `json:"items"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &accepted)
	if len(accepted.Items) != 1 {
		t.Fatal("missing accepted task")
	}
	for _, requestID := range []string{"request-detached-fixture", "request-joined-fixture"} {
		again := imageTaskCall(s, "POST", "/api/adobe/accounts/relogin", "admin-secret", adobeReloginBody(requestID, a.ID))
		var duplicate struct {
			Items []adobe.ReloginTask `json:"items"`
		}
		_ = json.Unmarshal(again.Body.Bytes(), &duplicate)
		if again.Code != 202 || len(duplicate.Items) != 1 || duplicate.Items[0].ProgressID != accepted.Items[0].ProgressID || calls.Load() != 1 {
			t.Fatal("duplicate admission launched another browser")
		}
	}
	conflicting := imageTaskCall(s, "POST", "/api/adobe/accounts/relogin", "admin-secret", adobeReloginBody("request-detached-fixture", "other"))
	if conflicting.Code != 409 {
		t.Fatal("request identity reused for another account")
	}
	if s.DeploymentStatus()["detached"] < 1 {
		t.Fatal("background task missing from deployment drain")
	}
	s.PauseDeployment(true)
	blocked := imageTaskCall(s, "POST", "/api/adobe/accounts/relogin", "admin-secret", adobeReloginBody("request-paused-fixture", a.ID))
	if blocked.Code != 503 {
		t.Fatal("maintenance admitted login")
	}
	progress := imageTaskCall(s, "GET", "/api/adobe/accounts/relogin?request_id=request-joined-fixture", "admin-secret", "")
	if progress.Code != 200 {
		t.Fatal("maintenance blocked progress reads")
	}
	close(release)
	tasks := waitAdobeReloginTasks(t, s, "request-detached-fixture")
	if !tasks[0].SessionUpdated || calls.Load() != 1 {
		t.Fatal("detached login did not complete once")
	}
	again := imageTaskCall(s, "GET", "/api/adobe/accounts/relogin?request_id=request-detached-fixture", "admin-secret", "")
	if again.Code != 200 || calls.Load() != 1 {
		t.Fatal("progress read replayed login")
	}
}

func TestAdobeBrowserReloginWorkerContract(t *testing.T) {
	for _, tc := range []struct {
		name, stage, code, want string
		mode, source            string
		verified                bool
	}{
		{name: "complete runtime session", stage: "completed", verified: true},
		{name: "runtime explicitly omitted", stage: "completed", verified: true, mode: "omitted", source: "spl_runtime"},
		{name: "omission without source", stage: "completed", verified: true, mode: "omitted", want: "adobe_relogin_media_session_incomplete"},
		{name: "token alone", stage: "completed", want: "adobe_relogin_media_session_incomplete"},
		{name: "login", stage: "needs_manual", code: "refresh_login_required", want: "adobe_relogin_login_required"},
		{name: "challenge", stage: "needs_manual", code: "refresh_verification_required", want: "adobe_relogin_verification_required"},
		{name: "otp", stage: "needs_manual", code: "refresh_otp_required", want: "adobe_relogin_email_verification_required"},
		{name: "missing login entry", stage: "needs_manual", code: "refresh_login_entry_unrecognized", want: "adobe_relogin_login_entry_unrecognized"},
		{name: "login form unavailable", stage: "needs_manual", code: "refresh_login_form_unavailable", want: "adobe_relogin_login_form_unavailable"},
		{name: "password form unavailable", stage: "needs_manual", code: "refresh_password_form_unavailable", want: "adobe_relogin_password_form_unavailable"},
		{name: "login pending", stage: "needs_manual", code: "refresh_login_pending", want: "adobe_relogin_login_pending"},
		{name: "completion unconfirmed", stage: "needs_manual", code: "refresh_login_completion_unconfirmed", want: "adobe_relogin_login_pending"},
		{name: "cancelled session", stage: "needs_manual", code: "media_session_cancelled", want: "adobe_relogin_cancelled"},
		{name: "untrusted detail", stage: "needs_manual", code: "private cookie secret url", want: "adobe_relogin_browser_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scheduleServer(t)
			a := adobeFixtureAccount(t, s)
			a.ProfileID = "fixture-profile"
			rows, err := s.adobeStore.Import([]adobe.Account{a})
			if err != nil {
				t.Fatal(err)
			}
			a = rows[0]
			runs, releases := 0, 0
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-worker-key" {
					t.Error("worker auth missing")
				}
				switch {
				case r.URL.Path == "/v1/browser/reservations" && r.Method == "POST":
					var opts map[string]any
					_ = json.NewDecoder(r.Body).Decode(&opts)
					if opts["adobe_mode"] != "refresh" || opts["provider"] != "adobe" || opts["profile_id"] != a.ProfileID || opts["account_id"] != a.AccountID || opts["proxy"] != "direct" {
						t.Error("relogin lost profile identity or egress")
					}
					writeJSON(w, 200, map[string]any{"status": "ready"})
				case strings.HasPrefix(r.URL.Path, "/v1/browser/reservations/") && r.Method == "DELETE":
					releases++
					writeJSON(w, 200, map[string]any{"status": "closed"})
				case r.URL.Path == "/v1/adobe/run" && r.Method == "POST":
					runs++
					var command map[string]any
					_ = json.NewDecoder(r.Body).Decode(&command)
					if command["action"] != "refresh" {
						t.Error("relogin started signup or generation")
					}
					replacement := a
					replacement.MediaSessionRefreshed = tc.verified
					replacement.MediaSessionStatus = "complete"
					replacement.MediaSessionARPMode = tc.mode
					replacement.MediaSessionARPSource = tc.source
					if tc.mode == "omitted" {
						replacement.Cookie = ""
					}
					writeJSON(w, 200, map[string]any{"stage": tc.stage, "code": tc.code, "account": replacement})
				default:
					t.Error("unexpected worker request")
					w.WriteHeader(404)
				}
			}))
			defer worker.Close()
			_, err = s.registerStore.Update(map[string]any{"adobe": map[string]any{"api_base": worker.URL, "api_key": "fixture-worker-key"}})
			if err != nil {
				t.Fatal(err)
			}
			s.adobeStore.SetBrowserRefresher(s.refreshAdobeBrowserAccount)
			_, err = s.adobeStore.Relogin(context.Background(), a.ID)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && adobe.SafeError(err) != tc.want {
				t.Fatalf("got %v want %s", err, tc.want)
			}
			if runs != 1 || releases != 1 {
				t.Fatal("browser command retried or lease leaked")
			}
			saved, _ := s.adobeStore.Get(a.ID)
			if tc.want == "" && tc.mode == "omitted" && (saved.Cookie != "" || saved.MediaSessionARPMode != "omitted" || saved.MediaSessionARPSource != "spl_runtime") {
				t.Fatal("Worker omission decision was dropped")
			}
			if tc.want != "" && (saved.Revision != a.Revision || saved.Cookie != a.Cookie || saved.Token != a.Token) {
				t.Fatal("failed browser verification replaced credentials")
			}
		})
	}
}

func TestAdobeReloginBatchRunsSeriallyAndMaintenanceFileRejectsAdmission(t *testing.T) {
	s := imageTaskTestServer(t)
	first := adobeFixtureAccount(t, s)
	second := first
	second.ID = ""
	second.AccountID = "second-fixture"
	second.Email = "second@example.test"
	claims, _ := json.Marshal(map[string]any{"user_id": second.AccountID, "exp": time.Now().Add(time.Hour).Unix()})
	second.Token = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	imported, err := s.adobeStore.Import([]adobe.Account{second})
	if err != nil {
		t.Fatal(err)
	}
	second = imported[0]
	maintenance := filepath.Join(s.cfg.DataDir, "cfm-maintenance")
	if err = os.WriteFile(maintenance, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	rejected := imageTaskCall(s, "POST", "/api/adobe/accounts/relogin", "admin-secret", adobeReloginBody("request-maintenance-fixture", first.ID))
	if rejected.Code != 503 {
		t.Fatal("maintenance file admitted background login")
	}
	if err = os.Remove(maintenance); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var calls atomic.Int32
	s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		current.MediaSessionRefreshed = true
		current.MediaSessionStatus = "complete"
		return current, nil
	})
	response := imageTaskCall(s, "POST", "/api/adobe/accounts/relogin", "admin-secret", adobeReloginBody("request-batch-fixture", first.ID, second.ID))
	if response.Code != 202 {
		t.Fatal("batch not accepted")
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("batch did not start")
	}
	tasks, err := s.adobeReloginJobs.List("request-batch-fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.AccountRef == second.ID && task.Stage != "queued" {
			t.Fatal("second account began before first completed")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("batch browser calls overlapped")
	}
	close(release)
	tasks = waitAdobeReloginTasks(t, s, "request-batch-fixture")
	if len(tasks) != 2 || calls.Load() != 2 {
		t.Fatal("batch did not complete exactly once per account")
	}
	for _, task := range tasks {
		if !task.SessionUpdated || task.Error != "" {
			t.Fatal("batch completion was not recorded")
		}
	}
}
