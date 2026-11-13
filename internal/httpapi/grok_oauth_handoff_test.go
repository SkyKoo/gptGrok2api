package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

type xaiTestCPA struct {
	mu           sync.Mutex
	rows         map[string]map[string]any
	uploads      int
	postMode     string
	postStarted  chan struct{}
	postContinue chan struct{}
}

func installXAITestCPA(t *testing.T, s *Server) *xaiTestCPA {
	t.Helper()
	state := &xaiTestCPA{rows: map[string]map[string]any{}}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/v0/management/auth-files":
			files := []map[string]any{}
			for name, row := range state.rows {
				files = append(files, map[string]any{"name": name, "provider": row["type"], "email": row["email"]})
			}
			json.NewEncoder(w).Encode(map[string]any{"files": files})
		case r.Method == "GET" && r.URL.Path == "/v0/management/auth-files/download":
			row, ok := state.rows[r.URL.Query().Get("name")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(row)
		case r.Method == "POST" && r.URL.Path == "/v0/management/auth-files":
			state.uploads++
			if state.postStarted != nil {
				close(state.postStarted)
				<-state.postContinue
			}
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer file.Close()
			var row map[string]any
			json.NewDecoder(file).Decode(&row)
			if state.postMode != "uncommitted" {
				state.rows[header.Filename] = row
			}
			if state.postMode != "" {
				w.WriteHeader(500)
				return
			}
			w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	if err := s.external.saveCPAList([]externalCPAPool{{ID: "test-cpa", Name: "CPA Test", BaseURL: remote.URL, SecretKey: "fixture-management"}}); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestXAIManagedCopiesNeverRefreshOrProbe(t *testing.T) {
	for _, receipt := range []*oauth.CPASync{
		{Status: "synced", PoolID: "test-cpa", FileName: "xai-test@example.test.json", At: 1},
		{Status: "pending", PoolID: "test-cpa", FileName: "xai-test@example.test.json", At: 1, Error: "凭据已刷新，请重新同步"},
		{Status: "unknown", PoolID: "test-cpa", AttemptedAt: 1},
	} {
		s := New(adminTestConfig(t.TempDir()))
		var calls int
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
		s.xaiProbe = provider.NewXAIProbe(upstream.URL, upstream.URL+"/token", upstream.Client())
		item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "old", RefreshToken: "refresh", ExpiresAt: 1, CPASync: receipt})
		for _, action := range []string{"quota", "refresh", "test", "models/sync"} {
			response := adminRequest(s.Handler(), "POST", "/api/grok/oauth/accounts/"+item.ID+"/"+action, strings.NewReader(`{}`))
			if response.Code != 409 {
				t.Fatalf("managed %s accepted: %d", action, response.Code)
			}
		}
		if calls != 0 {
			t.Fatal("managed copy contacted upstream")
		}
		stored, _, _ := s.oauthStore.Get(item.ID)
		if stored.AccessToken != "old" || stored.RefreshToken != "refresh" {
			t.Fatal("local retained credential changed")
		}
		upstream.Close()
	}
}

func TestXAILegacyCheckAdoptsCPAWithoutOverwrite(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	remote := installXAITestCPA(t, s)
	remote.rows["xai-test@example.test.json"] = map[string]any{"type": "xai", "email": "test@example.test", "access_token": "CPA-NEW", "refresh_token": "CPA-NEW-REFRESH"}
	item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "old", RefreshToken: "old-refresh", ExpiresAt: 1, Status: "invalid", CPASync: &oauth.CPASync{Status: "pending", PoolID: "test-cpa", FileName: "xai-test@example.test.json", At: 10}})
	updated, err := s.syncGrokOAuthCPA(context.Background(), item.ID, "test-cpa", true)
	if err != nil || updated.CPASync.Status != "synced" || !oauth.CPAManaged(updated) || remote.uploads != 0 || updated.AccessToken != "old" {
		t.Fatalf("legacy adoption failed: %v", err)
	}
	_, _, _ = s.registerStore.AddAccount(map[string]any{"id": "test-source", "email": "test@example.test", "sso": "fixture-sso"})
	response := adminRequest(s.Handler(), "GET", "/api/grok/oauth/candidates", nil)
	if !strings.Contains(response.Body.String(), `"oauth_status":"authorized"`) {
		t.Fatal("stale retained expiry hides CPA ownership")
	}
}

