package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestVideosRejectInvalidReferencesBeforeQueueing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  []byte
		count int
		key   string
	}{
		{"empty", nil, 1, "input_reference[]"},
		{"video disguised as image", videoTestMP4("video", "metadata"), 1, "input_reference[]"},
		{"unreadable image", []byte("not a PNG"), 1, "input_reference[]"},
		{"too many", tinyPNG, 8, "input_reference[]"},
		{"too large", make([]byte, maxVideoReferenceBytes+1), 1, "input_reference[]"},
		{"unsupported input field", tinyPNG, 1, "video_reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newVideoTestServer(adminTestConfig(t.TempDir()))
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			_ = writer.WriteField("prompt", "a paper plane")
			for i := 0; i < tc.count; i++ {
				part, err := writer.CreateFormFile(tc.key, "reference.png")
				if err != nil {
					t.Fatal(err)
				}
				_, _ = part.Write(tc.data)
			}
			_ = writer.Close()
			r := httptest.NewRequest(http.MethodPost, "/v1/videos", &body)
			r.Header.Set("Authorization", "Bearer api-secret")
			r.Header.Set("Content-Type", writer.FormDataContentType())
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 400 || len(s.videoJobs) != 0 {
				t.Fatalf("status=%d jobs=%d body=%s", w.Code, len(s.videoJobs), w.Body.String())
			}
		})
	}
}

func TestVideoReferenceFailureNeverFallsBackToText(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failAt    int
		response  string
		status    int
		challenge bool
	}{
		{"upload rejected", 1, `{"error":"SECRET"}`, 429, false},
		{"Cloudflare verification", 1, `<html>SECRET</html>`, 403, true},
		{"missing asset", 1, `{}`, 200, false},
		{"invalid asset URL", 1, `{"fileUri":" "}`, 200, false},
		{"second reference rejected", 2, `{"error":"SECRET"}`, 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uploads, post, chat := 0, 0, 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/upload":
					uploads++
					if uploads == tc.failAt {
						if tc.challenge {
							w.Header().Set("Cf-Mitigated", "challenge")
							w.Header().Set("Content-Type", "text/html")
						}
						w.WriteHeader(tc.status)
						fmt.Fprint(w, tc.response)
					} else {
						fmt.Fprint(w, `{"fileUri":"/reference.png"}`)
					}
				case "/post":
					post++
					fmt.Fprint(w, `{"post":{"id":"parent"}}`)
				case "/chat":
					chat++
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer up.Close()
			s := newVideoTestServer(adminTestConfig(t.TempDir()))
			s.mediaProvider = provider.NewMedia(up.Client(), up.URL+"/chat", up.URL+"/post", up.URL+"/upload", up.URL, s.cfg.RequestTimeout)
			if _, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"media_quota": testGrokMediaSnapshot(), "access_token": "test", "enabled": true, "status": "active"}}); err != nil {
				t.Fatal(err)
			}
			job := &videoJob{ID: "video_test", Model: "grok-imagine-video", Prompt: "paper plane"}
			s.videoJobs[job.ID] = job
			inputs := []provider.OpenAIImageInput{{Name: "a.png", MIME: "image/png", Data: tinyPNG}, {Name: "b.png", MIME: "image/png", Data: tinyPNG}}
			s.runVideoJob(job, job.Prompt, "9:16", "720p", 6, "custom", inputs)
			if job.Status != "failed" || uploads != tc.failAt || post != 0 || chat != 0 || job.ContentPath != "" {
				t.Fatalf("job=%+v uploads=%d post=%d chat=%d", job, uploads, post, chat)
			}
			if strings.Contains(stringValue(job.Error["message"]), "SECRET") {
				t.Fatal("upstream error body exposed")
			}
			if tc.challenge {
				if job.Error["code"] != "reference_verification_required" || job.Error["upstream_status"] != 403 || mapValue(job.Error["upload_diagnostics"])["upload_challenge"] != true {
					t.Fatalf("challenge evidence missing: %v", job.Error)
				}
				if !strings.Contains(stringValue(job.Error["message"]), "Cloudflare") || strings.Contains(stringValue(job.Error["message"]), "稍后重新提交") {
					t.Fatal("misleading challenge message")
				}
				rows, _, err := s.grokRuntimeStore.AccountSnapshot()
				if err != nil || len(rows) != 1 || rows[0]["status"] != "active" {
					t.Fatalf("challenge invalidated account: %v", err)
				}
			}
		})
	}
}

