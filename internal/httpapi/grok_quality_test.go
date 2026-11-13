package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
	"github.com/auucoder/gptgrok2api-go/internal/xaiquality"
)

// Solve actual requests, rather than returning a canned marker that would also
// pass if the probe accidentally sent a cached prompt or another model.
func qualityFixtureBody(t *testing.T, payload []byte, wrong bool) []byte {
	t.Helper()
	var req struct {
		Model string `json:"model"`
		Input []struct {
			Content string `json:"content"`
		} `json:"input"`
	}
	if json.Unmarshal(payload, &req) != nil || req.Model != "grok-4.7" || len(req.Input) != 1 {
		t.Error("invalid quality payload")
		return nil
	}
	prompt := req.Input[0].Content
	marker := regexp.MustCompile(`cfm-[a-f0-9]+`).FindString(prompt)
	question := strings.Split(prompt, "只输出")[0]
	numbers := regexp.MustCompile(`[0-9]+`).FindAllString(question, -1)
	n := []int{}
	for _, s := range numbers {
		v, _ := strconv.Atoi(s)
		n = append(n, v)
	}
	answer := 0
	switch {
	case strings.HasPrefix(question, "仓库"):
		if len(n) != 6 {
			t.Error("invalid inventory question")
			return nil
		}
		answer = n[0]*n[1]*n[2] - n[3]*n[2] - n[4] + n[5]*n[2]
	case strings.HasPrefix(question, "任务"):
		if len(n) != 6 {
			t.Error("invalid schedule question")
			return nil
		}
		answer = max(n[0]+n[3], n[1]+n[4]) + n[5]
	case strings.HasPrefix(question, "从1"):
		if len(n) < 2 {
			t.Error("invalid count question")
			return nil
		}
		for i := 1; i <= n[1]; i++ {
			if i%7 == 0 {
				continue
			}
			if i%3 == 0 {
				answer++
				continue
			}
			if i%5 == 0 {
				answer++
			}
		}
	default:
		t.Error("unknown challenge")
		return nil
	}
	if wrong {
		answer = -1
	}
	text, _ := json.Marshal(map[string]any{"answer": answer, "marker": marker})
	body, _ := json.Marshal(map[string]any{"model": "grok-4.7", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(text)}}}}, "usage": map[string]any{"output_tokens": 32, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}})
	return body
}

func installQualityUpstream(t *testing.T, s *Server, handler func(http.ResponseWriter, *http.Request)) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/responses" {
			t.Error("probe refreshed tokens or queried billing")
			w.WriteHeader(500)
			return
		}
		if handler != nil {
			handler(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		w.Write(qualityFixtureBody(t, raw, false))
	}))
	t.Cleanup(upstream.Close)
	s.xaiProbe = provider.NewXAIProbe(upstream.URL, upstream.URL+"/token", upstream.Client())
	return calls
}

func TestQualityGatesEveryNewCPADelivery(t *testing.T) {
	for _, mode := range []string{"normal", "wrong", "limited", "one_wrong"} {
		t.Run(mode, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			remote := installXAITestCPA(t, s)
			count := 0
			calls := installQualityUpstream(t, s, func(w http.ResponseWriter, r *http.Request) {
				count++
				if r.Header.Get("Authorization") != "Bearer fresh-access" {
					t.Error("wrong credential")
				}
				if mode == "limited" {
					w.WriteHeader(429)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				w.Write(qualityFixtureBody(t, raw, mode == "wrong" || mode == "one_wrong" && count == 1))
			})
			item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), CPASync: &oauth.CPASync{Status: "pending", PoolID: "test-cpa"}})
			updated, err := s.syncGrokOAuthCPA(context.Background(), item.ID, "test-cpa", false)
			if mode == "normal" {
				if err != nil || remote.uploads != 1 || calls.Load() != 3 || updated.Quality.Status != "normal" {
					t.Fatalf("healthy grant failed: %v", err)
				}
			} else {
				if err == nil || remote.uploads != 0 || updated.CPASync.Status != "quality_blocked" {
					t.Fatal("failed quality uploaded")
				}
				want := "high_risk"
				if mode == "limited" {
					want = "error"
				}
				if mode == "one_wrong" {
					want = "suspect"
				}
				if updated.Quality.Status != want {
					t.Fatalf("got %s want %s", updated.Quality.Status, want)
				}
			}
		})
	}
}

