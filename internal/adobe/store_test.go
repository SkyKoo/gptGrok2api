package adobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func readyStore(t *testing.T) (*Store, Account) {
	t.Helper()
	s := NewStore(filepath.Join(t.TempDir(), "adobe_accounts.json.enc"), "private-encryption-secret")
	items, e := s.Import([]Account{fixtureAccount(t)})
	if e != nil {
		t.Fatal(e)
	}
	a := items[0]
	n := 10.0
	e = s.UpdateQuota(a.ID, a.Revision, Quota{Status: "known", Available: &n, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if e != nil {
		t.Fatal(e)
	}
	a, _ = s.Get(a.ID)
	return s, a
}
func TestEncryptedStorePublicProjectionAndWrongKey(t *testing.T) {
	s, a := readyStore(t)
	raw, e := os.ReadFile(s.path)
	if e != nil {
		t.Fatal(e)
	}
	for _, secret := range []string{a.Token, a.Cookie, a.Email} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("secret in plaintext store")
		}
	}
	pub, _ := s.PublicList()
	b, _ := json.Marshal(pub)
	for _, secret := range []string{a.Token, a.Cookie, "sherlockToken"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("credential in public response")
		}
	}
	wrong := NewStore(s.path, "wrong-key")
	if _, e = wrong.List(); e == nil {
		t.Fatal("wrong encryption key accepted")
	}
}

