package accounts

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/auucoder/gptgrok2api-go/internal/store"
	"sync"
	"testing"
	"time"
)

func grokSnapshot(image, video int) map[string]any {
	return map[string]any{"status": "success", "checked_at": time.Now().UTC().Format(time.RFC3339Nano), "quotas": map[string]any{
		"imagePro":  map[string]any{"available": true, "remaining": image, "next_available_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		"video720p": map[string]any{"available": true, "remaining": video, "next_available_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}}
}
func TestGrokMediaUnknownStaleAndFailedDoNotBorrowTextQuota(t *testing.T) {
	for _, scenario := range []string{"missing", "stale", "failed", "unknown", "zero", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			_, s := quotaPool(t, 100, 100, "正常")
			snapshot := grokSnapshot(2, 2)
			fields := map[string]any{"media_quota": snapshot}
			switch scenario {
			case "missing":
				fields["media_quota"] = nil
			case "stale":
				snapshot["checked_at"] = time.Now().Add(-6 * time.Minute).Format(time.RFC3339)
			case "failed":
				snapshot["status"] = "failed"
			case "unknown":
				snapshot["quotas"] = map[string]any{"imagePro": map[string]any{"available": true}}
			case "zero":
				fields["media_quota"] = grokSnapshot(0, 0)
			case "disabled":
				fields["enabled"] = false
			}
			_, _, _ = s.UpdateAccount("token", fields)
			p := NewGrok(s)
			if _, err := reserveIntent(p, Intent{Kind: "image", Model: "grok-imagine-image-pro"}); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("media should be unavailable: %v", err)
			}
			if scenario != "disabled" {
				l, err := p.Reserve(context.Background(), []string{"basic"}, nil)
				if err != nil {
					t.Fatalf("media blocked text: %v", err)
				}
				p.Release(l)
			}
		})
	}
}
func TestGrokSharedReservationSurvivesReleaseRestartAndRefresh(t *testing.T) {
	_, s := quotaPool(t, 100, 100, "正常")
	_, _, _ = s.UpdateAccount("token", map[string]any{"media_quota": grokSnapshot(1, 1)})
	p := NewGrok(s)
	var mu sync.Mutex
	var leases []*Lease
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			model := "grok-imagine-image-pro"
			if i%2 == 1 {
				model = "grok-imagine-image-edit"
			}
			l, err := reserveIntent(p, Intent{Kind: "image", Model: model})
			if err == nil {
				mu.Lock()
				leases = append(leases, l)
				mu.Unlock()
			} else if !errors.Is(err, ErrUnavailable) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if len(leases) != 1 {
		t.Fatalf("last shared credit reserved %d times", len(leases))
	}
	l := leases[0]
	called := false
	fetch := func(Account) (map[string]any, error) {
		called = true
		return map[string]any{"media_quota": grokSnapshot(0, 1)}, nil
	}
	if err := p.RefreshGrokMedia(l.Account, fetch); !errors.Is(err, ErrRefreshDeferred) || called {
		t.Fatal("refresh ran during generation")
	}
	p.MarkSent(l, "image_gen")
	p.Release(l)
	p = NewGrok(s)
	if _, err := reserveIntent(p, Intent{Kind: "image", Model: "grok-imagine-image-edit"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("restart refunded pending balance")
	}
	if err := p.RefreshGrokMedia(l.Account, fetch); !errors.Is(err, ErrRefreshDeferred) || called {
		t.Fatal("refresh skipped settlement delay")
	}
	_, _, _ = s.UpdateAccount("token", map[string]any{"media_quota_not_before": time.Now().Add(-time.Second).Format(time.RFC3339Nano)})
	if err := p.RefreshGrokMedia(l.Account, fetch); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveIntent(p, Intent{Kind: "image", Model: "grok-imagine-image-pro"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("exhausted image admitted")
	}
	video, err := reserveIntent(p, Intent{Kind: "video", Model: "grok-imagine-video"})
	if err != nil {
		t.Fatal(err)
	}
	p.Release(video)
	rows, _, _ := s.AccountSnapshot()
	if IsAbnormal(rows[0]) || len(pendingQuotas(rows[0]["quota_pending"])) > 0 && pendingQuotas(rows[0]["quota_pending"])["image_gen"] != 0 {
		t.Fatal("refresh corrupted state")
	}
}
func TestGrokMediaRefreshFailureKeepsReservationsAndBlocksOnlyMedia(t *testing.T) {
	_, s := quotaPool(t, 100, 100, "正常")
	_, _, _ = s.UpdateAccount("token", map[string]any{"media_quota": grokSnapshot(4, 1), "quota_pending": map[string]int{"image_gen": 1}})
	p := NewGrok(s)
	rows, _, _ := s.AccountSnapshot()
	a := Account{Token: "token", Fields: rows[0]}
	err := p.RefreshGrokMedia(a, func(Account) (map[string]any, error) {
		return map[string]any{"media_quota": map[string]any{"status": "failed", "quotas": map[string]any{}}}, errors.New("HTTP 403")
	})
	if err == nil {
		t.Fatal("expected failed query")
	}
	rows, _, _ = s.AccountSnapshot()
	if pendingQuotas(rows[0]["quota_pending"])["image_gen"] != 1 || IsAbnormal(rows[0]) {
		t.Fatal("media failure refunded/invalidated account")
	}
	if _, err := reserveIntent(p, Intent{Kind: "video", Model: "grok-imagine-video"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("stale media balance used")
	}
}
func TestGrokMediaDueAndCosts(t *testing.T) {
	now := time.Now()
	row := map[string]any{"media_quota": grokSnapshot(4, 1)}
	if GrokMediaRefreshDue(row, now) {
		t.Fatal("fresh idle account queried")
	}
	row["media_quota_refresh_needed"] = true
	row["media_quota_not_before"] = now.Add(time.Minute).Format(time.RFC3339Nano)
	if GrokMediaRefreshDue(row, now) {
		t.Fatal("settlement not respected")
	}
	delete(row, "media_quota_not_before")
	if !GrokMediaRefreshDue(row, now) {
		t.Fatal("used account not refreshed")
	}
	row["media_quota_last_attempt_at"] = now.Format(time.RFC3339Nano)
	if GrokMediaRefreshDue(row, now) {
		t.Fatal("failed refresh retry loop")
	}
	if c := (Intent{Kind: "image", Model: "grok-imagine-image-pro", ImageCount: 3}).Costs(); c["image_gen"] != 3 || c["reason"] != 0 {
		t.Fatalf("image costs=%v", c)
	}
	if c := (Intent{Kind: "video", Model: "grok-imagine-video", VideoSegments: 2, VideoUnits: 3}).Costs(); c["video_gen"] != 3 {
		t.Fatalf("video costs=%v", c)
	}
}

func TestGrokCrashBeforeReleaseCannotReuseUnsettledPositiveBalance(t *testing.T) {
	_, s := quotaPool(t, 100, 100, "正常")
	_, _, _ = s.UpdateAccount("token", map[string]any{"media_quota": grokSnapshot(4, 2)})
	p := NewGrok(s)
	l, err := reserveIntent(p, Intent{Kind: "image", Model: "grok-imagine-image-pro"})
	if err != nil {
		t.Fatal(err)
	}
	reopened := store.New(s.AccountsPath(), s.AuthKeysPath(), filepath.Join(filepath.Dir(s.AccountsPath()), "config.json"))
	restarted := NewGrok(reopened)
	if _, err := reserveIntent(restarted, Intent{Kind: "image", Model: "grok-imagine-image-edit"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("restart reused balance before settlement")
	}
	// An attempt stopped before any media request can safely return its reservation.
	p.Release(l)
	l, err = reserveIntent(p, Intent{Kind: "image", Model: "grok-imagine-image-edit"})
	if err != nil {
		t.Fatal(err)
	}
	p.Release(l)
}

func TestGrokMediaFailureRefreshWindows(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		reason string
		age    time.Duration
		due    bool
	}{
		{"HTTP 403", 14 * time.Minute, false}, {"HTTP 403", 16 * time.Minute, true},
		{"HTTP 401", time.Minute, false}, {"HTTP 429", time.Minute, false},
		{"network_error", 40 * time.Second, false}, {"network_error", 2 * time.Minute, true},
	} {
		row := map[string]any{"media_quota": map[string]any{"status": "failed", "error": tc.reason, "checked_at": now.Add(-tc.age).Format(time.RFC3339Nano)}, "media_quota_last_attempt_at": now.Add(-tc.age).Format(time.RFC3339Nano), "media_quota_refresh_needed": true}
		if got := GrokMediaRefreshDue(row, now); got != tc.due {
			t.Errorf("%s after %s: due=%v", tc.reason, tc.age, got)
		}
	}
}

func TestGrokMediaCheckPlanSeparatesIdlePollingFromPreflight(t *testing.T) {
	now := time.Now().UTC()
	row := map[string]any{"media_quota": grokSnapshot(3, 4)}
	snapshot := row["media_quota"].(map[string]any)
	snapshot["checked_at"] = now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
	if !GrokMediaCheckPlan(row, now, false).At.After(now) {
		t.Fatal("idle account polled every five minutes")
	}
	if !GrokMediaRefreshDue(row, now) {
		t.Fatal("on-demand refresh admitted stale idle quota")
	}
	row["quota_last_used_at"] = now.Add(-15 * time.Minute).Format(time.RFC3339Nano)
	if GrokMediaCheckPlan(row, now, false).At.After(now) {
		t.Fatal("active account not refreshed")
	}
	row["quota_last_intent"] = "video"
	snapshot["quotas"].(map[string]any)["video720p"] = map[string]any{"remaining": 0, "available": false, "next_available_at": now.Add(20 * time.Minute).Format(time.RFC3339Nano)}
	plan := GrokMediaCheckPlan(row, now, false)
	if plan.Reason != "reset_due" || !plan.At.Equal(now.Add(20*time.Minute+5*time.Second)) {
		t.Fatalf("reset plan: %+v", plan)
	}
	// An elapsed reset still requires a query; it cannot grant a reservation.
	snapshot["quotas"].(map[string]any)["video720p"] = map[string]any{"remaining": 0, "available": false, "next_available_at": now.Add(-5 * time.Minute).Format(time.RFC3339Nano)}
	if GrokMediaQuotas(row)["video_gen"].Remaining == nil || *GrokMediaQuotas(row)["video_gen"].Remaining != 0 {
		t.Fatal("reset fabricated credits")
	}
}

func TestGrokMediaRetryAfterAndConsecutiveFailures(t *testing.T) {
	now := time.Now().UTC()
	row := map[string]any{"media_quota": map[string]any{"status": "failed", "error": "HTTP 429", "checked_at": now.Format(time.RFC3339Nano), "retry_at": now.Add(2 * time.Hour).Format(time.RFC3339Nano)}, "media_quota_failure_count": 8}
	if at := GrokMediaCheckPlan(row, now, true).At; !at.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("Retry-After lost: %s", at)
	}
	delete(row["media_quota"].(map[string]any), "retry_at")
	if at := GrokMediaCheckPlan(row, now, false).At; !at.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("failure backoff lost: %s", at)
	}
}

func TestGrokMediaHistoryAndUnknownReservations(t *testing.T) {
	_, s := quotaPool(t, 100, 100, "正常")
	_, _, _ = s.UpdateAccount("token", map[string]any{"media_quota": grokSnapshot(0, 0), "quota_pending": map[string]int{"image_gen": 1, "video_gen": 2}})
	p := NewGrok(s)
	refresh := func(fields map[string]any, err error) {
		t.Helper()
		rows, _, _ := s.AccountSnapshot()
		_ = p.RefreshGrokMedia(Account{Token: "token", Fields: rows[0]}, func(Account) (map[string]any, error) { return fields, err })
	}
	refresh(map[string]any{"media_quota": map[string]any{"status": "failed", "error": "HTTP 429", "checked_at": time.Now().Format(time.RFC3339Nano)}}, errors.New("limited"))
	rows, _, _ := s.AccountSnapshot()
	if rows[0]["media_quota_last_success"] == nil || pendingQuotas(rows[0]["quota_pending"])["video_gen"] != 2 {
		t.Fatal("failed check lost history or pending usage")
	}
	partial := grokSnapshot(3, 4)
	delete(partial["quotas"].(map[string]any), "video720p")
	refresh(map[string]any{"media_quota": partial}, nil)
	rows, _, _ = s.AccountSnapshot()
	if pendingQuotas(rows[0]["quota_pending"])["video_gen"] != 2 || pendingQuotas(rows[0]["quota_pending"])["image_gen"] != 0 {
		t.Fatal("unknown balance settled video reservations")
	}
	if len(grokMediaHistory(rows[0], GrokMediaQuotas(rows[0]), time.Now())) != 1 {
		t.Fatal("recovery history missing or duplicate")
	}
	states := p.GrokMediaAvailability(rows[0], time.Now())
	if !states["image"].(map[string]any)["eligible"].(bool) || states["video"].(map[string]any)["eligible"].(bool) {
		t.Fatal("feature availability was not independent")
	}
}
