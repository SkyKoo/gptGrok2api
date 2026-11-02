package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func TestResolveImageUsesInlineImagineBytesBeforeAssetURL(t *testing.T) {
	raw := []byte("inline-image-bytes")
	media := NewMedia(nil, "", "", "", "", 0)
	dir := t.TempDir()
	result, err := media.ResolveImage(context.Background(), accounts.Account{}, ImageResult{
		URL:    "https://assets.grok.com/users/user/generated/post/image.jpg",
		Base64: base64.StdEncoding.EncodeToString(raw),
		MIME:   "image/jpeg",
	}, "url", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	urlValue := result["url"]
	if !strings.HasPrefix(urlValue, "/v1/files/image?id=") {
		t.Fatalf("unexpected local image URL: %q", urlValue)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("inline image was not persisted: %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil || string(stored) != string(raw) {
		t.Fatalf("stored image bytes changed: %v %q", err, stored)
	}
}

func TestGenerateLiteSkipsPreviewFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"/preview.png","progress":25}}}}`)
		fmt.Fprintln(w, `data: {"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"/final.png","progress":100}}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer server.Close()
	media := NewMedia(server.Client(), server.URL, "", "", "", 3*time.Second)
	results, err := media.GenerateLite(context.Background(), accounts.Account{Token: "sso"}, "a cat", "fast", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !strings.HasSuffix(results[0].URL, "/final.png") {
		t.Fatalf("preview frame leaked into lite result: %#v", results)
	}
}

func TestGenerateLiteNoImagesIncludesSafeDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"result":{"response":{"modelResponse":{"message":"upstream did not include an image"}}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer server.Close()
	media := NewMedia(server.Client(), server.URL, "", "", "", 3*time.Second)
	_, err := media.GenerateLite(context.Background(), accounts.Account{Token: "sso"}, "a cat", "fast", 1)
	if err == nil {
		t.Fatal("expected no-image error")
	}
	var upstream *protocol.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("expected upstream error, got %T: %v", err, err)
	}
	if upstream.Status != http.StatusBadGateway {
		t.Fatalf("unexpected status: %d", upstream.Status)
	}
	for _, want := range []string{`"format":"sse"`, `"json_events":1`, `"text_events":1`, `"failure":"no_images"`} {
		if !strings.Contains(upstream.Message, want) {
			t.Fatalf("diagnostics missing %s: %s", want, upstream.Message)
		}
	}
	if strings.Contains(upstream.Message, "upstream did not include an image") {
		t.Fatal("diagnostics leaked upstream response text")
	}
}

func TestGenerateLiteClassifiesRateLimitDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"result":{"error":{"code":"rate_limit_exceeded"}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer server.Close()
	media := NewMedia(server.Client(), server.URL, "", "", "", 3*time.Second)
	_, err := media.GenerateLite(context.Background(), accounts.Account{Token: "sso"}, "a cat", "fast", 1)
	if err == nil {
		t.Fatal("expected rate-limit error")
	}
	var upstream *protocol.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("expected upstream error, got %T: %v", err, err)
	}
	if upstream.Status != http.StatusTooManyRequests || !strings.Contains(upstream.Message, `"failure":"rate_limited"`) {
		t.Fatalf("unexpected rate-limit diagnostics: status=%d message=%s", upstream.Status, upstream.Message)
	}
}

func TestResolveImagineUsesOriginalAndNeverFallsBackToPreview(t *testing.T) {
	raw := imagineTestPNG(t)
	for _, kind := range []string{"valid", "html", "truncated", "denied"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/final.jpg" || !strings.Contains(r.Header.Get("Cookie"), "sso=test-token") {
					t.Error("wrong asset request")
				}
				w.Header().Set("Content-Type", "image/jpeg") // deliberately wrong MIME
				switch kind {
				case "valid":
					w.Write(raw)
				case "html":
					w.Write([]byte("<html>Storage error</html>"))
				case "truncated":
					w.Write(raw[:len(raw)/2])
				case "denied":
					w.WriteHeader(http.StatusForbidden)
				}
			}))
			defer server.Close()
			media := NewMedia(server.Client(), "", "", "", server.URL, time.Second)
			dir := t.TempDir()
			result, err := media.ResolveImage(context.Background(), accounts.Account{Token: "test-token"}, ImageResult{URL: "https://assets.grok.com/final.jpg", Base64: base64.StdEncoding.EncodeToString([]byte("preview")), MIME: "image/jpeg", ImagineFinal: true}, "url", dir, "")
			if calls != 1 {
				t.Fatalf("original fetch count=%d", calls)
			}
			files, _ := os.ReadDir(dir)
			if kind != "valid" {
				if err == nil || len(files) > 0 {
					t.Fatal("bad original replaced by preview or persisted")
				}
				return
			}
			if err != nil || result["url"] == "" || len(files) != 1 {
				t.Fatalf("missing original: %v", err)
			}
			saved, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
			if err != nil || string(saved) != string(raw) || !strings.HasSuffix(files[0].Name(), ".png") {
				t.Fatal("original was re-encoded or MIME trusted")
			}
		})
	}
}

func TestResolvePublicImagineOriginalWithoutCredentialsOrRedirects(t *testing.T) {
	raw := imagineTestPNG(t)
	for _, kind := range []string{"valid", "redirect", "denied", "network", "untrusted"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			originalURL := "https://imagine-public.x.ai/imagine-public/images/final.jpg?signature=private-signature"
			jar, _ := cookiejar.New(nil)
			u, _ := url.Parse(originalURL)
			jar.SetCookies(u, []*http.Cookie{{Name: "sso", Value: "jar-token"}})
			client := &http.Client{Jar: jar, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != originalURL {
					t.Error("followed redirect or changed original URL")
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
					t.Error("credentials sent to public asset host")
				}
				status := 200
				body := raw
				header := http.Header{"Content-Type": []string{"image/jpeg"}}
				switch kind {
				case "redirect":
					status = 302
					header.Set("Location", "https://assets.grok.com/leak?signature=private-signature")
				case "denied":
					status = 403
					body = []byte("access denied private-signature")
				case "network":
					return nil, fmt.Errorf("signed URL failed: %s", originalURL)
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
			})}
			media := NewMedia(client, "", "", "", "", time.Second)
			if kind == "untrusted" {
				originalURL = "https://untrusted.example/final.jpg"
			}
			got, err := media.ResolveImage(context.Background(), accounts.Account{Token: "private-token", Fields: map[string]any{"cookie_header": "sso=private-cookie"}}, ImageResult{URL: originalURL, Base64: base64.StdEncoding.EncodeToString(raw), ImagineFinal: true}, "b64_json", "", "")
			expectedCalls := 1
			if kind == "untrusted" {
				expectedCalls = 0
			}
			if calls != expectedCalls {
				t.Fatalf("requests=%d", calls)
			}
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				decoded, _ := base64.StdEncoding.DecodeString(got["b64_json"])
				if !bytes.Equal(decoded, raw) || got["mime"] != "image/png" {
					t.Fatal("original changed or wrong MIME")
				}
			} else {
				if err == nil || got != nil {
					t.Fatal("failed original fell back to preview")
				}
				if strings.Contains(err.Error(), "private-signature") || strings.Contains(err.Error(), "https://") {
					t.Fatal("signed URL or body leaked")
				}
				if kind != "untrusted" {
					var up *protocol.UpstreamError
					if !errors.As(err, &up) || up.Status != 502 {
						t.Fatalf("storage error affects account status: %v", err)
					}
				}
			}
			if client.Jar != jar || client.CheckRedirect != nil {
				t.Fatal("shared client mutated")
			}
		})
	}
}
