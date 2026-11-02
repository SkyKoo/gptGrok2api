package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"golang.org/x/net/websocket"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
				bytes := sizeTestPNG(t, 2, 2)
				_ = websocket.Message.Send(ws, fmt.Sprintf(`{"type":"image","order":0,"current_status":"completed","blob":%q}`, base64.StdEncoding.EncodeToString(bytes)))
			}})
			defer upstream.Close()
			s := imageTaskTestServer(t)
			s.cfg.ImagineWSURL = "ws" + strings.TrimPrefix(upstream.URL, "http")
			s.mediaProvider = provider.NewMedia(upstream.Client(), upstream.URL, upstream.URL, upstream.URL, upstream.URL, 3*time.Second)
			_, _, _, _ = s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "grok-image-token", "media_quota": testGrokMediaSnapshot()}})
			req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"`+modelName+`","prompt":"a red cat","size":"2560x1440"}`))
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
			if properties["enable_pro"] != true || properties["resolution_name"] != "2k" || properties["aspect_ratio"] != "16:9" || properties["num_generations"] != float64(1) {
				t.Fatal("legacy model dispatched low quality")
			}
		})
	}
}

func TestGrokAsyncImageTaskPreserves2KAndDownloadsOriginals(t *testing.T) {
	original := sizeTestPNG(t, 3, 2)
	var generations, downloads atomic.Int32
	received := make(chan map[string]any, 1)
	mux := http.NewServeMux()
	mux.Handle("/listen", websocket.Server{Handshake: func(*websocket.Config, *http.Request) error { return nil }, Handler: func(ws *websocket.Conn) {
		defer ws.Close()
		var reset, request map[string]any
		if websocket.JSON.Receive(ws, &reset) != nil || websocket.JSON.Receive(ws, &request) != nil {
			t.Error("missing request")
			return
		}
		generations.Add(1)
		input := request["item"].(map[string]any)["content"].([]any)[0].(map[string]any)
		received <- input["properties"].(map[string]any)
		// Two interleaved results; a large preview must not finish the task.
		for _, event := range []map[string]any{
			{"type": "image", "order": 0, "percentage_complete": 40, "url": "/preview.jpg", "blob": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("p"), 300<<10))},
			{"type": "json", "order": 1, "current_status": "completed", "url": "/b.jpg"},
			{"type": "json", "order": 0, "current_status": "completed", "url": "/a.jpg"},
		} {
			event["request_id"] = input["requestId"]
			if err := websocket.JSON.Send(ws, event); err != nil {
				t.Error(err)
				return
			}
		}
	}})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a.jpg" && r.URL.Path != "/b.jpg" {
			t.Errorf("preview or unexpected asset requested: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		downloads.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Write(original)
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	s := imageTaskTestServer(t)
	s.cfg.ImagineWSURL = "ws" + strings.TrimPrefix(upstream.URL, "http") + "/listen"
	s.mediaProvider = provider.NewMedia(upstream.Client(), upstream.URL, upstream.URL, upstream.URL, upstream.URL, 3*time.Second)
	_, _, _, _ = s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "test-token", "media_quota": testGrokMediaSnapshot()}})
	body := `{"client_task_id":"grok-2k-original","model":"grok-imagine-image-pro","prompt":"a red cat","n":2,"size":"2560x1440"}`
	response := imageTaskCall(s, "POST", "/api/image-tasks/generations", "api-secret", body)
	if response.Code != 202 {
		t.Fatalf("submit: %d %s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		response = imageTaskCall(s, "GET", "/api/image-tasks/grok-2k-original", "api-secret", "")
		var task imageTaskState
		if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		if task.Status == "error" {
			t.Fatal(task.Error)
		}
		if task.Status == "success" {
			if len(task.Data) != 2 || task.Size != "2560x1440" {
				t.Fatalf("lost size or results: %s", response.Body.String())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async task did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	properties := <-received
	if properties["resolution_name"] != "2k" || properties["aspect_ratio"] != "16:9" || properties["num_generations"] != float64(2) {
		t.Fatalf("async parameters lost: %+v", properties)
	}
	if generations.Load() != 1 || downloads.Load() != 2 {
		t.Fatalf("generations=%d downloads=%d", generations.Load(), downloads.Load())
	}
	files, err := os.ReadDir(s.cfg.ImageDataDir)
	if err != nil {
		t.Fatal(err)
	}
	originals := 0
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".png") {
			continue
		}
		originals++
		raw, err := os.ReadFile(filepath.Join(s.cfg.ImageDataDir, file.Name()))
		if err != nil || !bytes.Equal(raw, original) {
			t.Fatal("original bytes changed")
		}
	}
	if originals != 2 {
		t.Fatalf("expected two original PNG files, got %d", originals)
	}
}
