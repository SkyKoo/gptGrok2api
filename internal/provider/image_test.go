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
