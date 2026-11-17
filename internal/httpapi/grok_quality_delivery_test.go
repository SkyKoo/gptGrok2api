package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
	"github.com/auucoder/gptgrok2api-go/internal/xaiquality"
)

func TestQualityRecheckAutomaticallyDeliversPendingGrant(t *testing.T) {
	for _, route := range []string{"manual", "scheduled", "legacy_pending"} {
		t.Run(route, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			remote := installXAITestCPA(t, s)
			calls := installQualityUpstream(t, s, nil)
			cp := &oauth.CPASync{Status: "quality_blocked", PoolID: "test-cpa", FileName: "xai-test@example.test.json", ReplaceExisting: true, Error: "质量检测未通过"}
			if route == "legacy_pending" {
				cp.Status, cp.Error = "pending", "凭据已刷新，请重新同步"
			}
			item, err := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), CPASync: cp, Quality: &xaiquality.Report{Status: "suspect", Blocked: true, At: time.Now().Add(-25 * time.Hour).Unix()}})
			if err != nil {
				t.Fatal(err)
			}
			if route == "scheduled" {
				s.registerStore.SetGrokQualitySchedule(registerruntime.GrokQualitySchedule{Enabled: true, IntervalHours: 24})
				s.runGrokQualityIfDue(time.Now())
				jobs := s.taskQueue.List()
				if len(jobs) != 1 {
					t.Fatal("scheduled recheck was not queued")
				}
				_, err = s.runGrokQualityTask(&jobs[0])
			} else {
				_, err = s.checkGrokQuality(context.Background(), item.ID, false)
			}
			after, _, _ := s.oauthStore.Get(item.ID)
			if err != nil || remote.uploads != 1 || calls.Load() != 6 || after.Quality.Blocked || after.Quality.Status != "normal" || after.CPASync.Status != "synced" || after.CPASync.Error != "" || !oauth.CPAManaged(after) {
				t.Fatalf("recheck did not deliver exactly once: err=%v uploads=%d probes=%d receipt=%+v", err, remote.uploads, calls.Load(), after.CPASync)
			}
			// After handoff, rechecks must use CPA's current credential. All local
			// renewal routes are rejected, even when the retained copy expires.
			_, err = s.checkGrokQuality(context.Background(), item.ID, false)
			if err != nil || remote.uploads != 1 || remote.qualityCalls != 3 || calls.Load() != 6 {
				t.Fatal("post-handoff recheck uploaded or used the local credential")
			}
			for _, action := range []string{"quota", "refresh", "test", "models/sync"} {
				res := adminRequest(s.Handler(), "POST", "/api/grok/oauth/accounts/"+item.ID+"/"+action, strings.NewReader(`{}`))
				if res.Code != http.StatusConflict {
					t.Fatalf("post-handoff %s accepted: %d", action, res.Code)
				}
			}
			if calls.Load() != 6 {
				t.Fatal("local renewal contacted xAI after handoff")
			}
		})
	}
}

func TestQualityRecheckKeepsDeliveryBlockedUntilPass(t *testing.T) {
	for _, mode := range []string{"error", "suspect", "high_risk"} {
		t.Run(mode, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			remote := installXAITestCPA(t, s)
			count := 0
			installQualityUpstream(t, s, func(w http.ResponseWriter, r *http.Request) {
				count++
				if mode == "error" {
					w.WriteHeader(429)
					return
				}
				raw, _ := io.ReadAll(r.Body)
				w.Write(qualityFixtureBody(t, raw, mode == "high_risk" || count == 1))
			})
			item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "fresh", RefreshToken: "refresh", CPASync: &oauth.CPASync{Status: "pending", PoolID: "test-cpa"}})
			after, err := s.checkGrokQuality(context.Background(), item.ID, false)
			if err != nil || after.Quality.Status != mode || remote.uploads != 0 || oauth.CPAManaged(after) {
				t.Fatalf("failed recheck changed ownership: %v", err)
			}
		})
	}
}

