package httpapi

import (
	"encoding/json"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGrokOAuthQuotaActionProjectsActualBilling(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/billing" {
			t.Error("quota refresh performed a model call")
		}
		w.Write([]byte(`{"config":{"monthlyLimit":{"val":0},"used":{"val":0},"onDemandCap":{"val":0},"billingPeriodEnd":"2026-12-01T00:00:00Z"}}`))
	}))
	defer upstream.Close()
	s := New(adminTestConfig(t.TempDir()))
	s.xaiProbe = provider.NewXAIProbe(upstream.URL, upstream.URL+"/token", upstream.Client())
	item, _ := s.oauthStore.Import(oauth.Account{Email: "one@example.test", AccessToken: "ACCESS_SECRET", RefreshToken: "REFRESH_SECRET", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	response := adminRequest(s.Handler(), "POST", "/api/grok/oauth/accounts/"+item.ID+"/quota", strings.NewReader(`{}`))
	if response.Code != 200 {
		t.Fatalf("refresh failed: %s", response.Body.String())
	}
	list := adminRequest(s.Handler(), "GET", "/api/grok/oauth/accounts", nil)
	var body struct {
		Items []map[string]any `json:"items"`
	}
	json.Unmarshal(list.Body.Bytes(), &body)
	quota := body.Items[0]["quota"].(map[string]any)
	if quota["remaining_percent"] != nil || quota["monthly_zero"] != true || quota["no_quota"] == true || quota["period"].(map[string]any)["end"] != "2026-12-01T00:00:00Z" || calls != 1 {
		t.Fatalf("quota lost: %s", list.Body.String())
	}
	for _, secret := range []string{"ACCESS_SECRET", "REFRESH_SECRET"} {
		if strings.Contains(list.Body.String()+response.Body.String(), secret) {
			t.Fatal("secret exposed")
		}
	}
}

func TestGrokOAuthCPASyncIdentityPayloadAndFailure(t *testing.T) {
	for _, mode := range []string{"success", "upload_failed", "conflicting_identity", "redirect", "missing_identity", "unrecognized", "invalid_list"} {
		t.Run(mode, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			expiry := time.Now().Add(time.Hour).Unix()
			account, _ := s.oauthStore.Import(oauth.Account{Email: "person@example.test", Subject: "subject-1", AccessToken: "ACCESS_PRIVATE", RefreshToken: "REFRESH_PRIVATE", IDToken: "ID_PRIVATE", TokenType: "Bearer", ExpiresAt: expiry})
			if mode == "missing_identity" {
				account.Email = ""
				account, _ = s.oauthStore.Import(account)
			}
			uploads, lists := 0, 0
			exists := mode == "conflicting_identity"
			var remotePayload map[string]any
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v0/management/auth-files/download" {
					json.NewEncoder(w).Encode(remotePayload)
					return
				}
				if r.URL.Path != "/v0/management/auth-files" {
					t.Error("followed redirect")
					w.WriteHeader(500)
					return
				}
				if r.Header.Get("Authorization") != "Bearer management-secret" {
					t.Error("wrong CPA credential")
				}
				if r.Method == "GET" {
					lists++
					if mode == "invalid_list" {
						w.Write([]byte(`{}`))
						return
					}
					files := []map[string]string{}
					if exists {
						email := "person@example.test"
						if mode == "conflicting_identity" {
							email = "other@example.test"
						}
						files = append(files, map[string]string{"name": "xai-person@example.test.json", "provider": "xai", "email": email})
					}
					json.NewEncoder(w).Encode(map[string]any{"files": files})
					return
				}
				uploads++
				if mode == "redirect" {
					w.Header().Set("Location", "/leak")
					w.WriteHeader(307)
					return
				}
				if mode == "upload_failed" {
					http.Error(w, "ACCESS_PRIVATE REFRESH_PRIVATE management-secret", 500)
					return
				}
				file, header, err := r.FormFile("file")
				if err != nil {
					t.Error(err)
					return
				}
				defer file.Close()
				var payload map[string]any
				json.NewDecoder(file).Decode(&payload)
				if header.Filename != "xai-person@example.test.json" || payload["type"] != "xai" || payload["auth_kind"] != "oauth" || payload["email"] != account.Email || payload["sub"] != "subject-1" || payload["access_token"] != account.AccessToken || payload["refresh_token"] != account.RefreshToken || payload["expired"] != time.Unix(expiry, 0).UTC().Format(time.RFC3339) {
					t.Error("CPA xAI payload mismatch")
				}
				if _, ok := payload["sso"]; ok {
					t.Error("SSO incorrectly uploaded")
				}
				remotePayload = payload
				exists = mode != "unrecognized"
				w.Write([]byte(`{"status":"ok"}`))
			}))
			defer upstream.Close()
			if err := s.external.saveCPAList([]externalCPAPool{{ID: "selected-pool", Name: "CPA Test", BaseURL: upstream.URL, SecretKey: "management-secret"}}); err != nil {
				t.Fatal(err)
			}
			unauthorized := httptest.NewRecorder()
			s.Handler().ServeHTTP(unauthorized, httptest.NewRequest("POST", "/api/grok/oauth/accounts/"+account.ID+"/cpa/sync", strings.NewReader(`{"cpa_pool_id":"selected-pool"}`)))
			if unauthorized.Code != 401 {
				t.Fatal("sync lacks admin authentication")
			}
			response := adminRequest(s.Handler(), "POST", "/api/grok/oauth/accounts/"+account.ID+"/cpa/sync", strings.NewReader(`{"cpa_pool_id":"selected-pool"}`))
			saved, _, _ := s.oauthStore.Get(account.ID)
			switch mode {
			case "success":
				if response.Code != 200 || uploads != 1 || lists != 2 || saved.CPASync == nil || saved.CPASync.Status != "synced" {
					t.Fatalf("sync failed: %s uploads=%d lists=%d", response.Body.String(), uploads, lists)
				}
			case "missing_identity":
				if response.Code != 502 || uploads != 0 || lists != 0 {
					t.Fatal("missing identity reached CPA")
				}
			default:
				if response.Code != 502 || saved.CPASync == nil || (saved.CPASync.Status != "failed" && saved.CPASync.Status != "unknown") {
					t.Fatalf("failure not persisted: %s", response.Body.String())
				}
				if (mode == "conflicting_identity" || mode == "invalid_list") && uploads != 0 {
					t.Error("conflicting remote overwritten")
				}
				if uploads > 1 {
					t.Error("upload automatically retried")
				}
			}
			for _, secret := range []string{"ACCESS_PRIVATE", "REFRESH_PRIVATE", "ID_PRIVATE", "management-secret"} {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatal("sync response exposed credentials")
				}
			}
		})
	}
}

