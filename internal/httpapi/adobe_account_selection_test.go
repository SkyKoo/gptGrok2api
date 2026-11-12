package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func TestAdobeMediaRoutesSkipUnavailableModelAccountBeforeBinding(t *testing.T) {
	for _, video := range []bool{false, true} {
		t.Run(fmt.Sprint(video), func(t *testing.T) {
			s := imageTaskTestServer(t)
			first := adobeFixtureAccount(t, s)
			second := first
			second.AccountID = "available-adobe"
			claims, _ := json.Marshal(map[string]any{"user_id": second.AccountID, "exp": time.Now().Add(time.Hour).Unix()})
			second.Token = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
			rows, err := s.adobeStore.Import([]adobe.Account{second})
			if err != nil {
				t.Fatal(err)
			}
			second = adobeFixtureModelPermissions(t, s, rows[0])
			target, other := "gemini-3.1-flash-image", "veo-3.1-lite"
			if video {
				target, other = other, target
			}
			zero, one := 0.0, 1.0
			observation := adobe.GenerationEntitlements{Status: "known", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
				Models: []adobe.ModelEntitlement{{Model: target, Authorized: true, ResourceType: "firefly_complimentary_generations", PoolID: "empty", Cost: &one}, {Model: other, Authorized: true, ResourceType: "firefly_complimentary_generations", PoolID: "available", Cost: &one}},
				Pools:  []adobe.GenerationPool{{ID: "empty", ResourceType: "firefly_complimentary_generations", Available: &zero}, {ID: "available", ResourceType: "firefly_complimentary_generations", Available: &one}},
			}
			if err = s.adobeStore.UpdateGenerationPermissions(first.ID, first.Revision, observation); err != nil {
				t.Fatal(err)
			}
			fixture := &adobeMediaCapture{video: video}
			if !video {
				fixture.output = adobeMediaPNG(t, 1024, 1024)
			}
			s.adobeClient = fixture.client(t)
			var route *mediaRoute
			if video {
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, adobeVideoMediaRequest(t, true, nil))
				if w.Code != 200 {
					t.Fatalf("accept: %d %s", w.Code, w.Body.String())
				}
				var created videoJob
				if err = json.Unmarshal(w.Body.Bytes(), &created); err != nil {
					t.Fatal(err)
				}
				route = waitAdobeVideoIdle(t, s, created.ID).Route
			} else {
				w := imageTaskCall(s, "POST", "/api/image-tasks/generations", "api-secret", `{"model":"gemini-3.1-flash-image","prompt":"a blue circle","size":"1024x1024","client_task_id":"selection"}`)
				if w.Code != 202 {
					t.Fatalf("accept: %d %s", w.Code, w.Body.String())
				}
				route = waitAdobeImage(t, s, "selection").Route
			}
			if route == nil || route.AccountID != second.ID {
				t.Fatalf("wrong persisted account route: %+v", route)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.preflights != 1 || fixture.submits != 1 {
				t.Fatalf("preflights=%d submits=%d", fixture.preflights, fixture.submits)
			}
		})
	}
}

func adobeSelectionPermissionsBody(target string, available float64) map[string]any {
	profiles := []any{}
	for _, m := range adobe.FreeModels() {
		pool := "other-pool"
		if m.ID == target {
			pool = "target-pool"
		}
		profiles = append(profiles, map[string]any{"featureId": m.FeatureID, "authorizationResult": map[string]any{"result": "authorized", "reasonCode": "quota_available"}, "selectedResource": map[string]any{"resourceType": "firefly_complimentary_generations", "effectiveQuotaReferenceId": pool, "computedCost": 1}})
	}
	return map[string]any{"featuresAuthorization": profiles, "effectiveQuotas": []any{map[string]any{"effectiveQuotaReferenceId": "target-pool", "resourceType": "firefly_complimentary_generations", "provisionedQuota": 1, "consumedQuota": 1 - available}, map[string]any{"effectiveQuotaReferenceId": "other-pool", "resourceType": "firefly_complimentary_generations", "provisionedQuota": 10, "consumedQuota": 0}}}
}

func markAdobeFixturePermissionsStale(t *testing.T, s *Server, a adobe.Account, oldAvailable float64) {
	t.Helper()
	fresh, err := s.adobeStore.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := *fresh.Quota.Generation
	e.Status = "stale"
	e.CheckedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	e.Pools = append([]adobe.GenerationPool(nil), e.Pools...)
	for i := range e.Pools {
		value := oldAvailable
		e.Pools[i].Available = &value
	}
	if err = s.adobeStore.UpdateGenerationPermissions(a.ID, a.Revision, e); err != nil {
		t.Fatal(err)
	}
}

