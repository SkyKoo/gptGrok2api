package adobe

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestExactPreflightRefusalDiagnosticsRemainFreeOnly(t *testing.T) {
	for _, tc := range []struct {
		name, resource, authorization, reason, wantCode, wantResource, wantAuthorization, wantReason string
		consumed                                                                                     int
	}{
		{"paid", "generative_credits", "authorized", "quota_available", "free_entitlement_unavailable", "generative_credits", "authorized", "quota_available", 0},
		{"denied", "firefly_complimentary_generations", "unauthorized_forbidden", "user_not_entitled", "free_entitlement_unavailable", "firefly_complimentary_generations", "unauthorized_forbidden", "user_not_entitled", 0},
		{"quota_exhausted", "firefly_complimentary_generations", "authorized", "quota_exhausted", "free_quota_exhausted", "firefly_complimentary_generations", "authorized", "quota_exhausted", 3},
		{"untrusted", "PRIVATE_RESOURCE", "PRIVATE_AUTH", "PRIVATE_REASON", "free_entitlement_unavailable", "unknown", "unknown", "unknown", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/v2/models/generation-preflight" || r.URL.Query().Get("api") != "/v2/3p-videos/generate-async" {
					t.Fatal("refusal uploaded or generated media")
				}
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				refs, _ := request["referenceBlobsMetadata"].([]any)
				if request["generateAudio"] != true || request["duration"] != float64(8) || request["resolution"] != "1080p" || len(refs) != 1 || request["referenceBlobs"] != nil {
					t.Fatal("preflight did not preserve the exact audio/reference request")
				}
				body := permissionFixture("firefly_3p:external:veo_3_1_lite", tc.resource, tc.authorization, tc.consumed)
				body = strings.ReplaceAll(body, "quota_available", tc.reason)
				return jsonResponse(200, body), nil
			})})
			permission, err := client.PreflightVideo(context.Background(), fixtureAccount(t), VideoRequest{Model: "veo-3.1-lite", Prompt: "PRIVATE_PROMPT", Width: 1920, Height: 1080, Duration: 8, Resolution: "1080p", AspectRatio: "16:9", GenerateAudio: true, References: []ReferenceImage{referenceFixture(t, 16, 9)}})
			d := PreflightDiagnosticFromError(err)
			if err == nil || permission.Free || calls != 1 || d == nil || d.ErrorCode != tc.wantCode || d.ResourceType != tc.wantResource || d.Authorization != tc.wantAuthorization || d.ReasonCode != tc.wantReason || !d.FeatureMatch {
				t.Fatalf("unexpected exact refusal: diagnostic=%+v error=%v", d, err)
			}
			raw, _ := json.Marshal(d)
			if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "image-shared-pool") {
				t.Fatal("preflight diagnostic leaked private response fields")
			}
		})
	}
}