func TestGrokOAuthRefreshDoesNotRotateCPAManagedCopy(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Write([]byte(`{"access_token":"NEW","refresh_token":"ROTATED","expires_in":3600}`))
			return
		}
		if r.URL.Path != "/billing" {
			t.Error("unexpected model call")
		}
		w.Write([]byte(`{"creditUsagePercent":25}`))
	}))
	defer upstream.Close()
	s.xaiProbe = provider.NewXAIProbe(upstream.URL, upstream.URL+"/token", upstream.Client())
	item, _ := s.oauthStore.Import(oauth.Account{Email: "rotate@example.test", AccessToken: "OLD", RefreshToken: "REFRESH", ExpiresAt: time.Now().Add(-time.Minute).Unix(), CPASync: &oauth.CPASync{Status: "synced", PoolID: "pool"}})
	response := adminRequest(s.Handler(), "POST", "/api/grok/oauth/accounts/"+item.ID+"/quota", strings.NewReader(`{}`))
	saved, _, _ := s.oauthStore.Get(item.ID)
	if response.Code != 409 || saved.AccessToken != "OLD" || saved.CPASync.Status != "synced" {
		t.Fatal("CFM rotated a CPA-managed credential")
	}
}

func TestGrokOAuthQuotaFailurePreservesAuthorizationAndConcurrentActions(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "SENSITIVE", 403) }))
	defer upstream.Close()
	s.xaiProbe = provider.NewXAIProbe(upstream.URL, "", upstream.Client())
	item, _ := s.oauthStore.Import(oauth.Account{Email: "one@example.test", AccessToken: "access", RefreshToken: "refresh"})
	response := adminRequest(s.Handler(), "POST", "/api/grok/oauth/accounts/"+item.ID+"/quota", strings.NewReader(`{}`))
	saved, _, _ := s.oauthStore.Get(item.ID)
	if response.Code != 200 || saved.Status != "active" || saved.Quota["status"] != "error" || saved.Quota["remaining_percent"] != nil {
		t.Fatal("billing failure corrupted authorization or invented zero quota")
	}
	if strings.Contains(response.Body.String(), "SENSITIVE") {
		t.Fatal("upstream body leaked")
	}
	s.grokOAuthActions.Store(item.ID, true)
	defer s.grokOAuthActions.Delete(item.ID)
	for _, action := range []string{"quota", "cpa/sync", "test"} {
		response = adminRequest(s.Handler(), "POST", "/api/grok/oauth/accounts/"+item.ID+"/"+action, strings.NewReader(`{}`))
		if response.Code != 409 {
			t.Fatalf("concurrent %s not rejected: %d", action, response.Code)
		}
	}
}