func TestAdobeForegroundSelectionRefreshesStaleModelQuotaWithMaintenanceDisabled(t *testing.T) {
	for _, target := range []string{"gemini-3.1-flash-image", "veo-3.1-lite"} {
		for _, failFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failed-first=%v", target, failFirst), func(t *testing.T) {
				s := imageTaskTestServer(t)
				first := adobeSchedulingAccount(t, s, "first-stale", time.Now().Add(-2*time.Hour))
				second := adobeSchedulingAccount(t, s, "second-stale", time.Now().Add(-time.Hour))
				if _, err := s.store.UpdateConfig("adobe_maintenance", map[string]any{"enabled": false}); err != nil {
					t.Fatal(err)
				}
				markAdobeFixturePermissionsStale(t, s, first, 0)
				markAdobeFixturePermissionsStale(t, s, second, 1)
				browserCalls, balances, permissions := 0, 0, 0
				s.adobeStore.SetBrowserRefresher(func(context.Context, adobe.Account) (adobe.Account, error) {
					browserCalls++
					t.Error("quota check opened browser for unexpired credentials")
					return adobe.Account{}, errors.New("unexpected browser")
				})
				s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					status := 200
					var body any
					switch r.URL.Path {
					case "/v1/credits/balance":
						balances++
						body = map[string]any{"total": map[string]any{"quota": map[string]any{"available": 0}}}
					case "/v1/feature-auth-profile":
						permissions++
						isFirst := r.Header.Get("Authorization") == "Bearer "+first.Token
						if isFirst && failFirst {
							status = 503
							body = map[string]any{"error_code": "upstream_error"}
						} else {
							available := 1.0
							if isFirst {
								available = 0
							}
							body = adobeSelectionPermissionsBody(target, available)
						}
					default:
						t.Errorf("selection performed generation/preflight: %s", r.URL.Path)
						status = 500
						body = map[string]any{}
					}
					raw, _ := json.Marshal(body)
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
				})})
				if err := s.ensureAdobeAccounts(context.Background(), target); err != nil {
					t.Fatal(err)
				}
				selected, _, err := s.adobeStore.ReserveMediaForModel(context.Background(), "fresh-selection", target, s.adobeAccountContext)
				if err != nil || selected.ID != second.ID {
					t.Fatalf("selected %s expected %s: %v", selected.ID, second.ID, err)
				}
				if browserCalls != 0 || balances != 2 || permissions != 2 {
					t.Fatalf("browser=%d balances=%d permissions=%d", browserCalls, balances, permissions)
				}
				firstNow, _ := s.adobeStore.Get(first.ID)
				if firstNow.InFlight != "" {
					t.Fatal("unavailable first account was bound")
				}
				if failFirst && firstNow.Quota.Generation.Status != "error" {
					t.Fatal("failed read reused historical positive/zero observation")
				}
			})
		}
	}
}

func TestAdobeMediaEntrypointsStopWhenEntitlementsCannotBeRefreshed(t *testing.T) {
	for _, kind := range []string{"sync_image", "async_image", "video"} {
		t.Run(kind, func(t *testing.T) {
			s := imageTaskTestServer(t)
			a := adobeFixtureAccount(t, s)
			markAdobeFixturePermissionsStale(t, s, a, 10)
			browserCalls, submits := 0, 0
			s.adobeStore.SetBrowserRefresher(func(context.Context, adobe.Account) (adobe.Account, error) {
				browserCalls++
				return adobe.Account{}, errors.New("unexpected browser")
			})
			s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, body := 200, `{"total":{"quota":{"available":0}}}`
				if r.URL.Path == "/v1/feature-auth-profile" {
					status, body = 503, `{"error_code":"upstream_error"}`
				} else if r.URL.Path != "/v1/credits/balance" {
					submits++
					t.Errorf("failed entitlement read advanced to %s", r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})})
			var status string
			var route *mediaRoute
			if kind == "sync_image" {
				response := imageTaskCall(s, "POST", "/v1/images/edits", "api-secret", adobePreparationImageBody(t))
				if response.Code < 400 {
					t.Fatalf("failed entitlement read accepted sync request: %d", response.Code)
				}
				status = "error"
				s.imageTaskMu.RLock()
				for _, task := range s.imageTasks {
					if task.Route != nil {
						route = task.Route
					}
				}
				s.imageTaskMu.RUnlock()
			} else {
				status, route, _ = adobePreparationRequest(t, s, kind)
			}
			if (status != "error" && status != "failed") || route != nil || browserCalls != 0 || submits != 0 {
				t.Fatalf("failed observation bound/generated: status=%s route=%+v browsers=%d submits=%d", status, route, browserCalls, submits)
			}
			current, _ := s.adobeStore.Get(a.ID)
			if current.InFlight != "" || current.Quota.Generation.Status != "error" {
				t.Fatal("failed observation retained usable historical quota")
			}
		})
	}
}

