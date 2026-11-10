package adobe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixtureAccount(t *testing.T) Account {
	t.Helper()
	id := "fixture@AdobeID"
	claims, _ := json.Marshal(map[string]any{"user_id": id, "created_at": time.Now().UnixMilli(), "expires_in": 3600000})
	session, _ := json.Marshal(map[string]string{"sid": "fixture", "ark": "fixture", "bfp": "fixture", "ftr": "fixture"})
	return Account{ID: "adobe:" + id, AccountID: id, Email: "fixture@example.test", Token: "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture", Cookie: "sherlockToken=" + base64.StdEncoding.EncodeToString(session), Enabled: true}
}
func jsonResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func TestNonceMatchesJSSurrogateBoundary(t *testing.T) {
	// 255 BMP units plus half an emoji becomes U+FFFD when Node encodes UTF-8.
	p := strings.Repeat("a", 255) + "😀rest"
	want := Nonce("id", strings.Repeat("a", 255)+"�")
	if Nonce("id", p) != want {
		t.Fatal("UTF-16 truncation differs from JavaScript")
	}
}
func TestAdobeTokenLifetimeMillisecondsAndStrings(t *testing.T) {
	a := fixtureAccount(t)
	id, exp, err := TokenIdentity(a.Token)
	if err != nil || id != a.AccountID || exp < time.Now().Add(59*time.Minute).Unix() {
		t.Fatal("Adobe created_at/expires_in not normalized")
	}
	c := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"a","created_at":"1790000000000","expires_in":"3600000"}`))
	_, exp, err = TokenIdentity("e30." + c + ".sig")
	if err != nil || exp != 1790003600 {
		t.Fatalf("expiry=%d %v", exp, err)
	}
}
func TestSubmitIsSinglePOSTAndNonceForBothKinds(t *testing.T) {
	for _, kind := range []string{"image", "video"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			a := fixtureAccount(t)
			client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "POST" || r.Header.Get("x-nonce") != Nonce(a.AccountID, "test") || r.Header.Get("x-arp-session-id") == "" || r.GetBody != nil {
					t.Fatal("unsafe submit headers/replay")
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if kind == "video" {
					if body["duration"] != float64(8) || body["resolution"] != "1080p" || object(body["generationSettings"])["aspectRatio"] != "16:9" {
						t.Fatal("stale video payload")
					}
				}
				return nil, errors.New("network with private URL and token")
			})})
			var j Job
			var err error
			if kind == "image" {
				j, err = client.SubmitImage(context.Background(), a, ImageRequest{Model: "gemini-3.1-flash-image", Prompt: "test", Width: 1024, Height: 1024, N: 1})
			} else {
				j, err = client.SubmitVideo(context.Background(), a, VideoRequest{Model: "veo-3.1-lite", Prompt: "test", Width: 1920, Height: 1080, Duration: 8, Resolution: "1080p", AspectRatio: "16:9"})
			}
			if calls != 1 || err == nil || j.Status != "SUBMISSION_UNKNOWN" || strings.Contains(err.Error(), "private") {
				t.Fatalf("calls=%d job=%+v error=%v", calls, j, err)
			}
		})
	}
}
func TestPaidAndUnverifiedParametersNeverSubmit(t *testing.T) {
	calls := 0
	c := NewClient(&http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected") })})
	a := fixtureAccount(t)
	for _, r := range []ImageRequest{{Model: "gpt-image-2.5-sunburst", Prompt: "x", Width: 1024, Height: 1024, N: 1}, {Model: "gemini-3.1-flash-image", Prompt: "x", Width: 2048, Height: 2048, N: 1}, {Model: "gemini-3.1-flash-image", Prompt: "x", Width: 1024, Height: 1024, N: 2}} {
		if _, e := c.SubmitImage(context.Background(), a, r); e == nil {
			t.Fatal("unsupported request accepted")
		}
	}
	if calls != 0 {
		t.Fatal("unsupported tiers reached upstream")
	}
}
func TestPollWaitsForTerminalStatusEvenWithOutputAnd100Percent(t *testing.T) {
	count := 0
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		count++
		status := "IN_PROGRESS"
		if count == 2 {
			status = "COMPLETED"
		}
		return jsonResponse(200, `{"status":"`+status+`","progress":100,"outputs":[{"image":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/result.png?secret=1"}}]}`), nil
	})})
	a := fixtureAccount(t)
	j := Job{Kind: "image", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture"}
	j, e := c.Poll(context.Background(), a, j)
	if e != nil || j.Status != "IN_PROGRESS" || len(j.OutputURLs) != 0 {
		t.Fatalf("premature result: %+v %v", j, e)
	}
	j, e = c.Poll(context.Background(), a, j)
	if e != nil || j.Status != "COMPLETED" || len(j.OutputURLs) != 1 {
		t.Fatal("terminal output missing")
	}
}
func TestURLsAndDownloadsNeverLeakAuthentication(t *testing.T) {
	calls := 0
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("x-api-key") != "" {
			t.Fatal("credential leaked to media")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("png"))}, nil
	})})
	for _, u := range []string{"http://127.0.0.1/x", "https://evil.test/x", "https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com.evil.test/x", "https://user@pre-signed-firefly-prod.s3-accelerate.amazonaws.com/x"} {
		if _, _, e := c.Download(context.Background(), u, 100); e == nil {
			t.Fatal("unsafe URL accepted")
		}
	}
	if calls != 0 {
		t.Fatal("untrusted request sent")
	}
	_, _, e := c.Download(context.Background(), "https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/x", 100)
	if e != nil || calls != 1 {
		t.Fatal(e)
	}
}
func TestRefreshRejectsGuestAndNeverUsesFireflyCookieForIMS(t *testing.T) {
	a := fixtureAccount(t)
	calls := 0
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Cookie") != "imsSession=private" || r.Header.Get("Authorization") != "" {
			t.Fatal("wrong scoped credentials")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "guest_allowed=false") {
			t.Fatal("guest token allowed")
		}
		guest := fixtureAccount(t)
		claims := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"different","exp":9999999999}`))
		guest.Token = "e30." + claims + ".sig"
		raw, _ := json.Marshal(map[string]string{"access_token": guest.Token})
		return jsonResponse(200, string(raw)), nil
	})})
	if _, e := c.Refresh(context.Background(), a); e == nil || calls != 0 {
		t.Fatal("unscoped cookie sent")
	}
	a.RefreshCookie = "imsSession=private"
	if _, e := c.Refresh(context.Background(), a); !errors.Is(e, ErrIdentity) {
		t.Fatal("identity mismatch not rejected")
	}
}
func TestBalanceDistinguishesUnknownAndZero(t *testing.T) {
	for _, tc := range []struct {
		body, status string
		missing      bool
	}{{`{"total":{"quota":{}}}`, "unknown", true}, {`{"total":{"quota":{"total":10,"used":10,"available":0}}}`, "known", false}} {
		c := NewClient(&http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) { return jsonResponse(200, tc.body), nil })})
		q, e := c.Balance(context.Background(), fixtureAccount(t))
		if e != nil || q.Status != tc.status || (q.Available == nil) != tc.missing {
			t.Fatalf("quota distinction lost: %+v %v", q, e)
		}
	}
}

// This fixture is the successful 2026-11-10 Node Veo request, with only the
// account identity replaced. It protects the verified external contract.
func TestVeoPayloadMatchesVerifiedNodeProtocol(t *testing.T) {
	const prompt = "A single continuous studio product shot of a small matte red ceramic teapot beside one cobalt blue cup on a pale wooden table. Thin steam gently rises from the teapot spout. The camera slowly moves closer. Soft daylight, plain cream background, realistic ceramic texture. No people, no text, no logo. Quiet room ambience."
	a := fixtureAccount(t)
	calls := 0
	c := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.String() != "https://firefly-3p.ff.adobe.io/v2/3p-videos/generate-async" {
			t.Fatal("endpoint/query differs from successful Node fixture")
		}
		expectedHeaders := map[string]string{"Accept": "*/*", "Content-Type": "application/json", "Origin": "https://firefly.adobe.com", "Referer": "https://firefly.adobe.com/", "Accept-Language": "en-US,en;q=0.9", "User-Agent": defaultUA, "sec-ch-ua": `"Chromium";v="145", "Google Chrome";v="145", "Not/A)Brand";v="99"`, "sec-ch-ua-mobile": "?0", "sec-ch-ua-platform": `"Windows"`, "sec-fetch-site": "cross-site", "sec-fetch-mode": "cors", "sec-fetch-dest": "empty", "x-api-key": "clio-playground-web", "Authorization": "Bearer " + a.Token, "x-nonce": "9bf9085d3ede756e0cbdcced4446aadd1ce36380566cdc7abb954413ed022f41"}
		for k, want := range expectedHeaders {
			if r.Header.Get(k) != want {
				t.Errorf("header %s differs from Node protocol", k)
			}
		}
		arp, _ := Sherlock(a.Cookie)
		if r.Header.Get("x-arp-session-id") != arp || r.Header.Get("Cookie") != "" || r.Header.Get("x-account-id") != "" {
			t.Fatal("generation session header differs")
		}
		if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(r.Header.Get("x-request-id")) {
			t.Fatal("request ID not UUID v4")
		}
		var got map[string]any
		if json.NewDecoder(r.Body).Decode(&got) != nil {
			t.Fatal("invalid JSON")
		}
		seeds, ok := got["seeds"].([]any)
		if !ok || len(seeds) != 1 {
			t.Fatal("invalid seeds")
		}
		seedValue, ok := seeds[0].(float64)
		if !ok || seedValue < 1 || seedValue >= 1000000 {
			t.Fatal("seed outside verified range")
		}
		got["seeds"] = []any{float64(445760)}
		want := map[string]any{"modelId": "veo", "modelVersion": "3.1-lite-generate", "n": float64(1), "seeds": []any{float64(445760)}, "prompt": prompt, "output": map[string]any{"storeInputs": true}, "size": map[string]any{"width": float64(1920), "height": float64(1080)}, "resolution": "1080p", "duration": float64(8), "generateAudio": false, "referenceBlobs": []any{}, "generationMetadata": map[string]any{"module": "text2video", "submodule": "ff-video-generate"}, "generationSettings": map[string]any{"aspectRatio": "16:9"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("Veo request fields differ from successful Node fixture")
		}
		return jsonResponse(200, `{"links":{"result":{"href":"https://bks-epo8552.adobe.io/v2/jobs/result/fixture"}}}`), nil
	})})
	j, e := c.SubmitVideo(context.Background(), a, VideoRequest{Model: "veo-3.1-lite", Prompt: prompt, Width: 1920, Height: 1080, Duration: 8, Resolution: "1080p", AspectRatio: "16:9"})
	if e != nil || calls != 1 || j.Status != "SUBMITTED" {
		t.Fatalf("fixture submit failed: %v", e)
	}
}