func TestVideoUploadsAndGenerationUseOneAccount(t *testing.T) {
	cookies := []string{}
	refs := []string{}
	uploads := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookies = append(cookies, r.Header.Get("Cookie"))
		switch r.URL.Path {
		case "/upload":
			uploads++
			fmt.Fprintf(w, `{"fileUri":"/reference%d.png"}`, uploads)
		case "/post":
			fmt.Fprint(w, `{"post":{"id":"parent"}}`)
		case "/chat":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			config := mapValue(mapValue(mapValue(mapValue(payload["responseMetadata"])["modelConfigOverride"])["modelMap"])["videoGenModelConfig"])
			for _, v := range anyList(config["imageReferences"]) {
				refs = append(refs, stringValue(v))
			}
			if payload["enableSideBySide"] != false {
				t.Error("multiple candidates enabled")
			}
			fmt.Fprintln(w, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"parentPostId":"parent","progress":100,"videoId":"video-result","videoUrl":"/result.mp4"}}}}`)
			fmt.Fprintln(w, "data: [DONE]")
		case "/result.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoTestMP4("output", "meta"))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer up.Close()
	s := newVideoTestServer(adminTestConfig(t.TempDir()))
	s.cfg.VideoDataDir = t.TempDir()
	s.mediaProvider = provider.NewMedia(up.Client(), up.URL+"/chat", up.URL+"/post", up.URL+"/upload", up.URL, s.cfg.RequestTimeout)
	for _, token := range []string{"account-a", "account-b"} {
		if _, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"media_quota": testGrokMediaSnapshot(), "access_token": token, "enabled": true, "status": "active"}}); err != nil {
			t.Fatal(err)
		}
	}
	job := &videoJob{ID: "video_test", Model: "grok-imagine-video", Prompt: "plane"}
	s.videoJobs[job.ID] = job
	inputs := []provider.OpenAIImageInput{{Name: "a.png", MIME: "image/png", Data: tinyPNG}, {Name: "b.png", MIME: "image/png", Data: tinyPNG}}
	s.runVideoJob(job, job.Prompt, "9:16", "720p", 6, "custom", inputs)
	if job.Status != "completed" || len(refs) != 2 || len(cookies) != 5 {
		t.Fatalf("job=%+v refs=%v calls=%d", job, refs, len(cookies))
	}
	for _, cookie := range cookies {
		if cookie == "" || cookie != cookies[0] {
			t.Fatal("uploads and generation used different accounts")
		}
	}
}

func TestDisabledGrokReviewDoesNotBlockHistoricalDuplicateMedia(t *testing.T) {
	prompts := []string{}
	post := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/post":
			post++
			fmt.Fprintf(w, `{"post":{"id":"parent-%d"}}`, post)
		case "/chat":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			prompts = append(prompts, stringValue(body["message"]))
			fmt.Fprintf(w, "data: {\"result\":{\"response\":{\"streamingVideoGenerationResponse\":{\"parentPostId\":\"parent-%d\",\"videoId\":\"video-%d\",\"videoUrl\":\"/result.mp4\",\"progress\":100}}}}\n", post, post)
			fmt.Fprintln(w, "data: [DONE]")
		case "/result.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoTestMP4("same cat video", fmt.Sprintf("signature %d", post)))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer up.Close()
	s := newVideoTestServer(adminTestConfig(t.TempDir()))
	s.cfg.VideoDataDir = t.TempDir()
	s.mediaProvider = provider.NewMedia(up.Client(), up.URL+"/chat", up.URL+"/post", up.URL+"/upload", up.URL, s.cfg.RequestTimeout)
	for _, token := range []string{"sequence-a", "sequence-b"} {
		if _, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"media_quota": testGrokMediaSnapshot(), "access_token": token, "enabled": true, "status": "active"}}); err != nil {
			t.Fatal(err)
		}
	}

	for i, prompt := range []string{"a paper plane", "a red boat"} {
		job := &videoJob{ID: fmt.Sprintf("video_%d", i), Model: "grok-imagine-video", Prompt: prompt}
		s.videoJobs[job.ID] = job
		s.runVideoJob(job, prompt, "9:16", "720p", 6, "custom", nil)
		if i == 0 && job.Status != "completed" {
			t.Fatalf("first failed: %+v", job)
		}
		if i == 1 && (job.Status != "completed" || job.ContentPath == "") {
			t.Fatalf("disabled review still rejected historical content: %+v", job)
		}
	}
	if len(prompts) != 2 || !strings.HasPrefix(prompts[0], "a paper plane") || !strings.HasPrefix(prompts[1], "a red boat") {
		t.Fatalf("prompts reused: %v", prompts)
	}
}

func TestVideoExtensionRequiresItsOwnCompletedResult(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("incomplete=%t", incomplete), func(t *testing.T) {
			chats, downloads := 0, 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/post":
					fmt.Fprint(w, `{"post":{"id":"parent"}}`)
				case "/chat":
					chats++
					var payload map[string]any
					_ = json.NewDecoder(r.Body).Decode(&payload)
					cfg := mapValue(mapValue(mapValue(mapValue(payload["responseMetadata"])["modelConfigOverride"])["modelMap"])["videoGenModelConfig"])
					if chats == 2 && cfg["extendPostId"] != "segment1" {
						t.Errorf("wrong extension source: %v", cfg)
					}
					if chats == 2 && incomplete {
						fmt.Fprintln(w, `data: {"result":{"response":{"token":"no video"}}}`)
					} else {
						fmt.Fprintf(w, "data: {\"result\":{\"response\":{\"streamingVideoGenerationResponse\":{\"parentPostId\":\"parent\",\"videoPostId\":\"segment%d\",\"videoUrl\":\"/segment%d.mp4\",\"progress\":100}}}}\n", chats, chats)
					}
					fmt.Fprintln(w, "data: [DONE]")
				case "/segment2.mp4":
					downloads++
					w.Header().Set("Content-Type", "video/mp4")
					_, _ = w.Write(videoTestMP4("full15seconds", "meta"))
				default:
					t.Errorf("unexpected download or request %s", r.URL.Path)
				}
			}))
			defer up.Close()
			s := newVideoTestServer(adminTestConfig(t.TempDir()))
			s.cfg.VideoDataDir = t.TempDir()
			s.mediaProvider = provider.NewMedia(up.Client(), up.URL+"/chat", up.URL+"/post", up.URL+"/upload", up.URL, s.cfg.RequestTimeout)
			if _, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"media_quota": testGrokMediaSnapshot(), "access_token": "test", "enabled": true, "status": "active"}}); err != nil {
				t.Fatal(err)
			}
			job := &videoJob{ID: "video_extension", Model: "grok-imagine-video", Prompt: "plane"}
			s.videoJobs[job.ID] = job
			s.runVideoJob(job, job.Prompt, "9:16", "720p", 15, "custom", nil)
			if chats != 2 {
				t.Fatalf("chats=%d job=%+v", chats, job)
			}
			if incomplete && (job.Status != "failed" || downloads != 0) {
				t.Fatalf("partial extension delivered: %+v", job)
			}
			if !incomplete && (job.Status != "completed" || downloads != 1) {
				t.Fatalf("valid extension failed: %+v", job)
			}
		})
	}
}
