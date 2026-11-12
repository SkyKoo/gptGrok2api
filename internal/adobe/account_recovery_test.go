package adobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func queryFixtureClient(t *testing.T, beforeResponse func()) *Client {
	t.Helper()
	return NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/credits/balance":
			if r.Method != "GET" {
				t.Error("balance is not a read")
			}
			if beforeResponse != nil {
				beforeResponse()
			}
			return jsonResponse(200, `{"total":{"quota":{"available":7,"used":3,"total":10}}}`), nil
		case "/v1/feature-auth-profile":
			raw, _ := json.Marshal(entitlementFixture())
			return jsonResponse(200, string(raw)), nil
		default:
			t.Errorf("quota query invoked unexpected upstream operation: %s", r.URL.Path)
			return jsonResponse(500, `{}`), nil
		}
	})})
}

func TestQueryQuotaPreservesUnknownAccountAndNoCredentialRefresh(t *testing.T) {
	s, a := readyStore(t)
	_, _ = s.Reserve("existing-task")
	_ = s.MarkUnknown("existing-task")
	a, _ = s.Get(a.ID)
	s.SetBrowserRefresher(func(context.Context, Account) (Account, error) {
		t.Error("quota read opened browser")
		return Account{}, nil
	})
	updated, err := s.QueryQuota(context.Background(), queryFixtureClient(t, nil), a.ID)
	if err != nil || updated.Quota.Available == nil || *updated.Quota.Available != 7 || updated.Quota.Generation.Status != "known" {
		t.Fatalf("query failed: %v", err)
	}
	protected := updated
	protected.Quota, protected.QuotaRevision = a.Quota, a.QuotaRevision
	if !reflect.DeepEqual(a, protected) {
		t.Fatal("quota read changed credentials, errors, timestamps or pinned task")
	}
	if Eligible(updated, time.Now()) {
		t.Fatal("quota query released unknown reservation")
	}
	pub := Public(updated)
	if pub["login_status"] != "valid" || pub["task_status"] != "submission_unknown" || pub["task_key"] != "existing-task" || pub["can_query_quota"] != true {
		t.Fatal("login and task states remain coupled")
	}
}

func TestQueryQuotaRejectsStaleConcurrentResult(t *testing.T) {
	for _, change := range []string{"credentials", "settlement", "new-quota", "maintenance"} {
		t.Run(change, func(t *testing.T) {
			s, a := readyStore(t)
			_, _ = s.Reserve("existing-task")
			_ = s.MarkUnknown("existing-task")
			a, _ = s.Get(a.ID)
			var expected Account
			client := queryFixtureClient(t, func() {
				switch change {
				case "credentials":
					if _, err := s.ReplaceCredentials(a, a); err != nil {
						t.Fatal(err)
					}
				case "settlement":
					if err := s.Release("existing-task"); err != nil {
						t.Fatal(err)
					}
				case "new-quota":
					n := 4.0
					if err := s.UpdateQuota(a.ID, a.Revision, Quota{Status: "known", Available: &n}); err != nil {
						t.Fatal(err)
					}
				case "maintenance":
					s.mu.Lock()
					s.maintenance[a.ID] = true
					s.mu.Unlock()
				}
				expected, _ = s.Get(a.ID)
			})
			if _, err := s.QueryQuota(context.Background(), client, a.ID); !errors.Is(err, ErrResolutionConflict) {
				t.Fatalf("stale read accepted: %v", err)
			}
			got, _ := s.Get(a.ID)
			if !reflect.DeepEqual(got, expected) {
				t.Fatal("stale query changed the newer account")
			}
		})
	}
}

func TestQueryQuotaAuthenticationFailureDoesNotRefreshOrLoseHistory(t *testing.T) {
	s, a := readyStore(t)
	_, _ = s.Reserve("existing-task")
	_ = s.MarkUnknown("existing-task")
	a, _ = s.Get(a.ID)
	calls := 0
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/v1/credits/balance" && r.URL.Path != "/v1/feature-auth-profile" {
			t.Error("query refreshed/submitted on unauthorized")
		}
		return jsonResponse(401, `{}`), nil
	})})
	got, err := s.QueryQuota(context.Background(), c, a.ID)
	if err == nil || calls != 2 || got.Quota.Status != "error" || got.Quota.Available == nil || *got.Quota.Available != *a.Quota.Available || got.Status != "submission_unknown" || got.InFlight != a.InFlight || got.LastError != a.LastError || got.Token != a.Token {
		t.Fatal("query failure lost state or retried credentials")
	}
}

