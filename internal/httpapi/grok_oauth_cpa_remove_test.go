package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/xaiquality"
)

func TestManualCPARemovalPreservesAuthorizationAndQuality(t *testing.T) {
	for _, mode := range []string{"removed", "absent", "lost_response", "unsupported", "denied", "retained", "identity_changed", "wrong_email", "wrong_provider", "missing_index", "list_failure"} {
		for _, qualityStatus := range []string{"normal", "error", "high_risk"} {
			t.Run(mode+"/"+qualityStatus, func(t *testing.T) {
				s := New(adminTestConfig(t.TempDir()))
				name := "xai-test@example.test.json"
				q := &xaiquality.Report{Version: xaiquality.Version, Model: xaiquality.Model, Status: qualityStatus, Source: "cpa", At: 100, Summary: "saved evidence", Samples: []xaiquality.Sample{{Status: "error", Reason: "HTTP 402"}}}
				if qualityStatus == "high_risk" {
					q.Blocked, q.ConfirmedAt = true, 99
					q.Removal = &xaiquality.Removal{Status: "manual_required", FileName: name, Error: "previous attempt"}
				}
				item, err := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "LOCAL-OLD", RefreshToken: "LOCAL-REFRESH", ExpiresAt: 1, Quality: q, CPASync: &oauth.CPASync{Status: "synced", PoolID: "pool", PoolName: "Saved CPA", FileName: name, Managed: true, At: 10, VerifiedAt: 10}})
				if err != nil {
					t.Fatal(err)
				}
				s.registerStore.AddAccount(map[string]any{"id": "grok-source", "email": item.Email, "sso": "retained-sso"})
				exists, lists, deletes := mode != "absent", 0, 0
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer MANAGEMENT-PRIVATE" || r.URL.Path != "/v0/management/auth-files" || r.URL.Query().Get("name") != name || r.URL.Query().Get("all") != "" {
						t.Error("removal must target only the recorded file")
						w.WriteHeader(400)
						return
					}
					switch r.Method {
					case "GET":
						lists++
						if mode == "list_failure" {
							w.WriteHeader(503)
							return
						}
						rows := []map[string]any{}
						if exists {
							email, provider, index := item.Email, "xai", "original-index"
							if mode == "wrong_email" {
								email = "other@example.test"
							}
							if mode == "wrong_provider" {
								provider = "codex"
							}
							if mode == "missing_index" {
								index = ""
							}
							if mode == "identity_changed" && lists > 1 {
								index = "new-index"
							}
							rows = append(rows, map[string]any{"name": name, "email": email, "provider": provider, "auth_index": index})
						}
						json.NewEncoder(w).Encode(map[string]any{"files": rows})
					case "DELETE":
						deletes++
						stored, _, _ := s.oauthStore.Get(item.ID)
						if !oauth.CPARemovalRequested(stored) || !reflect.DeepEqual(stored.Quality, q) {
							t.Error("removal was not fenced before deleting or changed quality")
						}
						if mode == "unsupported" || mode == "denied" {
							status := 405
							if mode == "denied" {
								status = 403
							}
							http.Error(w, "MANAGEMENT-PRIVATE LOCAL-OLD", status)
							return
						}
						if mode != "retained" {
							exists = false
						}
						if mode == "lost_response" {
							w.WriteHeader(500)
							return
						}
						w.Write([]byte(`{"status":"ok"}`))
					default:
						t.Error("removal must not upload, download tokens or run probes")
						w.WriteHeader(500)
					}
				}))
				defer remote.Close()
				s.external.saveCPAList([]externalCPAPool{{ID: "pool", Name: "Saved CPA", BaseURL: remote.URL, SecretKey: "MANAGEMENT-PRIVATE"}})
				payload := `{"cpa_pool_id":"pool","file_name":"` + name + `"}`
				path := "/api/grok/oauth/accounts/" + item.ID + "/cpa/remove"
				unauthorized := httptest.NewRecorder()
				s.Handler().ServeHTTP(unauthorized, httptest.NewRequest("POST", path, strings.NewReader(payload)))
				if unauthorized.Code != 401 || lists != 0 || deletes != 0 {
					t.Fatal("unauthenticated removal contacted CPA")
				}
				res := adminRequest(s.Handler(), "POST", path, strings.NewReader(payload))
				stored, _, _ := s.oauthStore.Get(item.ID)
				if !reflect.DeepEqual(stored.Quality, q) || stored.Status != item.Status || !oauth.SameCredential(stored, item) || stored.ExpiresAt != item.ExpiresAt || stored.CPASync.At != 10 || stored.CPASync.VerifiedAt != 10 {
					t.Fatal("remote removal changed authorization, quality evidence or historical delivery")
				}
				success := mode == "removed" || mode == "absent" || mode == "lost_response"
				if success {
					if res.Code != 200 || stored.CPASync.Status != "removed" || stored.CPASync.ManualRemoval.Status != "removed" || exists {
						t.Fatalf("removal not confirmed: %s", res.Body.String())
					}
					before := deletes
					if _, err := s.removeGrokOAuthCPA(context.Background(), item.ID, "pool", name); err != nil || deletes != before {
						t.Fatal("confirmed removal repeated")
					}
				} else if res.Code == 200 {
					t.Fatal("failed removal reported success")
				}
				if mode == "identity_changed" || mode == "wrong_email" || mode == "wrong_provider" || mode == "missing_index" || mode == "list_failure" || mode == "absent" {
					if deletes != 0 {
						t.Fatal("unsafe remote identity was deleted")
					}
				} else if deletes != 1 {
					t.Fatal("expected one targeted deletion attempt")
				}
				if oauth.CPARemovalRequested(stored) {
					before := lists
					for _, check := range []bool{false, true} {
						if _, err := s.syncGrokOAuthCPA(context.Background(), item.ID, "pool", check); err == nil {
							t.Fatal("removed credential resumed delivery")
						}
					}
					if _, err := s.checkGrokQuality(context.Background(), item.ID, false); err == nil {
						t.Fatal("removed credential overwritten by a new probe")
					}
					if ids, err := s.enqueueGrokQuality([]string{item.ID}, false); err != nil || len(ids) != 0 || qualityNextDue(stored, time.Hour, time.Now()) != 0 {
						t.Fatal("removed credential scheduled for probing")
					}
					if lists != before {
						t.Fatal("blocked operation contacted CPA")
					}
					restarted := New(s.cfg)
					if _, err := restarted.syncGrokOAuthCPA(context.Background(), item.ID, "pool", false); err == nil {
						t.Fatal("removal fence lost after restart")
					}
				}
				candidates := adminRequest(s.Handler(), "GET", "/api/grok/oauth/candidates", nil)
				if !strings.Contains(candidates.Body.String(), `"oauth_status":"authorized"`) {
					t.Fatal("remote removal cleared the authorized status")
				}
				for _, secret := range []string{"MANAGEMENT-PRIVATE", "LOCAL-OLD", "LOCAL-REFRESH", "retained-sso"} {
					if strings.Contains(res.Body.String()+candidates.Body.String(), secret) {
						t.Fatal("removal leaked credentials")
					}
				}
			})
		}
	}
}

