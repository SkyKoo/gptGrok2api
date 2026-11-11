package adobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"path/filepath"
	"testing"
)

func referenceFixture(t *testing.T, width, height int) ReferenceImage {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			im.SetNRGBA(x, y, color.NRGBA{R: 120, G: uint8(x), B: uint8(y), A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return ReferenceImage{Data: b.Bytes(), MIME: "image/png"}
}
func TestImageReferenceUsesCurrentUploadThenNamedAspect(t *testing.T) {
	a := fixtureAccount(t)
	ref := referenceFixture(t, 40, 30)
	calls := 0
	client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Cookie") != "" || r.GetBody != nil {
			t.Fatal("unsafe upload authentication/replay")
		}
		if calls == 1 {
			if r.URL.String() != uploadImageURL || r.Method != "POST" || r.Header.Get("Content-Type") != "image/png" || r.Header.Get("Authorization") != "Bearer "+a.Token {
				t.Fatal("incorrect UGS image upload")
			}
			data, _ := io.ReadAll(r.Body)
			if !bytes.Equal(data, ref.Data) {
				t.Fatal("image reference changed")
			}
			return jsonResponse(200, `{"images":[{"id":"fixture-reference"}]}`), nil
		}
		var payload map[string]any
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Fatal("invalid body")
		}
		if object(payload["size"])["width"] != float64(1200) || object(payload["size"])["height"] != float64(896) || object(payload["modelSpecificPayload"])["aspectRatio"] != "4:3" || payload["resolution"] != "1K" {
			t.Fatal("named output tier lost")
		}
		refs := payload["referenceBlobs"].([]any)
		if len(refs) != 1 || object(refs[0])["id"] != "fixture-reference" || object(refs[0])["usage"] != "general" {
			t.Fatal("reference handle missing")
		}
		return jsonResponse(200, `{"links":{"result":{"href":"https://bks-epo8552.adobe.io/v2/jobs/result/fixture"}}}`), nil
	})})
	j, err := client.SubmitImage(context.Background(), a, ImageRequest{Model: "gemini-3.1-flash-image", Prompt: "blue ceramic cup", Width: 1200, Height: 896, N: 1, References: []ReferenceImage{ref}})
	if err != nil || calls != 2 || j.Width != 1200 || j.Height != 896 || j.AspectRatio != "4:3" || j.ReferenceCount != 1 {
		t.Fatalf("job=%+v calls=%d error=%v", j, calls, err)
	}
	store := NewStore(filepath.Join(t.TempDir(), "accounts.enc"), "fixture")
	if err = store.SaveJob("image-request", j); err != nil {
		t.Fatal(err)
	}
	restored, err := store.LoadJob("image-request")
	if err != nil || restored.Width != j.Width || restored.Height != j.Height || restored.ReferenceCount != 1 {
		t.Fatal("restore lost immutable request parameters")
	}
}
func TestAllReferencesValidatedBeforeAnyUpload(t *testing.T) {
	valid := referenceFixture(t, 3, 3)
	cases := [][]ReferenceImage{
		{valid, {Data: []byte("not an image"), MIME: "image/png"}},
		{valid, {Data: valid.Data, MIME: "image/jpeg"}},
		{valid, valid, valid, valid, valid, valid, valid},
	}
	for _, refs := range cases {
		calls := 0
		c := NewClient(&http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })})
		_, err := c.SubmitImage(context.Background(), fixtureAccount(t), ImageRequest{Model: "gemini-3.1-flash-image", Prompt: "x", Width: 1024, Height: 1024, N: 1, References: refs})
		if err == nil || calls != 0 {
			t.Fatal("invalid trailing reference partially uploaded")
		}
	}
}
func TestUploadFailureNeverGeneratesOrReplays(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *http.Response
		err      error
	}{
		{"rejected", jsonResponse(403, `{"code":"access_error"}`), nil},
		{"missing_handle", jsonResponse(200, `{"images":[]}`), nil},
		{"network", nil, errors.New("private URL and token")},
		{"redirect", &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://evil.test/collect"}}, Body: io.NopCloser(bytes.NewReader(nil))}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != uploadImageURL {
					t.Fatal("generation submitted after failed upload")
				}
				return tc.response, tc.err
			})})
			j, err := c.SubmitImage(context.Background(), fixtureAccount(t), ImageRequest{Model: "gemini-3.1-flash-image", Prompt: "x", Width: 1024, Height: 1024, N: 1, References: []ReferenceImage{referenceFixture(t, 2, 2)}})
			var e *Error
			if !errors.As(err, &e) || e.Stage != "upload" || e.SubmissionUnknown || calls != 1 || j.Status != "REJECTED" {
				t.Fatalf("calls=%d job=%+v error=%v", calls, j, err)
			}
		})
	}
}
func TestAudioVideoReferencesFitCanvasAndKeepOrder(t *testing.T) {
	refs := []ReferenceImage{referenceFixture(t, 16, 12), referenceFixture(t, 12, 16)}
	calls := 0
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls <= 2 {
			cfg, format, err := image.DecodeConfig(r.Body)
			if err != nil || format != "png" || cfg.Width != 1920 || cfg.Height != 1080 {
				t.Fatalf("frame not fitted: %+v %s %v", cfg, format, err)
			}
			return jsonResponse(200, `{"images":[{"id":"frame-`+string(rune('0'+calls))+`"}]}`), nil
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["generateAudio"] != true {
			t.Fatal("audio dropped")
		}
		blobs := payload["referenceBlobs"].([]any)
		if len(blobs) != 2 {
			t.Fatal("frame handles missing")
		}
		for i, b := range blobs {
			if object(b)["usage"] != "frame" || object(b)["order"] != float64(i+1) {
				t.Fatal("first/last frame order lost")
			}
		}
		return nil, errors.New("generation response unknown")
	})})
	j, err := c.SubmitVideo(context.Background(), fixtureAccount(t), VideoRequest{Model: "veo-3.1-lite", Prompt: "gentle motion with birdsong", Width: 1920, Height: 1080, Duration: 8, Resolution: "1080p", AspectRatio: "16:9", GenerateAudio: true, References: refs})
	if err == nil || calls != 3 || j.Status != "SUBMISSION_UNKNOWN" || !j.GenerateAudio || j.ReferenceCount != 2 || j.Duration != 8 {
		t.Fatalf("job=%+v calls=%d error=%v", j, calls, err)
	}
}
func TestVariantMatchingRejectsUnsupportedCombinations(t *testing.T) {
	m, _ := FindModel("gemini-3.1-flash-image")
	if v, ok := m.MatchImage(1200, 896, 1, 6); !ok || v.AspectRatio != "4:3" || v.Resolution != "1K" {
		t.Fatal("named aspect tier missing")
	}
	if _, ok := m.MatchImage(1200, 896, 1, 7); ok {
		t.Fatal("web reference limit ignored")
	}
	if _, _, err := ValidateImageRequest(ImageRequest{Model: m.ID, Prompt: "x", Width: 1200, Height: 896, N: 1, AspectRatio: "75:56"}); err == nil {
		t.Fatal("pixel reduced ratio accepted as official aspect")
	}
	v, _ := FindModel("veo-3.1-lite")
	if _, ok := v.MatchVideo(1920, 1080, 8, "1080p", "16:9", true, 2); !ok {
		t.Fatal("supported audio frame tier missing")
	}
	if _, ok := v.MatchVideo(1920, 1080, 4, "1080p", "16:9", true, 2); ok {
		t.Fatal("unqualified duration accepted")
	}
}

func TestSubmission408IsUnknownAndNeverReplayed(t *testing.T) {
	calls := 0
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(408, `{"error_code":"timeout_error"}`), nil
	})})
	j, err := c.SubmitImage(context.Background(), fixtureAccount(t), ImageRequest{Model: "gemini-3.1-flash-image", Prompt: "a blue cup", Width: 1024, Height: 1024, N: 1})
	var upstream *Error
	if calls != 1 || !errors.As(err, &upstream) || !upstream.SubmissionUnknown || upstream.Code != "timeout_error" || j.Status != "SUBMISSION_UNKNOWN" {
		t.Fatalf("408 lost unknown state: job=%+v err=%v calls=%d", j, err, calls)
	}
}