func TestAccountProxyChangesPreserveCredentialsAndSurviveImport(t *testing.T) {
	s, before := readyStore(t)
	binding := proxyruntime.Binding{Reference: "http://account.invalid:8080", Platform: "adobe", Purpose: "account", BoundAt: time.Now().UTC().Format(time.RFC3339Nano)}
	bound, err := s.BindProxy(before.ID, binding, "operator chooses a daily proxy")
	if err != nil {
		t.Fatal(err)
	}
	if bound.ProxyPolicy != "explicit" || bound.Proxy != binding.Reference || bound.ProxyBinding == nil || len(bound.ProxyBindingHistory) != 1 {
		t.Fatal("manual proxy or audit was not saved")
	}
	protected := bound
	protected.Proxy, protected.ProxyBinding, protected.ProxyPolicy, protected.ProxyBindingHistory = before.Proxy, before.ProxyBinding, before.ProxyPolicy, before.ProxyBindingHistory
	if !reflect.DeepEqual(before, protected) {
		t.Fatal("proxy update changed credentials, account status or quota")
	}
	imported, err := s.Import([]Account{before})
	if err != nil || len(imported) != 1 || imported[0].ProxyPolicy != "explicit" || imported[0].Proxy != binding.Reference || len(imported[0].ProxyBindingHistory) != 1 {
		t.Fatal("credential reimport replaced the operator's daily proxy")
	}
	binding.Reference = "inherit"
	restored, err := s.BindProxy(before.ID, binding, "follow the default again")
	if err != nil || restored.ProxyPolicy != "inherit" || restored.Proxy != "" || restored.ProxyBinding != nil || len(restored.ProxyBindingHistory) != 2 {
		t.Fatal("returning to the default did not clear the override and retain history")
	}
	fresh, err := NewStore(s.path, s.secret).Get(before.ID)
	if err != nil || fresh.ProxyPolicy != "inherit" || len(fresh.ProxyBindingHistory) != 2 {
		t.Fatal("default inheritance did not survive restart")
	}
}
func TestReservationSharedAcrossModelsAndSurvivesRestart(t *testing.T) {
	s, a := readyStore(t)
	got, e := s.Reserve("image-task")
	if e != nil || got.ID != a.ID {
		t.Fatal(e)
	}
	restarted := NewStore(s.path, s.secret)
	if _, e = restarted.Reserve("video-task"); !errors.Is(e, ErrNoAccount) {
		t.Fatal("concurrent shared quota reservation")
	}
	if _, e = restarted.Reserve("image-task"); e == nil {
		t.Fatal("duplicate task allowed to regenerate")
	}
	if e = s.MarkUnknown("image-task"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Refresh(context.Background(), NewClient(nil), a.ID, false); !errors.Is(e, ErrBusy) {
		t.Fatal("unknown submission reservation released")
	}
	if e = s.Release("image-task"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Reserve("image-task"); e == nil {
		t.Fatal("finished task can replay")
	}
	if _, e = s.Reserve("new-task"); !errors.Is(e, ErrNoAccount) {
		t.Fatal("settlement barrier missing")
	}
}
func TestQuotaRevisionCASAndImportKeepManualDisabled(t *testing.T) {
	s, a := readyStore(t)
	if e := s.SetEnabled([]string{a.ID}, false); e != nil {
		t.Fatal(e)
	}
	items, e := s.Import([]Account{fixtureAccount(t)})
	if e != nil {
		t.Fatal(e)
	}
	if items[0].Enabled || items[0].Revision != a.Revision+1 {
		t.Fatal("reimport reenabled manually disabled account")
	}
	if e = s.UpdateQuota(a.ID, a.Revision, Quota{Status: "known"}); !errors.Is(e, ErrRevision) {
		t.Fatal("stale quota overwrote new credential")
	}
}
func TestResumeRequiresExistingBindingAndHandle(t *testing.T) {
	s, a := readyStore(t)
	_, e := s.Reserve("task")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Resume("task", a.ID); e == nil {
		t.Fatal("resume without submitted handle")
	}
	j := Job{PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/id", Kind: "image"}
	if e = s.SaveJob("task", j); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(s.jobPath("task"))
	if strings.Contains(string(raw), j.PollURL) {
		t.Fatal("poll URL not encrypted")
	}
	if _, e = s.Resume("task", a.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Resume("task", "other-account"); e == nil {
		t.Fatal("route changed during resume")
	}
}
func TestRefreshFailurePreservesObservedBalanceAndDisablesSelection(t *testing.T) {
	s, a := readyStore(t)
	c := NewClient(&http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return jsonResponse(429, `{"error_code":"rate_limit_exceeded"}`), nil
	})})
	if _, e := s.Refresh(context.Background(), c, a.ID, false); e == nil {
		t.Fatal("expected quota failure")
	}
	got, _ := s.Get(a.ID)
	if got.Quota.Status != "error" || got.Quota.Available == nil || *got.Quota.Available != 10 || Eligible(got, time.Now()) {
		t.Fatalf("lost failure/old quota distinction: %+v", got.Quota)
	}
}
func TestRefreshCASIdentityPreservesOriginalAccount(t *testing.T) {
	s, a := readyStore(t)
	bad := a
	bad.AccountID = "other"
	bad.Token = fixtureAccount(t).Token
	bad.Token = strings.Replace(bad.Token, "e30.", "invalid.", 1)
	bad.Token = "invalid"
	if _, e := s.ReplaceCredentials(a, bad); e == nil {
		t.Fatal("invalid replacement accepted")
	}
	got, _ := s.Get(a.ID)
	if got.Token != a.Token || got.Revision != a.Revision {
		t.Fatal("invalid refresh modified credentials")
	}
}

func TestReadOnly401RefreshesCredentialOnceWithoutGeneration(t *testing.T) {
	s, a := readyStore(t)
	a.RefreshCookie = "imsSession=private"
	if _, e := s.ReplaceCredentials(a, a); e != nil {
		t.Fatal(e)
	}
	a, _ = s.Get(a.ID)
	balanceCalls, refreshCalls := 0, 0
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "credits/balance") {
			balanceCalls++
			if balanceCalls == 1 {
				return jsonResponse(401, `{}`), nil
			}
			return jsonResponse(200, `{"total":{"quota":{"available":10}}}`), nil
		}
		if strings.Contains(r.URL.Path, "check/v6/token") {
			refreshCalls++
			raw, _ := json.Marshal(map[string]string{"access_token": a.Token})
			return jsonResponse(200, string(raw)), nil
		}
		t.Fatal("unexpected generation request")
		return nil, nil
	})})
	fresh, e := s.Refresh(context.Background(), c, a.ID, false)
	if e != nil || refreshCalls != 1 || balanceCalls != 2 || fresh.Revision != a.Revision+1 || fresh.Quota.Status != "known" {
		t.Fatalf("read-only recovery invalid: refresh=%d balance=%d err=%v", refreshCalls, balanceCalls, e)
	}
}
func TestBrowserRefreshFallbackUsesSameIdentityAndMaintenanceGuard(t *testing.T) {
	s, a := readyStore(t)
	a.ProfileID = "saved-profile"
	items, e := s.Import([]Account{a})
	if e != nil {
		t.Fatal(e)
	}
	a = items[0]
	browserCalls := 0
	s.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
		browserCalls++
		if _, e := s.Reserve("parallel-request"); !errors.Is(e, ErrNoAccount) {
			t.Error("browser refresh allowed parallel media")
		}
		return verifiedBrowserAccount(current), nil
	})
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "credits/balance") {
			t.Fatal("unexpected scoped-cookie request")
		}
		return jsonResponse(200, `{"total":{"quota":{"available":10}}}`), nil
	})})
	fresh, e := s.Refresh(context.Background(), c, a.ID, true)
	if e != nil || browserCalls != 1 || fresh.Revision != a.Revision+1 {
		t.Fatalf("browser fallback: %v calls=%d", e, browserCalls)
	}
}