func TestQualityManagedCPAProbeAndRemoval(t *testing.T) {
	for _, mode := range []string{"removed", "unsupported", "denied", "retained", "identity_mismatch", "changed_after_probe", "api_unsupported", "normal", "limited", "lost_delete_response", "target_changed"} {
		t.Run(mode, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			localCalls := installQualityUpstream(t, s, func(w http.ResponseWriter, r *http.Request) {
				t.Error("used stale local credential")
				w.WriteHeader(500)
			})
			name := "xai-test@example.test.json"
			exists := true
			probes, deletes := 0, 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer manager" {
					t.Error("wrong management credential")
				}
				switch {
				case r.Method == "GET" && r.URL.Path == "/v0/management/auth-files":
					rows := []map[string]any{{"name": "unrelated.json", "email": "other@example.test", "provider": "xai", "auth_index": "other-index"}}
					if exists {
						email := "test@example.test"
						index := "selected-index"
						if mode == "identity_mismatch" {
							email = "other@example.test"
						}
						if mode == "changed_after_probe" && probes == 6 {
							index = "replacement-index"
						}
						rows = append(rows, map[string]any{"name": name, "email": email, "provider": "xai", "auth_index": index})
					}
					json.NewEncoder(w).Encode(map[string]any{"files": rows})
				case r.Method == "POST" && r.URL.Path == "/v0/management/api-call":
					probes++
					if mode == "target_changed" && probes == 6 {
						s.external.saveCPAList([]externalCPAPool{{ID: "test-cpa", Name: "Replaced CPA", BaseURL: "http://changed.invalid", SecretKey: "manager"}})
					}
					if mode == "api_unsupported" {
						w.WriteHeader(404)
						return
					}
					var req struct {
						Index  string            `json:"auth_index"`
						URL    string            `json:"url"`
						Header map[string]string `json:"header"`
						Data   string            `json:"data"`
					}
					json.NewDecoder(r.Body).Decode(&req)
					if req.Index != "selected-index" || req.Header["Authorization"] != "Bearer $TOKEN$" || req.URL != s.cfg.XAICLIBaseURL+"/responses" {
						t.Error("probe not pinned to CPA identity")
					}
					status := 200
					if mode == "limited" {
						status = 402
					}
					json.NewEncoder(w).Encode(map[string]any{"status_code": status, "body": string(qualityFixtureBody(t, []byte(req.Data), mode != "normal"))})
				case r.Method == "DELETE" && r.URL.Path == "/v0/management/auth-files":
					deletes++
					if r.URL.Query().Get("name") != name || r.URL.Query().Get("all") != "" {
						t.Error("unsafe deletion target")
					}
					stored, _ := s.oauthStore.List()
					if !stored[0].Quality.Blocked {
						t.Error("removed before risk persisted")
					}
					if mode == "unsupported" {
						w.WriteHeader(405)
						return
					}
					if mode == "denied" {
						w.WriteHeader(403)
						return
					}
					if mode != "retained" {
						exists = false
					}
					if mode == "lost_delete_response" {
						w.WriteHeader(500)
						return
					}
					w.Write([]byte(`{"status":"ok"}`))
				default:
					t.Errorf("unexpected CPA operation %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer remote.Close()
			s.external.saveCPAList([]externalCPAPool{{ID: "test-cpa", Name: "CPA", BaseURL: remote.URL, SecretKey: "manager"}})
			item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "OLD-ACCESS", RefreshToken: "OLD-REFRESH", ExpiresAt: 1, CPASync: &oauth.CPASync{Status: "synced", PoolID: "test-cpa", FileName: name, Managed: true}})
			updated, err := s.checkGrokQuality(context.Background(), item.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if localCalls.Load() != 0 || updated.AccessToken != "OLD-ACCESS" || updated.RefreshToken != "OLD-REFRESH" {
				t.Fatal("CPA ownership violated")
			}
			switch mode {
			case "normal":
				if updated.Quality.Status != "normal" || deletes != 0 {
					t.Fatal("healthy account removed")
				}
			case "identity_mismatch", "api_unsupported", "limited":
				if updated.Quality.Status != "error" || updated.Quality.Blocked || deletes != 0 {
					t.Fatal("infrastructure error convicted")
				}
			case "changed_after_probe", "target_changed":
				if deletes != 0 || updated.Quality.Removal.Status != "manual_required" {
					t.Fatal("replacement credential removed")
				}
			default:
				if probes != 6 || deletes != 1 || !updated.Quality.Blocked {
					t.Fatalf("expected confirmed risk: probes %d deletes %d", probes, deletes)
				}
				want := "manual_required"
				if mode == "removed" || mode == "lost_delete_response" {
					want = "removed"
				}
				if updated.Quality.Removal.Status != want {
					t.Fatal("incorrect removal receipt")
				}
				if _, err = s.syncGrokOAuthCPA(context.Background(), item.ID, "test-cpa", false); err == nil {
					t.Fatal("risk account resynchronized")
				}
				if mode == "removed" {
					if _, err = s.checkGrokQuality(context.Background(), item.ID, true); err != nil || deletes != 1 {
						t.Fatal("removed file deleted twice")
					}
				}
			}
			public, _ := json.Marshal(oauth.Public(updated))
			if strings.Contains(string(public), "OLD-ACCESS") || strings.Contains(string(public), "OLD-REFRESH") {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestQualitySchedulePersistsAndDeduplicates(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	now := time.Now()
	item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "fresh", RefreshToken: "refresh"})
	api := s.Handler()
	res := adminRequest(api, "PUT", "/api/grok/oauth/quality/schedule", strings.NewReader(`{"enabled":true,"interval_hours":12}`))
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	s.runGrokQualityIfDue(now)
	s.runGrokQualityIfDue(now)
	jobs := s.taskQueue.List()
	if len(jobs) != 1 || stringValue(jobs[0].Payload["account_id"]) != item.ID {
		t.Fatal("duplicate scheduler jobs")
	}
	s.taskQueue.Cancel(jobs[0].ID)
	s.oauthStore.UpdateIfCredential(item, func(a *oauth.Account) { a.Quality = &xaiquality.Report{Status: "error", At: now.Unix()} })
	s.runGrokQualityIfDue(now.Add(11 * time.Hour))
	if len(s.taskQueue.List()) != 1 {
		t.Fatal("failed probe immediately retried")
	}
	s.registerStore.SetGrokQualitySchedule(registerruntime.GrokQualitySchedule{Enabled: false, IntervalHours: 12})
	s.runGrokQualityIfDue(now.Add(13 * time.Hour))
	if len(s.taskQueue.List()) != 1 {
		t.Fatal("disabled scheduler ran")
	}
	s.registerStore.SetGrokQualitySchedule(registerruntime.GrokQualitySchedule{Enabled: true, IntervalHours: 12})
	s.runGrokQualityIfDue(now.Add(13 * time.Hour))
	if len(s.taskQueue.List()) != 2 {
		t.Fatal("due account skipped")
	}
	restarted := New(s.cfg)
	if cfg := restarted.registerStore.GrokQualitySchedule(); !cfg.Enabled || cfg.IntervalHours != 12 {
		t.Fatal("schedule not persisted")
	}
	for _, hours := range []int{0, 169} {
		res = adminRequest(api, "PUT", "/api/grok/oauth/quality/schedule", strings.NewReader(fmt.Sprintf(`{"enabled":true,"interval_hours":%d}`, hours)))
		if res.Code != 400 {
			t.Fatal("invalid interval accepted")
		}
	}
	unauthorized := httptest.NewRecorder()
	api.ServeHTTP(unauthorized, httptest.NewRequest("POST", "/api/grok/oauth/quality", strings.NewReader(`{"ids":["x"]}`)))
	if unauthorized.Code != 401 {
		t.Fatal("quality API lacks admin authentication")
	}
}

func TestQualityReauthorizationRemovesOlderCPAFile(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	remote := installXAITestCPA(t, s)
	name := "xai-test@example.test.json"
	remote.rows[name] = map[string]any{"type": "xai", "email": "test@example.test", "access_token": "CPA-ROTATED", "refresh_token": "CPA-REFRESH"}
	installQualityUpstream(t, s, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Write(qualityFixtureBody(t, raw, true))
	})
	s.registerStore.SetGrokQualitySchedule(registerruntime.GrokQualitySchedule{Enabled: false, IntervalHours: 24})
	item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "fresh-grant", RefreshToken: "fresh-refresh", CPASync: &oauth.CPASync{Status: "pending", PoolID: "test-cpa", ReplaceExisting: true}})
	updated, err := s.syncGrokOAuthCPA(context.Background(), item.ID, "test-cpa", false)
	if err == nil || remote.uploads != 0 || remote.deletes != 1 || updated.Quality.Removal == nil || updated.Quality.Removal.Status != "removed" {
		t.Fatalf("new grant stranded old risky CPA file: %v", err)
	}
	if !updated.Quality.Blocked {
		t.Fatal("disabling schedule bypassed the delivery gate")
	}
}

