package adobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func omni12Request() VideoRequest {
	return VideoRequest{Model: "gemini-omni-flash-1.2", Prompt: "A blue ceramic cup in soft daylight.", Width: 1280, Height: 720, Duration: 5, Resolution: "720p", AspectRatio: "16:9", GenerateAudio: true}
}

func TestOmni12SubmitMatchesOfficialVideoContract(t *testing.T) {
	// Official discovery-public.json, Omni 1.2, and the successful 2026-11-11
	// generation-preflight agree on these fields. UGS Le/Ri omits empty refs.
	// This offline contract test does not claim upstream generation succeeded.
	for _, mode := range []string{"present", "omitted"} {
		t.Run(mode, func(t *testing.T) {
			a := fixtureAccount(t)
			a.MediaSessionARPMode, a.MediaSessionARPSource = mode, "spl_runtime"
			a.Cookie = "sherlockToken=official-opaque-fixture"
			if mode == "omitted" {
				a.Cookie = ""
			}
			request := omni12Request()
			calls, sentRequestID := 0, ""
			client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost || r.URL.String() != "https://firefly-3p.ff.adobe.io/v2/3p-videos/generate-async" || r.GetBody != nil {
					t.Fatal("Omni request changed endpoint or permits automatic replay")
				}
				for key, want := range map[string]string{
					"Authorization": "Bearer " + a.Token, "x-api-key": "clio-playground-web",
					"Content-Type": "application/json", "Accept": "*/*", "Origin": "https://firefly.adobe.com",
					"Referer": "https://firefly.adobe.com/", "User-Agent": defaultUA,
					"x-nonce": Nonce(a.AccountID, request.Prompt),
				} {
					if r.Header.Get(key) != want {
						t.Errorf("Omni header %s differs from official request", key)
					}
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("x-account-id") != "" {
					t.Fatal("unexpected credential or account header")
				}
				_, arpPresent := r.Header[http.CanonicalHeaderKey("x-arp-session-id")]
				if mode == "present" && (!arpPresent || r.Header.Get("x-arp-session-id") != "official-opaque-fixture") || mode == "omitted" && arpPresent {
					t.Fatal("official media-session decision was changed")
				}
				sentRequestID = r.Header.Get("x-request-id")
				if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(sentRequestID) {
					t.Fatal("request ID is not a UUID v4")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				want := map[string]any{
					"modelId": "gemini-omni", "modelVersion": "omni-flash-1.2", "n": float64(1),
					"prompt": request.Prompt, "resolution": "720p", "duration": float64(5), "generateAudio": true,
					"generationSettings": map[string]any{"aspectRatio": "16:9"},
					"generationMetadata": map[string]any{"module": "text2video", "submodule": "ff-video-generate"},
					"output":             map[string]any{"storeInputs": true},
				}
				if !reflect.DeepEqual(body, want) {
					t.Fatalf("Omni body differs from official contract: got %v", body)
				}
				return jsonResponse(200, `{"links":{"result":{"href":"https://bks-epo8552.adobe.io/v2/jobs/result/omni-fixture"}}}`), nil
			})})
			job, err := client.SubmitVideo(context.Background(), a, request)
			if err != nil || calls != 1 || job.Status != "SUBMITTED" || job.RequestID != sentRequestID || job.ModelID != "gemini-omni" || job.ModelVersion != "omni-flash-1.2" || job.Kind != "video" || job.Width != 1280 || job.Height != 720 || job.Duration != 5 || job.Resolution != "720p" || job.AspectRatio != "16:9" || !job.GenerateAudio || job.ReferenceCount != 0 {
				t.Fatalf("Omni submission metadata changed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestOmni12PreflightAndSubmitKeepSameParametersAndReferenceRoles(t *testing.T) {
	for _, count := range []int{0, 2} {
		t.Run(fmt.Sprintf("references_%d", count), func(t *testing.T) {
			request := omni12Request()
			if count > 0 {
				ref := referenceFixture(t, 1280, 720)
				request.References = []ReferenceImage{ref, ref}
			}
			var preflight, submitted map[string]any
			var operations []string
			uploads := 0
			client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.Host != "firefly-3p.ff.adobe.io" || r.GetBody != nil {
					t.Fatal("unexpected transport or replayable request")
				}
				switch r.URL.Path {
				case "/v2/models/generation-preflight":
					operations = append(operations, "preflight")
					if r.URL.Query().Get("api") != "/v2/3p-videos/generate-async" || r.Header.Get("x-arp-session-id") != "" {
						t.Fatal("preflight used generation/session headers or wrong API")
					}
					if err := json.NewDecoder(r.Body).Decode(&preflight); err != nil {
						t.Fatal(err)
					}
					return jsonResponse(200, permissionFixture("firefly_3p:external:gemini_omni_flash_1_2", "firefly_complimentary_generations", "authorized", 0)), nil
				case "/v2/storage/image":
					operations = append(operations, "upload")
					if uploads >= count || r.Header.Get("Content-Type") != "image/png" {
						t.Fatal("unexpected reference upload")
					}
					data, err := io.ReadAll(r.Body)
					if err != nil || !bytes.Equal(data, request.References[uploads].Data) {
						t.Fatal("matching-size reference was altered")
					}
					uploads++
					return jsonResponse(200, fmt.Sprintf(`{"images":[{"id":"omni-frame-%d"}]}`, uploads)), nil
				case "/v2/3p-videos/generate-async":
					operations = append(operations, "submit")
					if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
						t.Fatal(err)
					}
					return jsonResponse(200, `{"links":{"result":{"href":"https://bks-epo8552.adobe.io/v2/jobs/result/omni-fixture"}}}`), nil
				default:
					t.Fatalf("unexpected endpoint: %s", r.URL.Path)
					return nil, errors.New("unexpected endpoint")
				}
			})})
			a := fixtureAccount(t)
			permission, err := client.PreflightVideo(context.Background(), a, request)
			if err != nil || !permission.Authorized || !permission.Free || permission.ResourceType != "firefly_complimentary_generations" || !reflect.DeepEqual(operations, []string{"preflight"}) {
				t.Fatalf("preflight was not a free read-only check: %v", err)
			}
			job, err := client.SubmitVideo(context.Background(), a, request)
			if err != nil || job.ReferenceCount != count {
				t.Fatalf("reference submission failed: %v", err)
			}
			wantOperations := []string{"preflight"}
			for i := 0; i < count; i++ {
				wantOperations = append(wantOperations, "upload")
			}
			wantOperations = append(wantOperations, "submit")
			if !reflect.DeepEqual(operations, wantOperations) {
				t.Fatalf("unexpected network operations: %v", operations)
			}
			if _, exists := preflight["referenceBlobs"]; exists {
				t.Fatal("preflight carried uploaded handles")
			}
			if _, exists := submitted["referenceBlobsMetadata"]; exists {
				t.Fatal("submission carried preflight-only metadata")
			}
			if count == 0 {
				if _, exists := submitted["referenceBlobs"]; exists {
					t.Fatal("text-only video did not omit optional reference field")
				}
				if _, exists := preflight["referenceBlobsMetadata"]; exists {
					t.Fatal("text-only preflight invented reference metadata")
				}
			} else {
				metadata, _ := preflight["referenceBlobsMetadata"].([]any)
				blobs, _ := submitted["referenceBlobs"].([]any)
				if len(metadata) != count || len(blobs) != count {
					t.Fatal("preflight and submission reference counts differ")
				}
				for i := 0; i < count; i++ {
					entry, blob := object(metadata[i]), object(blobs[i])
					meta := object(entry["metadata"])
					if entry["usage"] != "frame" || entry["order"] != float64(i+1) || meta["mimeType"] != "image/png" || meta["blobSize"] != float64(len(request.References[i].Data)) || !reflect.DeepEqual(object(meta["size"]), map[string]any{"width": float64(1280), "height": float64(720)}) || blob["usage"] != "frame" || blob["order"] != float64(i+1) || blob["id"] != fmt.Sprintf("omni-frame-%d", i+1) {
						t.Fatal("first/last frame roles or validated dimensions changed")
					}
				}
			}
			delete(preflight, "referenceBlobsMetadata")
			delete(submitted, "referenceBlobs")
			if !reflect.DeepEqual(preflight, submitted) {
				t.Fatal("generation parameters differ from the authorized preflight")
			}
		})
	}
}

