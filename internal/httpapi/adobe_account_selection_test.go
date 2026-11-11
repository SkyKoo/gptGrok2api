package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
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
			second = rows[0]
			target, other := "gemini-3.1-flash-image", "veo-3.1-lite"
			if video {
				target, other = other, target
			}
			zero, one := 0.0, 1.0
			observation := adobe.GenerationEntitlements{Status: "known", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
				Models: []adobe.ModelEntitlement{{Model: target, Authorized: true, PoolID: "empty", Cost: &one}, {Model: other, Authorized: true, PoolID: "available", Cost: &one}},
				Pools:  []adobe.GenerationPool{{ID: "empty", Available: &zero}, {ID: "available", Available: &one}},
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

func TestEnsureAdobeAccountsRefreshesOtherModelCandidatesBeforeBinding(t *testing.T) {
	for _, target := range []string{"gemini-3.1-flash-image", "veo-3.1-lite"} {
		for _, denyFirstRefresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/first-refresh-denied=%v", target, denyFirstRefresh), func(t *testing.T) {
				s := imageTaskTestServer(t)
				first := adobeFixtureAccount(t, s)
				zero, one := 0.0, 1.0
				knownEmpty := adobe.GenerationEntitlements{Status: "known", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
					Models: []adobe.ModelEntitlement{{Model: target, Authorized: true, PoolID: "empty", Cost: &one}},
					Pools:  []adobe.GenerationPool{{ID: "empty", Available: &zero}},
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
