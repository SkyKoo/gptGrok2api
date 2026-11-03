package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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
		"media_quota":   testGrokMediaSnapshot(),
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

// A challenge at upload must remain visible and must never turn a reference edit
// into an unrelated text-to-image generation or invalidate the account.
func TestGrokImageEditUploadChallengeStopsBeforeGeneration(t *testing.T) {
	uploads, generation := 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upload" {
			generation++
			http.Error(w, "unexpected generation", http.StatusInternalServerError)
			return
		}
		uploads++
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html>SECRET_COOKIE https://private.invalid/?token=SECRET</html>")
	}))
	defer upstream.Close()
	s := New(adminTestConfig(t.TempDir()))
	s.mediaProvider = provider.NewMedia(upstream.Client(), upstream.URL+"/chat", upstream.URL+"/post", upstream.URL+"/upload", upstream.URL, s.cfg.RequestTimeout)
	if _, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": "edit-token", "enabled": true, "status": "active", "media_quota": testGrokMediaSnapshot()}}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"grok-imagine-image-edit","prompt":"a paper plane","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(tinyPNG) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer api-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != 403 || uploads != 1 || generation != 0 {
		t.Fatalf("status=%d uploads=%d generation=%d", response.Code, uploads, generation)
	}
	if !strings.Contains(response.Body.String(), "Cloudflare") || strings.Contains(response.Body.String(), "SECRET") || strings.Contains(response.Body.String(), "private.invalid") {
		t.Fatal("challenge explanation missing or secret exposed")
	}
	rows, _, err := s.grokRuntimeStore.AccountSnapshot()
	if err != nil || len(rows) != 1 || rows[0]["status"] != "active" {
		t.Fatalf("challenge invalidated account: %v", err)
	}
	quotas := mapValue(mapValue(rows[0]["media_quota"])["quotas"])
	if fmt.Sprint(mapValue(quotas["imagePro"])["remaining"]) != "10" || fmt.Sprint(mapValue(quotas["video720p"])["remaining"]) != "10" {
		t.Fatal("upload challenge changed generation quota")
	}
	s.monitor.mu.Lock()
	defer s.monitor.mu.Unlock()
	if len(s.monitor.completed) != 1 || s.monitor.completed[0].Metrics["upload_error_kind"] != "verification_required" || s.monitor.completed[0].Metrics["upload_challenge"] != true {
		t.Fatal("request log lost upload challenge diagnostics")
	}
}

func TestGrokReferenceV2AssetsReachImageAndVideoGeneration(t *testing.T) {
	for _, mode := range []string{"image", "video"} {
		t.Run(mode, func(t *testing.T) {
			uploads, starts := 0, 0
			cookies := []string{}
			client := &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				header := http.Header{"Content-Type": []string{"application/json"}}
				raw := ""
				if r.URL.Host == "storage.googleapis.com" {
					uploads++
					data, _ := io.ReadAll(r.Body)
					if r.Method != "PUT" || !bytes.Equal(data, tinyPNG) || r.Header.Get("Cookie") != "" {
						t.Error("reference bytes or credential isolation lost")
					}
				} else {
					cookies = append(cookies, r.Header.Get("Cookie"))
					switch r.URL.Path {
					case "/rest/app-chat/upload-file-v2/init":
						raw = `{"uploadId":"test-upload","uploadMethod":"UPLOAD_METHOD_SINGLE_PUT","singlePut":{"url":"https://storage.googleapis.com/test/reference","requiredHeaders":{"Content-Type":"image/png"}}}`
					case "/rest/app-chat/upload-file-v2/complete":
						raw = `{"fileMetadata":{"fileMetadataId":"01234567-89ab-cdef-0123-456789abcdef","fileUri":"https://assets.grok.com/users/test/reference.png"}}`
					case "/post":
						raw = `{"post":{"id":"parent"}}`
					case "/chat":
						starts++
						var payload map[string]any
						_ = json.NewDecoder(r.Body).Decode(&payload)
						models := mapValue(mapValue(mapValue(payload["responseMetadata"])["modelConfigOverride"])["modelMap"])
						key := "imageEditModelConfig"
						if mode == "video" {
							key = "videoGenModelConfig"
						}
						refs := anyList(mapValue(models[key])["imageReferences"])
						if len(refs) != 1 || refs[0] != "https://assets.grok.com/users/test/reference.png" {
							t.Errorf("uploaded reference missing from generation: %v", refs)
						}
						header.Set("Content-Type", "text/event-stream")
						if mode == "image" {
							raw = "data: {\"result\":{\"response\":{\"streamingImageGenerationResponse\":{\"imageUrl\":\"https://grok.test/result.png\",\"progress\":100}}}}\n\ndata: [DONE]\n\n"
						} else {
							raw = "data: {\"result\":{\"response\":{\"streamingVideoGenerationResponse\":{\"parentPostId\":\"parent\",\"videoId\":\"output\",\"videoUrl\":\"https://grok.test/result.mp4\",\"progress\":100}}}}\n\ndata: [DONE]\n\n"
						}
					case "/result.png":
						header.Set("Content-Type", "image/png")
						raw = string(tinyPNG)
					case "/result.mp4":
						header.Set("Content-Type", "video/mp4")
						raw = string(videoTestMP4("output", "meta"))
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
					}
				}
				return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
			})}
			s := New(adminTestConfig(t.TempDir()))
			s.cfg.VideoDataDir = t.TempDir()
			s.mediaProvider = provider.NewMedia(client, "https://grok.test/chat", "https://grok.test/post", "https://grok.test/rest/app-chat/upload-file-v2", "", s.cfg.RequestTimeout)
			for _, token := range []string{"account-a", "account-b"} {
				if _, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": token, "status": "active", "enabled": true, "media_quota": testGrokMediaSnapshot()}}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "image" {
				body := `{"model":"grok-imagine-image-edit","prompt":"a paper plane","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(tinyPNG) + `"}`
				request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer api-secret")
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				s.Handler().ServeHTTP(response, request)
				if response.Code != 200 {
					t.Fatalf("image edit failed: %d %s", response.Code, response.Body.String())
				}
			} else {
				job := &videoJob{ID: "v2-video", Model: "grok-imagine-video", Prompt: "a paper plane"}
				s.videoJobs[job.ID] = job
				s.runVideoJob(job, job.Prompt, "9:16", "720p", 6, "custom", []provider.OpenAIImageInput{{Name: "reference.png", MIME: "image/png", Data: tinyPNG}})
				if job.Status != "completed" {
					t.Fatalf("video failed: %+v", job)
				}
			}
			if uploads != 1 || starts != 1 || len(cookies) != 5 {
				t.Fatalf("unexpected calls: uploads=%d starts=%d authenticated=%d", uploads, starts, len(cookies))
			}
			for _, cookie := range cookies {
				if cookie == "" || cookie != cookies[0] {
					t.Fatal("generation changed the upload account")
				}
			}
		})
	}
}