func TestResolveSubmissionAtomicallyKeepsAuditAndPreventsRecovery(t *testing.T) {
	s, a := readyStore(t)
	_, _ = s.Reserve("existing-task")
	_ = s.SaveJob("existing-task", Job{Kind: "image", Model: "gemini-3.1-flash-image", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: "SUBMITTED"})
	_ = s.MarkUnknown("existing-task")
	a, _ = s.Get(a.ID)
	for _, invalid := range []struct {
		task    string
		rev     uint64
		confirm bool
		reason  string
	}{
		{"other-task", a.Revision, true, "Reviewed upstream history"},
		{"existing-task", a.Revision + 1, true, "Reviewed upstream history"},
		{"existing-task", a.Revision, false, "Reviewed upstream history"},
		{"existing-task", a.Revision, true, "short"},
	} {
		if _, _, err := s.ResolveSubmission(a.ID, invalid.task, invalid.rev, invalid.confirm, invalid.reason, "admin", "terminal"); err == nil {
			t.Fatal("invalid resolution accepted")
		}
	}
	resolved, audit, err := s.ResolveSubmission(a.ID, "existing-task", a.Revision, true, "Reviewed Adobe history; release local occupancy", "admin", "terminal")
	if err != nil || resolved.InFlight != "" || resolved.Status == "submission_unknown" || audit.TaskKey != "existing-task" {
		t.Fatalf("resolution failed %v", err)
	}
	if resolved.Token != a.Token || resolved.Cookie != a.Cookie || resolved.Enabled != a.Enabled || resolved.RegistrationMailbox != nil || resolved.Quota.Status != "stale" {
		t.Fatal("resolution changed credentials/account control or invented quota")
	}
	restarted := NewStore(s.path, s.secret)
	if _, err := restarted.Resume("existing-task", a.ID); !errors.Is(err, ErrTaskResolved) {
		t.Fatalf("resolved task resumed: %v", err)
	}
	if _, err := restarted.RefreshTask(context.Background(), NewClient(nil), "existing-task", a.ID); !errors.Is(err, ErrTaskResolved) {
		t.Fatalf("resolved task refreshed: %v", err)
	}
	if err := restarted.SaveJob("existing-task", Job{}); !errors.Is(err, ErrTaskResolved) {
		t.Fatal("resolved handle overwritten")
	}
	if _, err := restarted.Reserve("existing-task"); err == nil {
		t.Fatal("resolved task regenerated")
	}
	records, err := restarted.SubmissionResolutions(a.ID)
	if err != nil || len(records) != 1 || records[0].PreviousError != "adobe_submission_unknown" {
		t.Fatal("resolution audit lost")
	}
	job, err := restarted.LoadJob("existing-task")
	if err != nil || job.Status != "SUBMITTED" {
		t.Fatal("resolution invented upstream completion or deleted original handle")
	}
}

func TestResolveSubmissionFailedPersistenceKeepsReservation(t *testing.T) {
	s, a := readyStore(t)
	_, _ = s.Reserve("existing-task")
	_ = s.MarkUnknown("existing-task")
	originalPath := s.path
	// Read the existing file through its descriptor; atomic replacement in
	// /dev/fd fails even when tests run as root inside Docker.
	f, err := os.Open(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s.path = "/dev/fd/" + fmt.Sprint(f.Fd())
	if _, err := os.Stat(s.path); err != nil {
		t.Skip("descriptor paths unavailable")
	}
	if _, _, err := s.ResolveSubmission(a.ID, "existing-task", a.Revision, true, "Reviewed upstream history", "admin", "terminal"); err == nil {
		t.Fatal("persistence failure accepted")
	}
	fresh, err := NewStore(originalPath, s.secret).Get(a.ID)
	if err != nil || fresh.InFlight != "existing-task" || fresh.Status != "submission_unknown" {
		t.Fatal("failed save released reservation")
	}
}

func TestResolveSubmissionRetainsExpiredDisabledEligibility(t *testing.T) {
	s, a := readyStore(t)
	_, _ = s.Reserve("task")
	_ = s.MarkUnknown("task")
	_ = s.SetEnabled([]string{a.ID}, false)
	_ = s.update(a.ID, a.Revision, func(a *Account) { a.ExpiresAt = time.Now().Add(-time.Hour).Unix() })
	got, _, err := s.ResolveSubmission(a.ID, "task", a.Revision, true, "Reviewed upstream history", "admin", "terminal")
	if err != nil || got.Enabled || Eligible(got, time.Now().Add(time.Minute)) || Public(got)["login_status"] != "expired" {
		t.Fatal("manual resolution enabled invalid account")
	}
}

func TestQueryQuotaPreventsDeleteReimportABA(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "adobe.enc"), "fixture-secret")
	items, err := s.Import([]Account{fixtureAccount(t)})
	if err != nil {
		t.Fatal(err)
	}
	a := items[0] // Pending, credential revision 1 and quota revision 0.
	client := queryFixtureClient(t, func() {
		if _, err := s.Delete([]string{a.ID}); !errors.Is(err, ErrBusy) {
			t.Fatalf("deleted identity during quota read: %v", err)
		}
		if _, err := s.Import([]Account{fixtureAccount(t)}); !errors.Is(err, ErrBusy) {
			t.Fatalf("reimported identity during quota read: %v", err)
		}
	})
	got, err := s.QueryQuota(context.Background(), client, a.ID)
	if err != nil || got.Revision != a.Revision || got.Quota.Status != "known" {
		t.Fatalf("stable identity query failed: %v", err)
	}
	if _, err := s.Delete([]string{a.ID}); err != nil {
		t.Fatalf("query lock leaked after completion: %v", err)
	}
	fresh, err := s.Import([]Account{fixtureAccount(t)})
	if err != nil || fresh[0].Revision != 1 || fresh[0].Quota.Status != "unknown" {
		t.Fatal("new incarnation inherited old snapshot")
	}
}

