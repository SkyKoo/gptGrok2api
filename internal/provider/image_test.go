package provider

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
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
