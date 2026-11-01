package httpapi

import (
	"encoding/base64"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"golang.org/x/net/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGrokImageGenerationUsesProForLegacyAndCanonicalModels(t *testing.T) {
	for _, modelName := range []string{"grok-imagine-image-lite", "grok-imagine-image", "grok-imagine-image-pro"} {
		t.Run(modelName, func(t *testing.T) {
			received := make(chan map[string]any, 1)
			upstream := httptest.NewServer(websocket.Server{Handshake: func(*websocket.Config, *http.Request) error { return nil }, Handler: func(ws *websocket.Conn) {
				defer ws.Close()
				if !strings.Contains(ws.Request().Header.Get("Cookie"), "sso=grok-image-token") {
					t.Error("wrong account")
				}
				var reset, request map[string]any
				if err := websocket.JSON.Receive(ws, &reset); err != nil {
					t.Error(err)
					return
				}
				if err := websocket.JSON.Receive(ws, &request); err != nil {
					t.Error(err)
					return
				}
				received <- request
				bytes := append([]byte{0xff, 0xd8, 0xff}, make([]byte, 110<<10)...)
				_ = websocket.Message.Send(ws, fmt.Sprintf(`{"type":"image","url":"/images/final.jpg","blob":%q}`, base64.StdEncoding.EncodeToString(bytes)))
			}})
			defer upstream.Close()
			s := imageTaskTestServer(t)
			s.cfg.ImagineWSURL = "ws" + strings.TrimPrefix(upstream.URL, "http")
			s.mediaProvider = provider.NewMedia(upstream.Client(), upstream.URL, upstream.URL, upstream.URL, upstream.URL, 3*time.Second)
			_, _, _, _ = s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "grok-image-token", "media_quota": testGrokMediaSnapshot()}})
			req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"`+modelName+`","prompt":"a red cat"}`))
			req.Header.Set("Authorization", "Bearer api-secret")
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
			var request map[string]any
			select {
			case request = <-received:
			case <-time.After(time.Second):
				t.Fatal("no WS request")
			}
			item := request["item"].(map[string]any)
			content := item["content"].([]any)
			properties := content[0].(map[string]any)["properties"].(map[string]any)
			if properties["enable_pro"] != true {
				t.Fatal("legacy model dispatched low quality")
			}
		})
	}
}