func TestXAIUnknownDeliveryNeverRepostsOrAcceptsOldRemote(t *testing.T) {
	for _, mode := range []string{"committed", "uncommitted"} {
		t.Run(mode, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			remote := installXAITestCPA(t, s)
			remote.postMode = mode
			remote.rows["xai-test@example.test.json"] = map[string]any{"type": "xai", "email": "test@example.test", "access_token": "old-remote", "refresh_token": "old-remote-refresh"}
			item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "new", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), CPASync: &oauth.CPASync{Status: "pending", PoolID: "test-cpa", ReplaceExisting: true, AuthorizationTaskID: "new-grant"}})
			updated, err := s.syncGrokOAuthCPA(context.Background(), item.ID, "test-cpa", false)
			if mode == "committed" {
				if err != nil || updated.CPASync.Status != "synced" {
					t.Fatalf("committed response loss not recovered: %v", err)
				}
			} else {
				if err == nil || updated.CPASync.Status != "unknown" {
					t.Fatal("older same identity accepted as new delivery")
				}
				for _, check := range []bool{false, true} {
					current, err := s.syncGrokOAuthCPA(context.Background(), item.ID, "test-cpa", check)
					if err == nil || current.CPASync.Status != "unknown" {
						t.Fatal("unconfirmed delivery falsely resolved")
					}
				}
			}
			if remote.uploads != 1 {
				t.Fatal("upload was repeated")
			}
		})
	}
}

func TestXAIStartPinsTargetBeforeAnyGrant(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	_, _, _ = s.registerStore.AddAccount(map[string]any{"id": "selected", "email": "test@example.test", "sso": "fixture"})
	if _, _, err := s.startGrokOAuth("selected"); err == nil {
		t.Fatal("accepted no CPA target")
	}
	if len(s.taskQueue.List()) != 0 {
		t.Fatal("missing target created task")
	}
	installXAITestCPA(t, s)
	job, _, err := s.startGrokOAuth("selected", "test-cpa")
	if err != nil {
		t.Fatal(err)
	}
	if job.Payload["cpa_pool_id"] != "test-cpa" {
		t.Fatal("target not pinned")
	}
	pools := s.external.listCPA()
	other := pools[0]
	other.ID = "other"
	s.external.saveCPAList(append(pools, other))
	if _, _, err = s.startGrokOAuth("selected", "other"); err == nil {
		t.Fatal("duplicate changed pinned target")
	}
}

