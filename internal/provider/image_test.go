package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