func TestQualityKnownRiskCannotAdoptExistingCPA(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	remote := installXAITestCPA(t, s)
	name := "xai-test@example.test.json"
	remote.rows[name] = map[string]any{"type": "xai", "email": "test@example.test", "access_token": "CPA-ROTATED"}
	item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "fresh-grant", RefreshToken: "refresh", Quality: &xaiquality.Report{Status: "high_risk", Blocked: true, ConfirmedAt: 1}, CPASync: &oauth.CPASync{Status: "pending", PoolID: "test-cpa"}})
	updated, err := s.syncGrokOAuthCPA(context.Background(), item.ID, "test-cpa", false)
	if err == nil || updated.CPASync.Status == "synced" || remote.uploads != 0 {
		t.Fatal("known risk adopted as healthy delivery")
	}
}

func TestQualityCancellationStopsUpstreamWork(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	calls := installQualityUpstream(t, s, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	item, _ := s.oauthStore.Import(oauth.Account{Email: "cancel@example.test", AccessToken: "access", RefreshToken: "refresh"})
	ids, err := s.enqueueGrokQuality([]string{item.ID}, false)
	if err != nil || len(ids) != 1 {
		t.Fatal("failed to enqueue")
	}
	task, _ := s.taskQueue.Get(ids[0])
	done := make(chan struct{})
	go func() { defer close(done); s.runGrokQualityTask(&task) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not start")
	}
	s.taskQueue.Cancel(task.ID)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled probe kept running")
	}
	current, _, _ := s.oauthStore.Get(item.ID)
	if calls.Load() != 1 || current.Quality == nil || current.Quality.Status != "error" || current.Quality.Blocked {
		t.Fatal("cancelled probe convicted account or continued sampling")
	}
}