func TestQualityRecheckDoesNotRestoreStoppedOrUncertainDelivery(t *testing.T) {
	for _, mode := range []string{"managed", "unknown", "unknown_no_attempt", "legacy_incomplete", "removed", "manual_removal", "no_target", "no_receipt", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			remote := installXAITestCPA(t, s)
			cp := &oauth.CPASync{Status: "pending", PoolID: "test-cpa", FileName: "xai-test@example.test.json"}
			status := "active"
			switch mode {
			case "managed":
				cp.Status, cp.Managed = "synced", true
			case "unknown":
				cp.Status, cp.AttemptedAt = "unknown", 1
			case "unknown_no_attempt":
				cp.Status = "unknown"
			case "legacy_incomplete":
				cp.Status, cp.At, cp.FileName = "pending", 1, ""
			case "removed":
				cp.Status, cp.Managed = "quality_removed", true
			case "manual_removal":
				cp.ManualRemoval = &xaiquality.Removal{Status: "removed"}
			case "no_target":
				cp.PoolID = ""
			case "no_receipt":
				cp = nil
			case "disabled":
				status = "disabled"
			}
			if mode == "managed" || mode == "unknown" {
				remote.rows[cp.FileName] = map[string]any{"type": "xai", "email": "test@example.test", "access_token": "remote-rotated", "refresh_token": "remote-refresh"}
			}
			item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", Status: status, AccessToken: "local", RefreshToken: "local-refresh", CPASync: cp})
			s.checkGrokQuality(context.Background(), item.ID, false)
			after, _, _ := s.oauthStore.Get(item.ID)
			if remote.uploads != 0 || remote.deletes != 0 || !reflect.DeepEqual(after.CPASync, cp) {
				t.Fatalf("recheck changed delivery without permission: %+v", after.CPASync)
			}
		})
	}
}

func TestQualityAutoDeliveryUnknownResponseNeverRepeatsUpload(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	remote := installXAITestCPA(t, s)
	remote.postMode = "uncommitted"
	item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "local", RefreshToken: "refresh", CPASync: &oauth.CPASync{Status: "pending", PoolID: "test-cpa"}})
	after, err := s.checkGrokQuality(context.Background(), item.ID, false)
	if err == nil || remote.uploads != 1 || after.Quality.Status != "normal" || after.CPASync.Status != "unknown" || !oauth.CPAManaged(after) {
		t.Fatal("unknown upload was not recorded independently from successful quality")
	}
	s.checkGrokQuality(context.Background(), item.ID, false)
	if remote.uploads != 1 {
		t.Fatal("recheck repeated an uncertain upload")
	}
}

func TestQualityAutoDeliveryPersistsMissingTargetFailure(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	remote := installXAITestCPA(t, s)
	item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "local", RefreshToken: "refresh", CPASync: &oauth.CPASync{Status: "quality_blocked", PoolID: "deleted-cpa", Error: "old quality failure"}})
	after, err := s.checkGrokQuality(context.Background(), item.ID, false)
	if err == nil || after.Quality.Status != "normal" || after.CPASync.Status != "failed" || after.CPASync.Error != err.Error() || after.CPASync.PoolID != "deleted-cpa" || remote.uploads != 0 {
		t.Fatalf("preflight failure was lost or target changed: err=%v receipt=%+v", err, after.CPASync)
	}
	reloaded, _, _ := s.oauthStore.Get(item.ID)
	if !reflect.DeepEqual(reloaded.CPASync, after.CPASync) || reloaded.Quality.Status != "normal" {
		t.Fatal("failure not persisted independently from the quality pass")
	}
}

func TestOAuthRenewalPreservesOriginalDeliveryFailure(t *testing.T) {
	for _, state := range []string{"pending", "quality_blocked", "failed"} {
		t.Run(state, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/token":
					w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
				case "/billing":
					w.Write([]byte(`{"creditUsagePercent":25}`))
				default:
					t.Error("unexpected upstream path")
				}
			}))
			defer upstream.Close()
			s.xaiProbe = provider.NewXAIProbe(upstream.URL, upstream.URL+"/token", upstream.Client())
			item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: 1, CPASync: &oauth.CPASync{Status: state, PoolID: "pool", Error: "original delivery reason"}})
			after, err := s.refreshOAuthQuota(context.Background(), item.ID)
			if err != nil || after.AccessToken != "new-access" || !reflect.DeepEqual(after.CPASync, item.CPASync) {
				t.Fatalf("renewal erased delivery state: %v %+v", err, after.CPASync)
			}
		})
	}
}
