package adobe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func entitlementFixture() map[string]any {
	profiles := []any{}
	for _, m := range FreeModels() {
		ref := "private-image-pool"
		resource := "firefly_complimentary_generations"
		if m.Kind == "video" {
			ref = "private-video-pool"
		}
		if m.ID == "gpt-image-2" {
			ref = "private-taste-pool"
			resource = "firefly_limited_taste"
		}
		profiles = append(profiles, map[string]any{"featureId": m.FeatureID, "authorizationResult": map[string]any{"result": "authorized", "reasonCode": "quota_available"}, "selectedResource": map[string]any{"resourceType": resource, "effectiveQuotaReferenceId": ref, "computedCost": 1}})
	}
	return map[string]any{"featuresAuthorization": profiles, "effectiveQuotas": []any{
		map[string]any{"effectiveQuotaReferenceId": "private-image-pool", "resourceType": "firefly_complimentary_generations", "provisionedQuota": 3, "consumedQuota": 1, "refreshPeriod": "P1D"},
		map[string]any{"effectiveQuotaReferenceId": "private-video-pool", "resourceType": "firefly_complimentary_generations", "provisionedQuota": 1, "consumedQuota": 0, "refreshPeriod": "P1D"},
		map[string]any{"effectiveQuotaReferenceId": "private-taste-pool", "resourceType": "firefly_limited_taste", "provisionedQuota": 3},
	}}
}
func TestGenerationPermissionsSharesPoolsAndPreservesUnknown(t *testing.T) {
	calls := 0
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.Host != "bks.adobe.io" || r.URL.Path != "/v1/feature-auth-profile" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Features []map[string]any `json:"features"`
		}
		if json.Unmarshal(raw, &body) != nil || len(body.Features) != 14 {
			t.Fatalf("invalid feature request: %s", raw)
		}
		raw, _ = json.Marshal(entitlementFixture())
		return jsonResponse(200, string(raw)), nil
	})})
	result, err := c.GenerationPermissions(context.Background(), fixtureAccount(t))
	if err != nil || calls != 1 || result.Status != "known" {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
	if len(result.Pools) != 3 || len(result.Models) != 14 {
		t.Fatalf("duplicated model pools: %+v", result)
	}
	var imagePool, videoPool, tastePool *GenerationPool
	for i := range result.Pools {
		p := &result.Pools[i]
		if p.ResourceType == "firefly_limited_taste" {
			tastePool = p
		} else if len(p.Kinds) == 1 && p.Kinds[0] == "video" {
			videoPool = p
		} else {
			imagePool = p
		}
	}
	if imagePool == nil || *imagePool.Available != 2 || len(imagePool.Models) != 7 {
		t.Fatalf("image shared pool: %+v", imagePool)
	}
	if videoPool == nil || *videoPool.Available != 1 || len(videoPool.Models) != 6 {
		t.Fatalf("video shared pool: %+v", videoPool)
	}
	if tastePool == nil || tastePool.Available != nil || tastePool.Used != nil {
		t.Fatalf("unknown became zero: %+v", tastePool)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "private-") {
		t.Fatal("upstream private pool reference leaked")
	}
	if !result.AnyAvailable() {
		t.Fatal("known free pools unavailable")
	}
}

func TestGenerationPermissionsPreservesExplicitVeoAudioDefaults(t *testing.T) {
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Features []struct {
				FeatureID string         `json:"featureId"`
				Params    map[string]any `json:"params"`
			} `json:"features"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		wantAudio := map[string]bool{
			"firefly_3p:external:veo_3_1_lite":          false,
			"firefly_3p:external:veo_3_fast":            false,
			"firefly_3p:external:gemini_omni_flash_1_1": true,
			"firefly_3p:external:gemini_omni_flash_1_2": true,
		}
		for _, feature := range body.Features {
			want, hasAudioContract := wantAudio[feature.FeatureID]
			got, present := feature.Params["generateAudio"]
			if hasAudioContract {
				if !present || got != want {
					t.Fatalf("%s default audio missing or changed: %v", feature.FeatureID, feature.Params)
				}
				delete(wantAudio, feature.FeatureID)
			} else if present {
				t.Fatalf("unexpected audio parameter for %s", feature.FeatureID)
			}
		}
		if len(wantAudio) != 0 {
			t.Fatalf("missing model features: %v", wantAudio)
		}
		raw, _ := json.Marshal(entitlementFixture())
		return jsonResponse(200, string(raw)), nil
	})})
	if _, err := c.GenerationPermissions(context.Background(), fixtureAccount(t)); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationPermissionsRejectsIncompleteAndPaidProfiles(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "missing_auth", "missing_resource", "paid"} {
		t.Run(kind, func(t *testing.T) {
			fixture := entitlementFixture()
			profiles := fixture["featuresAuthorization"].([]any)
			switch kind {
			case "missing":
				fixture["featuresAuthorization"] = profiles[1:]
			case "duplicate":
				fixture["featuresAuthorization"] = append(profiles, profiles[0])
			case "missing_auth":
				delete(profiles[0].(map[string]any), "authorizationResult")
			case "missing_resource":
				delete(profiles[0].(map[string]any), "selectedResource")
			case "paid":
				for _, raw := range profiles {
					raw.(map[string]any)["selectedResource"] = map[string]any{"resourceType": "firefly_credits", "computedCost": 1}
				}
			}
			c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				raw, _ := json.Marshal(fixture)
				return jsonResponse(200, string(raw)), nil
			})})
			out, err := c.GenerationPermissions(context.Background(), fixtureAccount(t))
			if kind == "paid" {
				if err != nil || out.AnyAvailable() {
					t.Fatalf("paid eligibility: %+v %v", out, err)
				}
			} else if err == nil {
				t.Fatal("incomplete response accepted")
			}
		})
	}
}
func TestGenerationQuotaPublicStateDoesNotUseGeneralCredits(t *testing.T) {
	a := fixtureAccount(t)
	zero := float64(0)
	a.ExpiresAt = time.Now().Add(time.Hour).Unix()
	a.Status = "active"
	a.Quota = Quota{Status: "known", Available: &zero, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Generation: &GenerationEntitlements{Status: "known", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Models: []ModelEntitlement{{Model: "gemini-3.1-flash-image", Authorized: true}}}}
	if Public(a)["status"] == "exhausted" {
		t.Fatal("general credits hide free generation allowance")
	}
	a.Quota.Generation.CheckedAt = time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	public := Public(a)["quota"].(Quota)
	if public.Generation.Status != "stale" || a.Quota.Generation.Status != "known" {
		t.Fatal("public stale projection mutated stored quota")
	}
}
