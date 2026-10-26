package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestGrokImageEditRunsUploadPostAndStreamingGeneration(t *testing.T) {
	var upload map[string]any
	var editPayload map[string]any
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload":
			if err := json.NewDecoder(r.Body).Decode(&upload); err != nil {
				t.Errorf("decode upload: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"fileMetadataId": "01234567-89ab-cdef-0123-456789abcdef",
				"fileUri":        upstream.URL + "/source.png",
			})
		case "/post":
			_ = json.NewEncoder(w).Encode(map[string]any{"post": map[string]any{"id": "edit-post-1"}})
		case "/chat":
			if err := json.NewDecoder(r.Body).Decode(&editPayload); err != nil {
				t.Errorf("decode image edit payload: %v", err)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"`+upstream.URL+`/generated.png","progress":100}}}}`)
			fmt.Fprintln(w, `data: [DONE]`)
		case "/generated.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(tinyPNG)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = filepath.Join(root, "data")
	cfg.ImageDataDir = filepath.Join(root, "data", "images")
	cfg.AccountsPath = filepath.Join(root, "data", "accounts.json")
	cfg.GrokRuntimeAccountsPath = filepath.Join(root, "data", "grok_runtime_accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "data", "auth_keys.json")
	cfg.ConfigPath = filepath.Join(root, "data", "config.json")
	cfg.MediaChatURL = upstream.URL + "/chat"
	cfg.MediaPostURL = upstream.URL + "/post"
	cfg.AssetUploadURL = upstream.URL + "/upload"
	cfg.AssetsBaseURL = upstream.URL
	cfg.Version = "test"
	cfg.RequestTimeout = 3 * time.Second
	server := New(cfg)
	server.mediaProvider = provider.NewMedia(upstream.Client(), cfg.MediaChatURL, cfg.MediaPostURL, cfg.AssetUploadURL, cfg.AssetsBaseURL, cfg.RequestTimeout)
	if _, _, _, err := server.grokRuntimeStore.AddAccounts(nil, []map[string]any{{
		"access_token":  "image-edit-token",
		"source_type":   "grok_protocol",
		"pool":          "basic",
		"cookie_header": "sso=image-edit-token; x-userid=user-1",
	}}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"grok-imagine-image-edit","prompt":"turn it into a night scene","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(tinyPNG) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer api-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("image edit failed: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"url":"/v1/files/image?id=`) {
		t.Fatalf("image edit did not return local file URL: %s", response.Body.String())
	}
	if upload["fileName"] != "image_url.png" || upload["fileMimeType"] != "image/png" {
		t.Fatalf("unexpected upload metadata: %#v", upload)
	}
	encoded, _ := upload["content"].(string)
	if encoded != base64.StdEncoding.EncodeToString(tinyPNG) {
		t.Fatal("image edit upload content changed")
	}
	metadata := mapValue(editPayload["responseMetadata"])
	modelOverride := mapValue(metadata["modelConfigOverride"])
	modelMap := mapValue(modelOverride["modelMap"])
	editConfig := mapValue(modelMap["imageEditModelConfig"])
	if editConfig["parentPostId"] != "edit-post-1" {
		t.Fatalf("image edit payload missing parent post: %#v", editPayload)
	}
}