func TestImportPreservesRegistrationOwner(t *testing.T) {
	s, _ := readyStore(t)
	a := fixtureAccount(t)
	a.RegistrationJobID = "registration-owner"
	rows, e := s.Import([]Account{a})
	if e != nil {
		t.Fatal(e)
	}
	a = rows[0]
	a.RegistrationJobID = ""
	rows, e = s.Import([]Account{a})
	if e != nil || rows[0].RegistrationJobID != "registration-owner" {
		t.Fatal("reimport dropped recovery ownership")
	}
}
func TestRefreshTaskKeepsPinnedReservationAndNeverGenerates(t *testing.T) {
	s, a := readyStore(t)
	a.RefreshCookie = "imsSession=private"
	a, e := s.ReplaceCredentials(a, a)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Reserve("pending-video"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.RefreshTask(context.Background(), NewClient(nil), "pending-video", a.ID); e == nil {
		t.Fatal("missing handle should refuse refresh")
	}
	if e = s.SaveJob("pending-video", Job{Kind: "video", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/pinned"}); e != nil {
		t.Fatal(e)
	}
	calls := 0
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.Contains(r.URL.Path, "check/v6/token") {
			t.Fatal("task refresh queried quota or submitted media")
		}
		if _, e := s.Reserve("other-task"); !errors.Is(e, ErrNoAccount) {
			t.Error("task refresh released account")
		}
		raw, _ := json.Marshal(map[string]string{"access_token": a.Token})
		return jsonResponse(200, string(raw)), nil
	})})
	fresh, e := s.RefreshTask(context.Background(), c, "pending-video", a.ID)
	if e != nil || calls != 1 || fresh.InFlight != "pending-video" || fresh.Revision != a.Revision+1 {
		t.Fatalf("pinned refresh failed: %v", e)
	}
	if _, e = s.RefreshTask(context.Background(), c, "other-task", a.ID); e == nil {
		t.Fatal("unbound task allowed refresh")
	}
}

func TestTokenRefreshPreservesWholeMediaCookieWhenMissing(t *testing.T) {
	s, a := readyStore(t)
	replacement := a
	replacement.Cookie = "unrelatedCookie=value"
	replacement.RefreshCookie = "imsSession=new-private"
	fresh, e := s.ReplaceCredentials(a, replacement)
	if e != nil {
		t.Fatal(e)
	}
	if fresh.Cookie != a.Cookie || fresh.RefreshCookie != replacement.RefreshCookie || fresh.MediaSessionStatus != "preserved" || fresh.Revision != a.Revision+1 {
		t.Fatal("missing media token replaced/mixed with known media cookie")
	}
	if Public(fresh)["media_session_refreshed"] != false {
		t.Fatal("token refresh falsely marked media renewed")
	}
	empty := fresh
	empty.Cookie = ""
	again, e := s.ReplaceCredentials(fresh, empty)
	if e != nil || again.Cookie != a.Cookie || again.MediaSessionStatus != "preserved" {
		t.Fatal("empty cookie did not retain prior complete session")
	}
}
func TestMediaCookieRefreshRequiresCompleteOldOrNew(t *testing.T) {
	s, a := readyStore(t)
	if e := s.update(a.ID, a.Revision, func(v *Account) { v.Cookie = "partial" }); e != nil {
		t.Fatal(e)
	}
	a, _ = s.Get(a.ID)
	newToken := a
	newToken.Cookie = ""
	if _, e := s.ReplaceCredentials(a, newToken); e == nil {
		t.Fatal("no valid media session accepted")
	}
	newToken = fixtureAccount(t)
	newToken.Cookie += "; freshCookie=yes"
	fresh, e := s.ReplaceCredentials(a, newToken)
	if e != nil || fresh.MediaSessionStatus != "refreshed" || Public(fresh)["media_session_refreshed"] != true {
		t.Fatalf("complete new session not recorded: %v", e)
	}
}

func TestFreePreflightEligibilityDoesNotRequireGeneralCredits(t *testing.T) {
	_, a := readyStore(t)
	zero := 0.0
	a.Quota.Available = &zero
	if !Eligible(a, time.Now()) {
		t.Fatal("zero general credits blocked independent daily free pools")
	}
	a.Quota = Quota{Status: "unknown"}
	if !Eligible(a, time.Now()) {
		t.Fatal("unrelated credit query blocked free generation preflight")
	}
	a.InFlight = "unknown-submission"
	if Eligible(a, time.Now()) {
		t.Fatal("free preflight eligibility bypassed account binding")
	}
}
