package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func TestAdobeVideoSafetyFailureReleasesOnlyOriginalReservation(t *testing.T) {
	for _, mode := range []string{"foreground", "background", "saved_terminal_expired_token", "saved_terminal_unavailable_proxy", "background_saved_terminal_unavailable_proxy", "nonterminal_unavailable_proxy", "completed_unavailable_proxy", "unknown_451"} {
		t.Run(mode, func(t *testing.T) {
			s := imageTaskTestServer(t)
			id := seedAdobeRecoveryVideo(t, s, "polling", true)
			job := s.videoJobs[id]
			a, err := s.adobeStore.Get(job.Route.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			other := adobeSchedulingAccount(t, s, "another-adobe-account", time.Now().Add(-time.Hour))
			if _, err := s.adobeStore.ReserveAccount("other-pending-task", other.ID); err != nil {
				t.Fatal(err)
			}
			key := job.Route.JobKey
			handle, err := s.adobeStore.LoadJob(key)
			if err != nil {
				t.Fatal(err)
			}
			var polls, forbidden atomic.Int32
			s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/v2/jobs/result/fixture" {
					forbidden.Add(1)
					t.Error("terminal recovery uploaded, generated or refreshed credentials")
				}
				polls.Add(1)
				body := `{"error_code":"video_unsafe","message":"PRIVATE_BODY"}`
				if mode == "unknown_451" {
					body = `{"error_code":"PRIVATE_UNKNOWN"}`
				}
				return &http.Response{StatusCode: 451, ProtoMajor: 2, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})})
			savedTerminal := strings.Contains(mode, "saved_terminal")
			if savedTerminal {
				handle.Status = "FAILED"
				handle.PollDiagnostic = &adobe.SubmissionDiagnostic{HTTPStatus: 451, ErrorCode: "video_unsafe", ErrorSource: "top_error_code", ResponseReceived: true}
				if err := s.adobeStore.SaveJob(key, handle); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "completed_unavailable_proxy" {
				handle.Status = "COMPLETED"
				handle.OutputURLs = []string{"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/fixture.mp4"}
				if err := s.adobeStore.SaveJob(key, handle); err != nil {
					t.Fatal(err)
				}
			}
			if savedTerminal || strings.Contains(mode, "unavailable_proxy") {
				editAdobeReconciliationFixture(t, s, a.ID, func(v *adobe.Account) {
					if savedTerminal {
						v.ExpiresAt = time.Now().Add(-time.Hour).Unix()
					}
					if strings.Contains(mode, "unavailable_proxy") {
						v.ProxyPolicy = "explicit"
						v.ProxyBinding = &proxyruntime.Binding{Reference: "node:missing/node", Platform: "adobe", Purpose: "account"}
					}
				})
				a, err = s.adobeStore.Get(a.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(mode, "background") {
				if err := s.adobeStore.MarkUnknown(key); err != nil {
					t.Fatal(err)
				}
				s.setVideoStatus(job, "failed", 0, map[string]any{"code": "upstream_poll_failed", "message": "old HTTP 451"})
				s.reconcileAdobeSubmissions(context.Background(), time.Now())
			} else {
				s.finishAdobeVideo(context.Background(), job, a, handle)
			}
			finished := waitAdobeVideoIdle(t, s, id)
			current, err := s.adobeStore.Get(a.ID)
			if err != nil {
				t.Fatal(err)
			}
			untouched, err := s.adobeStore.Get(other.ID)
			if err != nil {
				t.Fatal(err)
			}
			if untouched.InFlight != "other-pending-task" {
				t.Fatal("released another account's task")
			}
			known := mode != "unknown_451" && mode != "nonterminal_unavailable_proxy" && mode != "completed_unavailable_proxy"
			if finished.Status != "failed" || (current.InFlight == "") != known {
				t.Fatalf("status=%s in_flight=%s", finished.Status, current.InFlight)
			}
			if known && (finished.Error["code"] != "video_unsafe" || !strings.Contains(finished.Error["message"].(string), "视频安全检查未通过") || current.Quota.Status != "stale" || current.SettlementUntil <= time.Now().Unix()) {
				t.Fatal("terminal rejection lost cause or incorrectly assumed refund")
			}
			saved, err := s.adobeStore.LoadJob(key)
			if err != nil {
				t.Fatal(err)
			}
			if (saved.Status == "FAILED") != known || known && (saved.PollDiagnostic == nil || saved.PollDiagnostic.ErrorCode != "video_unsafe") {
				t.Fatal("terminal upstream evidence was not persisted")
			}
			if strings.HasPrefix(mode, "background") {
				records, err := s.adobeStore.SubmissionResolutions(a.ID)
				if err != nil || len(records) != 1 || records[0].Outcome != "failed" || records[0].Method != "automatic_poll" {
					t.Fatalf("missing automatic poll audit: %+v %v", records, err)
				}
			}
			wantPolls := int32(1)
			if savedTerminal || strings.Contains(mode, "unavailable_proxy") {
				wantPolls = 0
			}
			if (mode == "nonterminal_unavailable_proxy" || mode == "completed_unavailable_proxy") && finished.Error["code"] != "account_proxy_unavailable" {
				t.Fatal("poll or download skipped unavailable egress guard")
			}
			if polls.Load() != wantPolls || forbidden.Load() != 0 {
				t.Fatalf("polls=%d forbidden=%d", polls.Load(), forbidden.Load())
			}
			fresh := NewWithWorkers(s.cfg, false)
			fresh.adobeClient = s.adobeClient
			if known {
				fresh.reconcileAdobeSubmissions(context.Background(), time.Now())
				if polls.Load() != wantPolls {
					t.Fatal("restart replayed terminal poll")
				}
			}
			checkpoint, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "video_tasks", videoJournalName(id)))
			if err != nil || strings.Contains(string(checkpoint), "PRIVATE") {
				t.Fatal("private response leaked into task checkpoint")
			}
		})
	}
}

func TestAdobeExactPreflightRefusalIsRecordedBeforeUpload(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	fixture := &adobeMediaCapture{video: true, preflightPaid: true}
	s.adobeClient = fixture.client(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, adobeVideoMediaRequest(t, true, [][]byte{adobeMediaPNG(t, 16, 9)}))
	if w.Code != 200 {
		t.Fatalf("accept=%d %s", w.Code, w.Body.String())
	}
	var created videoJob
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	finished := waitAdobeVideoIdle(t, s, created.ID)
	if finished.Status != "failed" || finished.Error["code"] != "free_generation_unavailable" || !strings.Contains(finished.Error["message"].(string), "本次参数组合") {
		t.Fatalf("wrong preflight result: %+v", finished.Error)
	}
	fixture.mu.Lock()
	if fixture.preflights != 1 || fixture.uploads != 0 || fixture.submits != 0 || fixture.polls != 0 {
		t.Fatal("refused preflight caused media side effects")
	}
	fixture.mu.Unlock()
	current, err := s.adobeStore.Get(a.ID)
	if err != nil || current.InFlight != "" {
		t.Fatal("unsubmitted reservation retained")
	}
	raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "video_tasks", videoJournalName(created.ID)))
	if err != nil {
		t.Fatal(err)
	}
	var saved videoCheckpoint
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(saved.Record)
	for _, fragment := range []string{`"adobe_preflight_diagnostic"`, `"authorization":"authorized"`, `"resource_type":"generative_credits"`, `"error_code":"free_entitlement_unavailable"`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("checkpoint lost diagnostic %s", fragment)
		}
	}
}
