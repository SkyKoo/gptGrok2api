package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestGrokImageGenerationUsesGrokRuntimePool(t *testing.T) {
	var cookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		switch r.URL.Path {
		case "/image":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"result":{"response":{"modelResponse":{"generatedImageUrls":["https://assets.grok.com/image.png"]}}}}`)
			fmt.Fprintln(w, `data: [DONE]`)
		case "/image.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(tinyPNG)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	s := imageTaskTestServer(t)
	s.mediaProvider = provider.NewMedia(http.DefaultClient, upstream.URL+"/image", upstream.URL, upstream.URL, upstream.URL, 10*time.Second)
	if _, _, _, err := s.grokRuntimeStore.AddAccounts([]string{"grok-image-token"}, nil); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"grok-imagine-image-lite","prompt":"a red cat"}`))
	req.Header.Set("Authorization", "Bearer api-secret")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(cookie, "sso=grok-image-token") {
		t.Fatalf("Grok request did not use the dedicated runtime account: %q", cookie)
	}
	if !strings.Contains(response.Body.String(), `"url":"/v1/files/image?id=`) {
		t.Fatalf("unexpected image response: %s", response.Body.String())
	}
}