func TestAdobeForegroundEntitlementRefreshHasFourAccountBudget(t *testing.T) {
	s := imageTaskTestServer(t)
	for i := 0; i < 5; i++ {
		a := adobeSchedulingAccount(t, s, fmt.Sprintf("budget-%d", i), time.Now().Add(-time.Hour))
		markAdobeFixturePermissionsStale(t, s, a, 1)
	}
	balances, permissions := 0, 0
	s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body any
		switch r.URL.Path {
		case "/v1/credits/balance":
			balances++
			body = map[string]any{"total": map[string]any{"quota": map[string]any{"available": 0}}}
		case "/v1/feature-auth-profile":
			permissions++
			body = adobeSelectionPermissionsBody("veo-3.1-lite", 0)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		raw, _ := json.Marshal(body)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
	})})
	if err := s.ensureAdobeAccounts(context.Background(), "veo-3.1-lite"); !errors.Is(err, adobe.ErrNoAccount) {
		t.Fatal(err)
	}
	if balances != 4 || permissions != 4 {
		t.Fatalf("budget exceeded: balances=%d permissions=%d", balances, permissions)
	}
	all, _ := s.adobeStore.List()
	for _, a := range all {
		if a.InFlight != "" {
			t.Fatal("quota-only budget checks bound task")
		}
	}
}

func TestAdobeFreshButUnselectableAccountDoesNotBlockStaleCandidateRefresh(t *testing.T) {
	for _, blocker := range []string{"media_cooldown", "unavailable_egress"} {
		t.Run(blocker, func(t *testing.T) {
			s := imageTaskTestServer(t)
			first := adobeSchedulingAccount(t, s, "fresh-unselectable", time.Now().Add(-2*time.Hour))
			second := adobeSchedulingAccount(t, s, "stale-recoverable", time.Now().Add(-time.Hour))
			markAdobeFixturePermissionsStale(t, s, second, 1)
			if blocker == "media_cooldown" {
				s.adobeStore.SetBrowserRefresher(func(context.Context, adobe.Account) (adobe.Account, error) {
					return adobe.Account{}, &adobe.Error{Stage: "relogin", Code: "browser_failed"}
				})
				fixed, err := s.adobeAccountContext(context.Background(), first)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.adobeStore.WarmMediaSession(fixed, first.ID, first.Revision); err == nil {
					t.Fatal("fixture did not enter media cooldown")
				}
			} else if _, err := s.adobeStore.BindProxy(first.ID, proxyruntime.Binding{Reference: "node:missing/node", Platform: "adobe", Purpose: "account"}, "fixture unavailable route"); err != nil {
				t.Fatal(err)
			}
			s.adobeStore.SetBrowserRefresher(func(context.Context, adobe.Account) (adobe.Account, error) {
				t.Error("entitlement selection unexpectedly opened browser")
				return adobe.Account{}, errors.New("unexpected browser")
			})
			reads := 0
			s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				reads++
				var body any
				switch r.URL.Path {
				case "/v1/credits/balance":
					if r.Header.Get("x-account-id") != second.AccountID {
						t.Error("queried unselectable first account")
					}
					body = map[string]any{"total": map[string]any{"quota": map[string]any{"available": 0}}}
				case "/v1/feature-auth-profile":
					if r.Header.Get("Authorization") != "Bearer "+second.Token {
						t.Error("queried unselectable first account")
					}
					body = adobeSelectionPermissionsBody("veo-3.1-lite", 1)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				raw, _ := json.Marshal(body)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
			})})
			if err := s.ensureAdobeAccounts(context.Background(), "veo-3.1-lite"); err != nil {
				t.Fatal(err)
			}
			chosen, _, err := s.adobeStore.ReserveMediaForModel(context.Background(), "select-recoverable", "veo-3.1-lite", s.adobeAccountContext)
			if err != nil || chosen.ID != second.ID || reads != 2 {
				t.Fatalf("selection=%s reads=%d err=%v", chosen.ID, reads, err)
			}
		})
	}
}