func TestRecordQuotaQueryFailurePreservesSuccessfulValuesAndCredentials(t *testing.T) {
	s, a := readyStore(t)
	if _, err := s.QueryQuota(context.Background(), queryFixtureClient(t, nil), a.ID); err != nil {
		t.Fatal(err)
	}
	a, _ = s.Get(a.ID)
	now := time.Now().UTC()
	got, err := s.RecordQuotaQueryFailure(a, "proxy_unavailable", now)
	if err != nil || got.QuotaRevision != a.QuotaRevision+1 || got.Quota.Status != "error" || got.Quota.Error != "adobe_routing_proxy_unavailable" || got.Quota.CheckedAt != now.Format(time.RFC3339Nano) || got.Quota.Generation.Status != "error" || got.Quota.Generation.CheckedAt != got.Quota.CheckedAt {
		t.Fatalf("routing observation was not persisted: %v", err)
	}
	expectedQuota := a.Quota
	expectedQuota.Status, expectedQuota.Error, expectedQuota.CheckedAt = "error", "adobe_routing_proxy_unavailable", now.Format(time.RFC3339Nano)
	expectedGeneration := *a.Quota.Generation
	expectedGeneration.Status, expectedGeneration.Error, expectedGeneration.CheckedAt = "error", expectedQuota.Error, expectedQuota.CheckedAt
	expectedQuota.Generation = &expectedGeneration
	if !reflect.DeepEqual(got.Quota, expectedQuota) {
		t.Fatal("routing failure erased the last successful quota values")
	}
	got.Quota, got.QuotaRevision = a.Quota, a.QuotaRevision
	if !reflect.DeepEqual(got, a) {
		t.Fatal("routing observation changed login, task, proxy or account controls")
	}
	restarted := NewStore(s.path, s.secret)
	persisted, _ := restarted.Get(a.ID)
	if !reflect.DeepEqual(persisted.Quota, expectedQuota) {
		t.Fatal("routing-failure cooldown was lost after restart")
	}
	if _, err = s.RecordQuotaQueryFailure(persisted, "Bearer private-token", now); !errors.Is(err, ErrResolutionReview) {
		t.Fatal("unbounded failure text accepted")
	}
}

func TestRecordQuotaQueryFailureRejectsChangedAccountAndConcurrentWork(t *testing.T) {
	for _, change := range []string{"credentials", "quota", "task", "status", "settlement", "last-used", "route", "maintenance", "query"} {
		t.Run(change, func(t *testing.T) {
			s, a := readyStore(t)
			var err error
			switch change {
			case "credentials":
				_, err = s.ReplaceCredentials(a, a)
			case "quota":
				err = s.UpdateQuota(a.ID, a.Revision, a.Quota)
			case "task":
				_, err = s.Reserve("new-task")
			case "status":
				err = s.update(a.ID, a.Revision, func(a *Account) { a.Status = "invalid" })
			case "settlement":
				err = s.update(a.ID, a.Revision, func(a *Account) { a.SettlementUntil = time.Now().Add(time.Minute).Unix() })
			case "last-used":
				err = s.update(a.ID, a.Revision, func(a *Account) { a.LastUsedAt++ })
			case "route":
				_, err = s.BindProxy(a.ID, proxyruntime.Binding{Reference: "direct", Platform: "adobe", Purpose: "account"}, "fixture")
			case "maintenance":
				s.mu.Lock()
				s.maintenance[a.ID] = true
				s.mu.Unlock()
			case "query":
				s.mu.Lock()
				s.quotaQueries[a.ID] = true
				s.mu.Unlock()
			}
			if err != nil {
				t.Fatal(err)
			}
			before, _ := s.Get(a.ID)
			if _, err := s.RecordQuotaQueryFailure(a, "proxy_unavailable", time.Now()); err == nil {
				t.Fatal("outdated routing failure overwrote changed account")
			}
			after, _ := s.Get(a.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected routing observation changed persisted state")
			}
		})
	}
}