func TestXAIRegistrationAuthorizationRequiresAndPinsCPA(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		pools  int
		target string
		queued bool
	}{
		{name: "missing_pool"},
		{name: "single_pool_default", pools: 1, queued: true},
		{name: "ambiguous_pool", pools: 2},
		{name: "selected_pool", pools: 2, target: "other-cpa", queued: true},
		{name: "unknown_pool", pools: 2, target: "not-configured"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			_, _, _ = s.registerStore.AddAccount(map[string]any{"id": "selected", "email": "test@example.test", "sso": "fixture"})
			if scenario.pools > 0 {
				installXAITestCPA(t, s)
			}
			if scenario.pools > 1 {
				pools := s.external.listCPA()
				other := pools[0]
				other.ID = "other-cpa"
				if err := s.external.saveCPAList(append(pools, other)); err != nil {
					t.Fatal(err)
				}
			}
			body, _ := json.Marshal(map[string]any{"ids": []string{"selected", "selected"}, "cpa_pool_id": scenario.target})
			response := adminRequest(s.Handler(), "POST", "/api/register/grok/accounts/oauth/authorize", strings.NewReader(string(body)))
			if response.Code != http.StatusOK {
				t.Fatalf("unexpected response: %d", response.Code)
			}
			var result struct {
				Summary map[string]int `json:"summary"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			jobs := s.taskQueue.List()
			if !scenario.queued {
				if result.Summary["failed"] != 1 || len(jobs) != 0 {
					t.Fatal("invalid CPA selection created an authorization task")
				}
				return
			}
			wantTarget := scenario.target
			if wantTarget == "" {
				wantTarget = "test-cpa"
			}
			if result.Summary["queued"] != 1 || len(jobs) != 1 || jobs[0].Payload["cpa_pool_id"] != wantTarget {
				t.Fatalf("registration authorization did not pin selected target: %s", response.Body.String())
			}
		})
	}
}

func TestXAIRecoveryDeliversSavedGrantWithoutAuthorizingAgain(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		s := New(adminTestConfig(t.TempDir()))
		remote := installXAITestCPA(t, s)
		_, _, _ = s.registerStore.AddAccount(map[string]any{"id": "selected", "email": "test@example.test", "sso": "fixture"})
		job, _, err := s.startGrokOAuth("selected", "test-cpa")
		if err != nil {
			t.Fatal(err)
		}
		cp := &oauth.CPASync{Status: "pending", PoolID: "test-cpa", AuthorizationTaskID: job.ID, ReplaceExisting: true}
		if attempted {
			cp.Status = "uploading"
			cp.AttemptedAt = 1
			remote.rows["xai-test@example.test.json"] = map[string]any{"type": "xai", "email": "test@example.test", "access_token": "saved", "refresh_token": "saved-refresh"}
		}
		s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "saved", RefreshToken: "saved-refresh", CPASync: cp})
		s.registerStore.SetOAuthAuthorization("selected", "running", "")
		value, err := s.runGrokOAuthTask(job)
		if err != nil || value["status"] != "authorized" || value["cpa_error"] != nil {
			t.Fatalf("recovery %v %v", value, err)
		}
		want := 1
		if attempted {
			want = 0
		}
		if remote.uploads != want {
			t.Fatal("recovery replayed upload")
		}
	}
}

func TestXAIImportWaitsForOldRemoteDelivery(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	remote := installXAITestCPA(t, s)
	remote.postStarted = make(chan struct{})
	remote.postContinue = make(chan struct{})
	item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "old", RefreshToken: "old-refresh", CPASync: &oauth.CPASync{Status: "pending", PoolID: "test-cpa", ReplaceExisting: true}})
	synced := make(chan error, 1)
	go func() { _, err := s.syncGrokOAuthCPA(context.Background(), item.ID, "test-cpa", false); synced <- err }()
	select {
	case <-remote.postStarted:
	case <-time.After(time.Second):
		t.Fatal("delivery did not start")
	}
	imported := make(chan error, 1)
	go func() {
		_, err := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "new", RefreshToken: "new-refresh", CPASync: &oauth.CPASync{Status: "pending", PoolID: "test-cpa", ReplaceExisting: true, AuthorizationTaskID: "next-grant"}})
		imported <- err
	}()
	select {
	case <-imported:
		t.Fatal("new grant replaced local while old upload in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(remote.postContinue)
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	if err := <-imported; err != nil {
		t.Fatal(err)
	}
	saved, _, _ := s.oauthStore.Get(item.ID)
	if saved.AccessToken != "new" || saved.CPASync.AuthorizationTaskID != "next-grant" || saved.CPASync.Status != "pending" {
		t.Fatal("old completion marked new grant synced")
	}
}

func TestXAIVerifiedRecoveryAcceptsCPARotation(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	remote := installXAITestCPA(t, s)
	remote.rows["xai-test@example.test.json"] = map[string]any{"type": "xai", "email": "test@example.test", "access_token": "rotated", "refresh_token": "rotated-refresh"}
	item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "old", RefreshToken: "old-refresh", CPASync: &oauth.CPASync{Status: "synced", PoolID: "test-cpa", Managed: true, AttemptedAt: 1, VerifiedAt: 2, At: 2}})
	saved, err := s.syncGrokOAuthCPA(context.Background(), item.ID, "test-cpa", false)
	if err != nil || saved.CPASync.Status != "synced" || remote.uploads != 0 {
		t.Fatal("verified receipt was downgraded after normal CPA rotation")
	}
}