func TestEnsureAdobeAccountsRefreshesOtherModelCandidatesBeforeBinding(t *testing.T) {
	for _, target := range []string{"gemini-3.1-flash-image", "veo-3.1-lite"} {
		for _, denyFirstRefresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/first-refresh-denied=%v", target, denyFirstRefresh), func(t *testing.T) {
				s := imageTaskTestServer(t)
				first := adobeFixtureAccount(t, s)
				zero, one := 0.0, 1.0
				knownEmpty := adobe.GenerationEntitlements{Status: "known", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
					Models: []adobe.ModelEntitlement{{Model: target, Authorized: true, ResourceType: "firefly_complimentary_generations", PoolID: "empty", Cost: &one}},
					Pools:  []adobe.GenerationPool{{ID: "empty", ResourceType: "firefly_complimentary_generations", Available: &zero}},
				}
				if err := s.adobeStore.UpdateGenerationPermissions(first.ID, first.Revision, knownEmpty); err != nil {
					t.Fatal(err)
				}
				freshTokens := map[string]string{}
				candidateCount := 1
				if denyFirstRefresh {
					candidateCount = 2
				}
				var expected adobe.Account
				for i := 1; i <= candidateCount; i++ {
					a := first
					a.AccountID = fmt.Sprintf("refresh-candidate-%d", i)
					a.RefreshCookie = "imsSession=" + a.AccountID
					claims, _ := json.Marshal(map[string]any{"user_id": a.AccountID, "exp": time.Now().Add(30 * time.Second).Unix()})
					a.Token = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
					rows, err := s.adobeStore.Import([]adobe.Account{a})
					if err != nil {
						t.Fatal(err)
					}
					expected = rows[0]
					claims, _ = json.Marshal(map[string]any{"user_id": a.AccountID, "exp": time.Now().Add(time.Hour).Unix()})
					freshTokens[a.AccountID] = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
				}
				refreshes, balances, entitlements := 0, 0, 0
				s.adobeClient = adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					var body any
					switch r.URL.Path {
					case "/ims/check/v6/token":
						refreshes++
						id := strings.TrimPrefix(r.Header.Get("Cookie"), "imsSession=")
						token, ok := freshTokens[id]
						if !ok {
							t.Fatalf("refreshed unexpected account: %s", id)
						}
						body = map[string]any{"access_token": token}
					case "/v1/credits/balance":
						balances++
						if _, ok := freshTokens[r.Header.Get("x-account-id")]; !ok {
							t.Fatal("rechecked known unavailable account")
						}
						body = map[string]any{"total": map[string]any{"quota": map[string]any{"available": 0}}}
					case "/v1/feature-auth-profile":
						entitlements++
						profiles := []any{}
						for _, m := range adobe.FreeModels() {
							result, reason := "authorized", "quota_available"
							if denyFirstRefresh && r.Header.Get("Authorization") == "Bearer "+freshTokens["refresh-candidate-1"] && m.ID == target {
								result, reason = "unauthorized_not_entitled", "user_not_entitled"
							}
							profiles = append(profiles, map[string]any{"featureId": m.FeatureID, "authorizationResult": map[string]any{"result": result, "reasonCode": reason}, "selectedResource": map[string]any{"resourceType": "firefly_complimentary_generations", "effectiveQuotaReferenceId": "free-pool", "computedCost": 1}})
						}
						body = map[string]any{"featuresAuthorization": profiles, "effectiveQuotas": []any{map[string]any{"effectiveQuotaReferenceId": "free-pool", "resourceType": "firefly_complimentary_generations", "provisionedQuota": 3, "consumedQuota": 0}}}
					default:
						t.Fatalf("unexpected upstream generation/preflight during maintenance: %s", r.URL.Path)
					}
					raw, _ := json.Marshal(body)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
				})})
				if err := s.ensureAdobeAccounts(context.Background(), target); err != nil {
					t.Fatal(err)
				}
				if refreshes != candidateCount || balances != candidateCount || entitlements != candidateCount {
					t.Fatalf("refresh=%d balance=%d entitlements=%d expected=%d", refreshes, balances, entitlements, candidateCount)
				}
				all, err := s.adobeStore.List()
				if err != nil {
					t.Fatal(err)
				}
				for _, a := range all {
					if a.InFlight != "" {
						t.Fatal("read-only maintenance bound a generation")
					}
				}
				got, err := s.adobeStore.ReserveForModel("task", target)
				if err != nil || got.ID != expected.ID {
					t.Fatalf("selected=%s want=%s err=%v", got.ID, expected.ID, err)
				}
			})
		}
	}
}