func TestManualCPARemovalRejectsUnboundOrBusyAccount(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	remote := installXAITestCPA(t, s)
	item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "access", RefreshToken: "refresh", CPASync: &oauth.CPASync{Status: "synced", PoolID: "test-cpa", FileName: "xai-test@example.test.json", Managed: true}})
	if _, err := s.removeGrokOAuthCPA(context.Background(), item.ID, "different-pool", item.CPASync.FileName); err == nil {
		t.Fatal("changed target accepted")
	}
	s.grokOAuthActions.Store(item.ID, true)
	if _, err := s.removeGrokOAuthCPA(context.Background(), item.ID, "test-cpa", item.CPASync.FileName); err == nil {
		t.Fatal("concurrent action accepted")
	}
	s.grokOAuthActions.Delete(item.ID)
	local, _ := s.oauthStore.Import(oauth.Account{Email: "local@example.test", AccessToken: "access", CPASync: &oauth.CPASync{Status: "quality_blocked", PoolID: "test-cpa", FileName: "xai-local@example.test.json"}})
	if _, err := s.removeGrokOAuthCPA(context.Background(), local.ID, "test-cpa", local.CPASync.FileName); err == nil {
		t.Fatal("never-delivered local account accepted for remote deletion")
	}
	if remote.deletes != 0 {
		t.Fatal("invalid request deleted a file")
	}
}

func TestManualCPARemovalRetryKeepsOriginalTarget(t *testing.T) {
	for _, retryMode := range []string{"same_file", "new_index", "new_connection"} {
		t.Run(retryMode, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			name, index, exists, deny, deletes := "xai-test@example.test.json", "original-index", true, true, 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "GET":
					files := []map[string]any{}
					if exists {
						files = append(files, map[string]any{"name": name, "provider": "xai", "email": "test@example.test", "auth_index": index})
					}
					json.NewEncoder(w).Encode(map[string]any{"files": files})
				case "DELETE":
					deletes++
					if r.URL.Query().Get("name") != name {
						t.Error("wrong file")
					}
					if deny {
						w.WriteHeader(405)
						return
					}
					exists = false
					w.WriteHeader(200)
				default:
					t.Error("unexpected remote mutation")
					w.WriteHeader(500)
				}
			}))
			defer remote.Close()
			pool := externalCPAPool{ID: "pool", Name: "CPA", BaseURL: remote.URL, SecretKey: "fixture"}
			s.external.saveCPAList([]externalCPAPool{pool})
			item, _ := s.oauthStore.Import(oauth.Account{Email: "test@example.test", AccessToken: "old", CPASync: &oauth.CPASync{Status: "synced", PoolID: "pool", FileName: name, Managed: true}})
			first, err := s.removeGrokOAuthCPA(context.Background(), item.ID, "pool", name)
			if err == nil || first.CPASync.ManualRemoval == nil || first.CPASync.ManualRemoval.AuthIndex != "original-index" || deletes != 1 {
				t.Fatal("failed deletion did not preserve its original target")
			}
			deny = false
			if retryMode == "new_index" {
				index = "replacement-index"
			}
			if retryMode == "new_connection" {
				pool.BaseURL = "http://replacement.invalid"
				s.external.saveCPAList([]externalCPAPool{pool})
			}
			last, err := s.removeGrokOAuthCPA(context.Background(), item.ID, "pool", name)
			if retryMode == "same_file" {
				if err != nil || deletes != 2 || exists || last.CPASync.ManualRemoval.Status != "removed" {
					t.Fatal("retry did not remove the original file")
				}
			} else if err == nil || deletes != 1 || !exists || last.CPASync.ManualRemoval.AuthIndex != "original-index" {
				t.Fatal("retry adopted a replacement CPA identity")
			}
		})
	}
}
