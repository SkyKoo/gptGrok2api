package adobe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func permissionFixture(feature, resource, result string, consumed int) string {
	return fmt.Sprintf(`{"featureId":%q,"authorizationResult":{"result":%q,"reasonCode":"quota_available"},"featureDetails":{},"selectedResource":{"resourceType":%q,"computedCost":1,"effectiveQuotaReferenceId":"image-shared-pool"},"effectiveQuotas":[{"effectiveQuotaReferenceId":"image-shared-pool","resourceType":%q,"provisionedQuota":3,"consumedQuota":%d,"refreshPeriod":"P1D"}]}`, feature, result, resource, resource, consumed)
}
func TestPreflightChecksFreeResourceWithoutUploadOrGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, resource, result, feature string
		consumed                        int
		allowed                         bool
	}{
		{"daily_free", "firefly_complimentary_generations", "authorized", "firefly_3p:external:nano_banana_3", 0, true},
		{"limited_taste", "firefly_limited_taste", "authorized", "firefly_3p:external:nano_banana_3", 0, true},
		{"fair_use", "firefly_fair_use", "authorized", "firefly_3p:external:nano_banana_3", 0, true},
		{"paid_credits", "generative_credits", "authorized", "firefly_3p:external:nano_banana_3", 0, false},
		{"not_entitled", "firefly_complimentary_generations", "unauthorized_forbidden", "firefly_3p:external:nano_banana_3", 0, false},
		{"exhausted", "firefly_complimentary_generations", "authorized", "firefly_3p:external:nano_banana_3", 3, false},
		{"wrong_model", "firefly_complimentary_generations", "authorized", "another_feature", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "firefly-3p.ff.adobe.io" || r.URL.Path != "/v2/models/generation-preflight" || r.URL.Query().Get("api") != "/v2/3p-images/generate-async" {
					t.Fatal("preflight uploaded/generated or used wrong route")
				}
				var b map[string]any
				_ = json.NewDecoder(r.Body).Decode(&b)
				if _, ok := b["referenceBlobs"]; ok {
					t.Fatal("preflight carried uploaded handles")
				}
				refs, _ := b["referenceBlobsMetadata"].([]any)
				if len(refs) != 1 || object(refs[0])["usage"] != "general" || object(object(refs[0])["metadata"])["mimeType"] != "image/png" {
					t.Fatal("reference metadata absent")
				}
				return jsonResponse(200, permissionFixture(tc.feature, tc.resource, tc.result, tc.consumed)), nil
			})})
			p, err := c.PreflightImage(context.Background(), fixtureAccount(t), ImageRequest{Model: "gemini-3.1-flash-image", Prompt: "blue cup", Width: 1200, Height: 896, N: 1, References: []ReferenceImage{referenceFixture(t, 4, 3)}})
			if (err == nil) != tc.allowed || calls != 1 {
				t.Fatalf("permission=%+v err=%v calls=%d", p, err, calls)
			}
			if tc.allowed && (p.Quota == nil || p.Quota.ReferenceID != "image-shared-pool" || p.Quota.RefreshPeriod != "P1D") {
				t.Fatal("shared pool lost")
			}
		})
	}
}
func TestAllFreeModelRequestsRespectCurrentProtocol(t *testing.T) {
	models := FreeModels()
	if len(models) != 14 {
		t.Fatalf("want 14 authorized model versions, got %d", len(models))
	}
	for _, m := range models {
		t.Run(m.ID, func(t *testing.T) {
			w, h := ParseSize(m.Size)
			v := m.Variants[0]
			var body map[string]any
			if m.Kind == "image" {
				body = imagePayload(m, v, ImageRequest{Model: m.ID, Prompt: "blue cup", Width: w, Height: h, N: 1}, nil)
			} else {
				body = videoPayload(m, v, VideoRequest{Model: m.ID, Prompt: "blue cup", Width: w, Height: h, Duration: m.Duration, Resolution: m.Resolution, AspectRatio: m.AspectRatio, GenerateAudio: m.DefaultAudio}, nil)
			}
			raw, _ := json.Marshal(body)
			_ = json.Unmarshal(raw, &body)
			switch m.ModelID {
			case "firefly-video":
				for _, field := range []string{"duration", "resolution", "generateAudio", "generationSettings"} {
					if _, ok := body[field]; ok {
						t.Fatalf("unexpected Firefly Video %s", field)
					}
				}
			case "gemini-omni":
				if body["generateAudio"] != true || body["size"] != nil || body["seeds"] != nil {
					t.Fatal("Omni const audio/size/seeds contract lost")
				}
			case "gpt-image":
				if body["seeds"] != nil || body["resolution"] != nil || object(body["generationSettings"])["detailLevel"] != float64(3) {
					t.Fatal("GPT image request contract lost")
				}
			case "humain":
				if body["size"] != nil || body["n"] != nil {
					t.Fatal("HUMAIN unsupported size/count leaked")
				}
			case "luma":
				if body["generateAudio"] != nil || object(body["modelSpecificPayload"])["aspect_ratio"] != "16:9" {
					t.Fatal("Luma required aspect payload missing")
				}
			}
			if m.ID == "gemini-3.1-flash-image-2.1" && body["size"] != nil {
				t.Fatal("Nano 2.1 explicit size is unsupported")
			}
		})
	}
	omni, _ := FindModel("gemini-omni-flash-1.1")
	if _, ok := omni.MatchVideo(1280, 720, 5, "720p", "16:9", false, 0); ok {
		t.Fatal("Omni silently accepted impossible silent output")
	}
}
func TestOfficialOpaqueARPSessionDoesNotRequireInventedFields(t *testing.T) {
	for _, value := range []string{"official-opaque-token", "eyJzaWQiOiJvZmZpY2lhbCIsImZ0ciI6Im9mZmljaWFsIn0="} {
		got, err := Sherlock("other=x; sherlockToken=" + value + "; other2=y")
		if err != nil || got != value {
			t.Fatal("official opaque session rejected or altered")
		}
	}
	for _, value := range []string{"", "bad\r\nHeader:value", "a b"} {
		if _, err := Sherlock("sherlockToken=" + value); err == nil {
			t.Fatal("empty or unsafe HTTP header token accepted")
		}
	}
}
