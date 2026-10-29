package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunVideoJobResolvesGeneratedPostAsset(t *testing.T) {
	videoBytes := []byte("fake-mp4")
	var sawVideoPayload map[string]any
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/post":
			if r.Header.Get("Cookie") == "" {
				t.Error("video post request is missing cookies")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"post": map[string]any{"id": "parent-1"}})
		case "/chat":
			if err := json.NewDecoder(r.Body).Decode(&sawVideoPayload); err != nil {
				t.Errorf("decode video payload: %v", err)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":40,"videoId":"video-1","videoPostId":"video-post-1"}}}}`)
			fmt.Fprintln(w, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoId":"video-1","videoPostId":"video-post-1"}}}}`)
			fmt.Fprintln(w, `data: [DONE]`)
		case "/users/user-1/generated/video-post-1/generated_video.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = filepath.Join(root, "data")
	cfg.VideoDataDir = filepath.Join(root, "data", "videos")
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
	if _, _, _, err := server.grokRuntimeStore.AddAccounts(nil, []map[string]any{{
		"access_token":  "video-token",
		"source_type":   "grok_protocol",
		"pool":          "basic",
		"cookie_header": "sso=video-token; x-userid=user-1",
	}}); err != nil {
		t.Fatal(err)
	}
	job := &videoJob{ID: "video_test", Model: "grok-imagine-video", Status: "queued", Prompt: "animate this"}
	server.videoMu.Lock()
	server.videoJobs[job.ID] = job
	server.videoMu.Unlock()

	server.runVideoJob(job, "animate this", "16:9", "720p", 6, "custom", nil)

	server.videoMu.RLock()
	status, path, videoURL := job.Status, job.ContentPath, job.VideoURL
	server.videoMu.RUnlock()
	if status != "completed" || path == "" || videoURL == "" {
		t.Fatalf("video job did not complete: status=%q path=%q url=%q error=%v", status, path, videoURL, job.Error)
	}
	if !strings.Contains(videoURL, "/generated/video-post-1/generated_video.mp4") {
		t.Fatalf("unexpected generated video URL: %q", videoURL)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(videoBytes) {
		t.Fatalf("stored video mismatch: err=%v bytes=%q", err, got)
	}
	configValue, ok := sawVideoPayload["responseMetadata"].(map[string]any)
	if !ok || configValue["modelConfigOverride"] == nil {
		t.Fatalf("video payload missing model config: %#v", sawVideoPayload)
	}
}

func TestVideosCreateRejectsMalformedMultipartBeforeStartingJob(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = filepath.Join(root, "data")
	cfg.AccountsPath = filepath.Join(root, "data", "accounts.json")
	cfg.GrokRuntimeAccountsPath = filepath.Join(root, "data", "grok_runtime_accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "data", "auth_keys.json")
	cfg.ConfigPath = filepath.Join(root, "data", "config.json")
	cfg.Version = "test"
	server := New(cfg)
	request := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(""))
	request.Header.Set("Authorization", "Bearer api-secret")
	request.Header.Set("Content-Type", "multipart/form-data; boundary=missing")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid multipart to fail before job creation, got %d: %s", response.Code, response.Body.String())
	}
	server.videoMu.RLock()
	count := len(server.videoJobs)
	server.videoMu.RUnlock()
	if count != 0 {
		t.Fatalf("invalid request created %d video jobs", count)
	}
}

func TestVideosAPICompletesAndServesContent(t *testing.T) {
	videoBytes := []byte("api-video")
	var videoPayload map[string]any
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode reference upload: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"fileUri": upstream.URL + "/source.png"})
		case "/post":
			_ = json.NewEncoder(w).Encode(map[string]any{"post": map[string]any{"id": "parent-api"}})
		case "/chat":
			if err := json.NewDecoder(r.Body).Decode(&videoPayload); err != nil {
				t.Errorf("decode API video payload: %v", err)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"video-api"}}}}`)
			fmt.Fprintln(w, `data: [DONE]`)
		case "/users/user-api/generated/video-api/generated_video.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = filepath.Join(root, "data")
	cfg.VideoDataDir = filepath.Join(root, "data", "videos")
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
	if _, _, _, err := server.grokRuntimeStore.AddAccounts(nil, []map[string]any{{
		"access_token":  "video-api-token",
		"source_type":   "grok_protocol",
		"pool":          "basic",
		"cookie_header": "sso=video-api-token; x-userid=user-api",
	}}); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "grok-imagine-video"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("prompt", "a paper plane"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("input_reference[]", "reference.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(tinyPNG); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRequest(http.MethodPost, "/v1/videos", &body)
	create.Header.Set("Authorization", "Bearer api-secret")
	create.Header.Set("Content-Type", writer.FormDataContentType())
	created := httptest.NewRecorder()
	server.Handler().ServeHTTP(created, create)
	if created.Code != http.StatusOK {
		t.Fatalf("video create failed: %d %s", created.Code, created.Body.String())
	}
	var initial videoJob
	if err := json.Unmarshal(created.Body.Bytes(), &initial); err != nil || initial.ID == "" {
		t.Fatalf("invalid video create response: %v %s", err, created.Body.String())
	}

	if initial.Status != "queued" || initial.Progress != 0 || initial.Error != nil {
		t.Fatal("submission response was mutated by background worker")
	}
	deadline := time.Now().Add(2 * time.Second)
	var latest videoJob
	for time.Now().Before(deadline) {
		poll := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/videos/"+initial.ID, nil)
		request.Header.Set("Authorization", "Bearer api-secret")
		server.Handler().ServeHTTP(poll, request)
		if poll.Code != http.StatusOK {
			t.Fatalf("video poll failed: %d %s", poll.Code, poll.Body.String())
		}
		if err := json.Unmarshal(poll.Body.Bytes(), &latest); err != nil {
			t.Fatal(err)
		}
		if latest.Status == "completed" || latest.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if latest.Status != "completed" {
		t.Fatalf("video API did not complete: %#v", latest)
	}
	content := httptest.NewRecorder()
	contentRequest := httptest.NewRequest(http.MethodGet, "/v1/videos/"+initial.ID+"/content", nil)
	contentRequest.Header.Set("Authorization", "Bearer api-secret")
	server.Handler().ServeHTTP(content, contentRequest)
	got, _ := io.ReadAll(content.Body)
	if content.Code != http.StatusOK || string(got) != string(videoBytes) {
		t.Fatalf("video content mismatch: status=%d bytes=%q", content.Code, got)
	}
	metadata := mapValue(videoPayload["responseMetadata"])
	modelOverride := mapValue(metadata["modelConfigOverride"])
	modelMap := mapValue(modelOverride["modelMap"])
	videoConfig := mapValue(modelMap["videoGenModelConfig"])
	refs, ok := videoConfig["imageReferences"].([]any)
	if !ok || len(refs) != 1 || !strings.Contains(stringValue(refs[0]), "/source.png") {
		t.Fatalf("image-to-video reference missing from payload: %#v", videoPayload)
	}
}