func TestOmni12HTTP408SubmitsOnceAndPreservesSafeUnknownDiagnostic(t *testing.T) {
	calls, sentRequestID := 0, ""
	request := omni12Request()
	request.Prompt = "PRIVATE_PROMPT"
	client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v2/3p-videos/generate-async" || r.GetBody != nil {
			t.Fatal("timeout triggered an extra operation or replayable submit")
		}
		sentRequestID = r.Header.Get("x-request-id")
		response := jsonResponse(408, `{"error_code":"timeout_error","message":"PRIVATE_BODY"}`)
		response.ProtoMajor, response.ProtoMinor = 2, 0
		response.Header.Set("x-access-error", "PRIVATE_ACCESS_HEADER")
		response.Header.Set("x-access-user-context", "PRIVATE_ACCOUNT_CONTEXT")
		response.Header.Set("Set-Cookie", "PRIVATE_COOKIE")
		return response, nil
	})})
	job, err := client.SubmitVideo(context.Background(), fixtureAccount(t), request)
	var upstream *Error
	if calls != 1 || !errors.As(err, &upstream) || !upstream.SubmissionUnknown || upstream.Stage != "submit" || upstream.Status != 408 || err.Error() != "adobe_submit_timeout_error (HTTP 408)" || job.Status != "SUBMISSION_UNKNOWN" || job.PollURL != "" || job.RequestID == "" || job.RequestID != sentRequestID {
		t.Fatalf("408 lost the original uncertain submission: calls=%d err=%v", calls, err)
	}
	d := job.SubmissionDiagnostic
	if d == nil || !d.ResponseReceived || d.HTTPStatus != 408 || d.HTTPProto != "2" || d.ErrorCode != "timeout_error" || d.ErrorSource != "top_error_code" || d.AccessError != "unknown" || d.ElapsedMS < 0 || !reflect.DeepEqual(d, upstream.Diagnostic) {
		t.Fatalf("408 confused an upstream response with local transport timeout: %+v", d)
	}
	if job.Model != request.Model || job.ModelID != "gemini-omni" || job.ModelVersion != "omni-flash-1.2" || job.Kind != "video" || job.Width != 1280 || job.Height != 720 || job.Resolution != "720p" || job.Duration != 5 || !job.GenerateAudio || job.ReferenceCount != 0 {
		t.Fatal("failure lost exact video parameters")
	}
	raw, marshalErr := json.Marshal(job)
	if marshalErr != nil || strings.Contains(string(raw), "PRIVATE") || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("diagnostic retained private prompt, response or headers")
	}
}
