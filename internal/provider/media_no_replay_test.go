package provider

import (
	"context"
	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMediaGenerationRedirectNeverReplaysPOST(t *testing.T) {
	var submitted, redirected atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/second" {
			redirected.Add(1)
			w.WriteHeader(200)
			return
		}
		submitted.Add(1)
		w.Header().Set("Location", "/second")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	m := NewMedia(upstream.Client(), upstream.URL+"/first", upstream.URL, upstream.URL, upstream.URL, time.Second)
	if _, err := m.StreamChat(context.Background(), accounts.Account{Token: "fixture"}, map[string]any{"message": "fixture"}); err == nil {
		t.Fatal("redirect accepted")
	}
	if submitted.Load() != 1 || redirected.Load() != 0 {
		t.Fatalf("POST replayed: first=%d second=%d", submitted.Load(), redirected.Load())
	}
	o := NewOpenAIImage(upstream.URL, upstream.Client(), nil, time.Second)
	ctx := context.WithValue(context.Background(), mediaNoReplayContextKey{}, true)
	if _, err := o.doRequest(ctx, http.MethodPost, upstream.URL+"/first", "/first", accounts.Account{}, strings.NewReader("{}"), nil, false, false); err == nil {
		t.Fatal("image redirect accepted")
	}
	if submitted.Load() != 2 || redirected.Load() != 0 {
		t.Fatal("image POST replayed redirect")
	}
}
