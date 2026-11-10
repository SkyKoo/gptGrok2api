package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/config"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

type fixtureVideoVerifier struct {
	verdict string
	seen    func(string, []provider.OpenAIImageInput, []byte)
}

func (v fixtureVideoVerifier) Close() {}
func (v fixtureVideoVerifier) Verify(_ context.Context, prompt string, refs []provider.OpenAIImageInput, raw []byte, _ int) videoVerification {
	if v.seen != nil {
		v.seen(prompt, refs, raw)
	}
	if v.verdict == "invalid-review" {
		return parseVideoVerification("PRIVATE_REVIEW_RESPONSE", 6, len(refs))
	}
	reason := "consistent"
	if v.verdict == "mismatch" {
		reason = "unrelated_scene"
	} else if v.verdict != "matched" {
		reason = "review_unavailable"
	}
	return videoVerification{Verdict: v.verdict, Reason: reason, Model: "fixture", Frames: 6, References: len(refs), confirmed: v.verdict == "mismatch", expected: "人物", observed: "山湖", basis: "主要内容明显不相关"}
}

// Existing protocol tests isolate upstream translation from semantic review.
func newVideoTestServer(cfg config.Config) *Server {
	s := New(cfg)
	s.newVideoVerifier = func(context.Context, int) (videoResultVerifier, error) {
		return fixtureVideoVerifier{verdict: "matched"}, nil
	}
	return s
}

func TestVideoVerificationFormattingAndSemanticBoundaries(t *testing.T) {
	valid := `{"verdict":"matched","frames_reviewed":6,"references_reviewed":1,"reason":"consistent"}`
	mismatch := strings.ReplaceAll(strings.Replace(valid, `"matched"`, `"mismatch"`, 1), `"consistent"`, `"unrelated_scene"`)
	uncertain := strings.ReplaceAll(strings.Replace(valid, `"matched"`, `"uncertain"`, 1), `"consistent"`, `"insufficient_evidence"`)
	for _, tc := range []struct {
		name, raw, want, cause string
		refs                   int
	}{
		{"related", valid, "matched", "", 1},
		{"no references", strings.Replace(valid, `"references_reviewed":1`, `"references_reviewed":0`, 1), "matched", "", 0},
		{"unrelated scene", mismatch, "mismatch", "", 1},
		{"unrelated subject", strings.Replace(mismatch, "unrelated_scene", "unrelated_subject", 1), "mismatch", "", 1},
		{"placeholder", strings.Replace(mismatch, "unrelated_scene", "placeholder", 1), "mismatch", "", 1},
		{"insufficient evidence", uncertain, "uncertain", "", 1},
		{"json fence", "```json\n" + valid + "\n```", "matched", "", 1},
		{"plain fence", "```\n" + valid + "\n```", "matched", "", 1},
		{"fenced rejection", "```json\n" + mismatch + "\n```", "mismatch", "", 1},
		{"fenced CRLF", " \n```JSON\r\n" + valid + "\r\n```\n ", "matched", "", 1},
		{"contradictory", strings.Replace(valid, "consistent", "unrelated_scene", 1), "uncertain", "inconsistent_verdict", 1},
		{"unsupported detail rejection", strings.Replace(mismatch, "unrelated_scene", "action_mismatch", 1), "uncertain", "invalid_reason", 1},
		{"misreported refs", valid, "matched", "", 2},
		{"missing frames", strings.Replace(valid, `"frames_reviewed":6`, `"frames_reviewed":1`, 1), "matched", "", 1},
		{"null count", strings.Replace(valid, `"references_reviewed":1`, `"references_reviewed":null`, 1), "matched", "", 0},
		{"string count", strings.Replace(valid, `"frames_reviewed":6`, `"frames_reviewed":"6"`, 1), "matched", "", 1},
		{"missing field", strings.Replace(valid, `,"references_reviewed":1`, "", 1), "matched", "", 1},
		{"extra field", strings.Replace(valid, `{"verdict"`, `{"description":"PRIVATE","verdict"`, 1), "matched", "", 1},
		{"empty", "", "uncertain", "invalid_json_object", 1},
		{"refusal", "Cannot assess this video", "uncertain", "invalid_json_object", 1},
		{"extra data", valid + `{}`, "uncertain", "missing_verdict", 1},
		{"duplicate key", strings.Replace(valid, `"verdict":"matched"`, `"verdict":"uncertain","verdict":"matched"`, 1), "uncertain", "conflicting_duplicate_field", 1},
		{"fence with commentary", "Here is the result: \n```json\n" + valid + "\n```", "matched", "", 1},
		{"multiple fenced objects", "```json\n" + valid + "\n" + mismatch + "\n```", "uncertain", "conflicting_review_objects", 1},
		{"incomplete fence", "```json\n" + valid, "matched", "", 1},
		{"invalid verdict", strings.Replace(valid, "matched", "approved", 1), "uncertain", "invalid_verdict", 1},
		{"arbitrary reason", strings.Replace(valid, "consistent", "PRIVATE", 1), "matched", "", 1},
		{"oversized reply", strings.Repeat(" ", maxGrokReviewResponseBytes) + valid, "uncertain", "response_too_long", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseVideoVerification(tc.raw, 6, tc.refs)
			if got.Verdict != tc.want || got.parseError != tc.cause || strings.Contains(got.Reason, "PRIVATE") {
				t.Fatalf("got=%+v", got)
			}
			if tc.cause != "" && got.Reason != "invalid_review_response" {
				t.Fatalf("protocol failure became a content decision: %+v", got)
			}
		})
	}
}

func TestRejectedVideoFingerprintSurvivesRestartAndBlocksSameInput(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	s := New(cfg)
	raw := videoTestMP4("unrelated scene", "first signature")
	hash, err := s.checkVideoResult("request", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.rejectVideoResult("video_bad", "request", hash, videoVerification{Verdict: "mismatch", Reason: "unrelated_scene"}); err != nil {
		t.Fatal(err)
	}
	restarted := New(cfg)
	for _, request := range []string{"request", "different-request"} {
		if _, err := restarted.checkVideoResult(request, videoTestMP4("unrelated scene", "changed signature")); err != errRejectedVideo {
			t.Fatalf("bad media accepted: %v", err)
		}
		if _, _, err := restarted.storeVideoResult("video_later", request, raw, "video/mp4"); err != errRejectedVideo {
			t.Fatalf("bad media stored: %v", err)
		}
	}
}

func TestVideoDeliveryRequiresReviewAndPersistsTerminalEvidence(t *testing.T) {
	for _, verdict := range []string{"matched", "mismatch", "uncertain", "invalid-review", "disabled-default", "disabled-explicit"} {
		t.Run(verdict, func(t *testing.T) {
			reviewEnabled := !strings.HasPrefix(verdict, "disabled-")
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/post":
					fmt.Fprint(w, `{"post":{"id":"parent-private"}}`)
				case "/chat":
					calls++
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintln(w, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"parentPostId":"parent-private","videoPostId":"result-private","videoUrl":"/result.mp4?signature=SECRET"}}}}`)
					fmt.Fprintln(w, "data: [DONE]")
				case "/result.mp4":
					w.Header().Set("Content-Type", "video/mp4")
					fmt.Fprint(w, "fixture-mp4")
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			cfg := adminTestConfig(t.TempDir())
			s := New(cfg)
			if verdict != "disabled-default" {
				if _, err := s.store.UpdateConfig("video_review_enabled", reviewEnabled); err != nil {
					t.Fatal(err)
				}
			}
			s.mediaProvider = provider.NewMedia(upstream.Client(), upstream.URL+"/chat", upstream.URL+"/post", upstream.URL, upstream.URL, cfg.RequestTimeout)
			_, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"media_quota": testGrokMediaSnapshot(), "access_token": "SECRET", "enabled": true, "status": "active"}, {"media_quota": testGrokMediaSnapshot(), "access_token": "SECOND", "enabled": true, "status": "active"}})
			if err != nil {
				t.Fatal(err)
			}
			reviewed := false
			s.newVideoVerifier = func(context.Context, int) (videoResultVerifier, error) {
				if !reviewEnabled {
					t.Fatal("disabled review reserved a reviewer")
				}
				return fixtureVideoVerifier{verdict: verdict, seen: func(prompt string, refs []provider.OpenAIImageInput, raw []byte) {
					reviewed = true
					if prompt != "private prompt" || len(refs) != 0 || string(raw) != "fixture-mp4" {
						t.Fatal("review inputs not forwarded")
					}
					// An in-flight review keeps the decision captured at job start.
					if _, err := s.store.UpdateConfig("video_review_enabled", false); err != nil {
						t.Fatal(err)
					}
				}}, nil
			}
			job := &videoJob{ID: "video_fixture", Model: "grok-imagine-video", Prompt: "private prompt", Status: "queued"}
			s.videoJobs[job.ID] = job
			s.runVideoJob(job, job.Prompt, "9:16", "720p", 6, "custom", nil)
			wantCalls := 1
			if calls != wantCalls || reviewed != reviewEnabled {
				t.Fatalf("calls=%d reviewed=%v", calls, reviewed)
			}
			want := "failed"
			if verdict == "matched" || !reviewEnabled {
				want = "completed"
			}
			if job.Status != want {
				t.Fatalf("job=%+v", job)
			}
			if want == "failed" {
				if job.ContentPath != "" || job.VideoURL != "" || len(listMediaItems(cfg.VideoDataDir, "video", "video/")) != 0 {
					t.Fatal("rejected media was published")
				}
				w := httptest.NewRecorder()
				s.videoContent(w, httptest.NewRequest("GET", "/", nil), job.ID)
				if w.Code != http.StatusConflict {
					t.Fatalf("download status=%d", w.Code)
				}
			}
			if !reviewEnabled && (job.Verification == nil || job.Verification.Verdict != "skipped" || job.Verification.Frames != 0 || job.Verification.Model != "") {
				t.Fatal("disabled review was not marked as skipped")
			}
			logs := s.loadCallLogs()
			if len(logs) != 1 {
				t.Fatalf("logs=%d", len(logs))
			}
			raw, _ := json.Marshal(logs)
			if verdict == "invalid-review" && !strings.Contains(string(raw), `"video_review_parse_error":"invalid_json_object"`) {
				t.Fatal("review parse failure classification not persisted")
			}
			for _, secret := range []string{"SECRET", "private prompt", "parent-private", "result-private", "signature=", "PRIVATE_REVIEW_RESPONSE"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("log leaked %s", secret)
				}
			}
			detail := mapValue(logs[0]["detail"])
			if detail["endpoint"] != "/v1/videos" || mapValue(mapValue(detail["monitor"])["metrics"])["video_stream"] == nil {
				t.Fatal("missing terminal diagnostics")
			}
			if want == "failed" && detail["output_videos"] != nil {
				t.Fatal("failed log has deliverable")
			}
			s.setVideoStatus(job, "completed", 100, nil)
			if job.Status != want || len(s.loadCallLogs()) != 1 {
				t.Fatal("terminal status changed or duplicate log")
			}
			restarted := New(cfg)
			if restarted.videoLoadErr != nil || restarted.videoJobs[job.ID] == nil || restarted.videoJobs[job.ID].Status != want || len(restarted.loadCallLogs()) != 1 {
				t.Fatal("restart lost result/log")
			}
			if want == "completed" {
				w := httptest.NewRecorder()
				restarted.videoContent(w, httptest.NewRequest("GET", "/", nil), job.ID)
				if w.Code != 200 || w.Body.String() != "fixture-mp4" {
					t.Fatal("restart download failed")
				}
			}
			response := adminRequest(restarted.Handler(), "POST", "/api/logs/delete", strings.NewReader(`{"ids":["video_fixture"]}`))
			if response.Code != 200 || len(restarted.loadCallLogs()) != 0 {
				t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
			}
			if len(New(cfg).loadCallLogs()) != 0 {
				t.Fatal("deleted log resurrected")
			}
		})
	}
}

func TestVideoWithoutGenerationAccountDoesNotReserveReviewer(t *testing.T) {
	s := New(adminTestConfig(t.TempDir()))
	if _, err := s.store.UpdateConfig("video_review_enabled", true); err != nil {
		t.Fatal(err)
	}
	job := &videoJob{ID: "video_no_reviewer", Model: "grok-imagine-video"}
	s.videoJobs[job.ID] = job
	s.runVideoJob(job, "private prompt", "9:16", "720p", 6, "custom", nil)
	if job.Status != "failed" || job.Error["code"] != "no_available_account" {
		t.Fatalf("job=%+v", job)
	}
	if len(s.loadCallLogs()) != 1 {
		t.Fatal("missing reviewer failure log")
	}
}

func TestVideoReviewSettingDefaultsOffAndPersistsThroughSettingsAPI(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	s := New(cfg)
	read := func(server *Server) bool {
		t.Helper()
		response := adminRequest(server.Handler(), http.MethodGet, "/api/settings", nil)
		var body map[string]any
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil {
			t.Fatalf("settings response: %s", response.Body.String())
		}
		enabled, ok := mapValue(body["config"])["video_review_enabled"].(bool)
		if !ok {
			t.Fatal("settings omitted the boolean default")
		}
		return enabled
	}
	if read(s) {
		t.Fatal("video review enabled without opt-in")
	}
	for _, value := range []string{`"true"`, `1`, `null`, `{}`} {
		response := adminRequest(s.Handler(), http.MethodPost, "/api/settings", strings.NewReader(`{"video_review_enabled":`+value+`,"invalid_patch_marker":true}`))
		settings, err := s.store.Config()
		if response.Code != http.StatusBadRequest || err != nil || settings["invalid_patch_marker"] != nil || read(s) {
			t.Fatal("invalid setting applied a partial patch")
		}
	}
	reviewerCalls := 0
	s.newVideoVerifier = func(context.Context, int) (videoResultVerifier, error) {
		reviewerCalls++
		return nil, fmt.Errorf("review unavailable in fixture")
	}
	for i, enabled := range []bool{true, false} {
		response := adminRequest(s.Handler(), http.MethodPost, "/api/settings", strings.NewReader(fmt.Sprintf(`{"video_review_enabled":%t}`, enabled)))
		if response.Code != http.StatusOK || read(s) != enabled || read(New(cfg)) != enabled {
			t.Fatal("saved setting lost or requires restart")
		}
		job := &videoJob{ID: fmt.Sprintf("video_setting_%d", i), Model: "grok-imagine-video", Status: "queued"}
		s.runVideoJob(job, "fixture", "9:16", "720p", 6, "normal", nil)
		if enabled && job.Error["code"] != "no_available_account" {
			t.Fatal("generation quota must be checked before lazy reviewer reservation")
		}
		if !enabled && job.Error["code"] != "no_available_account" {
			t.Fatal("disabled review still required a reviewer")
		}
	}
	if reviewerCalls != 0 {
		t.Fatal("disabled review consumed reviewer capacity")
	}
}

func TestChatVideoReviewerUploadsEveryReferenceAndDecodedFrame(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "sample.mp4")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=8", "-t", "6", "-c:v", "mpeg4", "-threads", "1", file)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s, uploads := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		messages := request["messages"].([]any)
		if len(messages) != 2 || mapValue(mapValue(messages[0])["author"])["role"] != "system" {
			t.Error("review instructions not isolated")
		}
		systemParts := mapValue(mapValue(messages[0])["content"])["parts"].([]any)
		instructions := stringValue(systemParts[0])
		example := instructions[strings.LastIndex(instructions, "\n")+1:]
		if got := parseVideoVerification(example, 6, 2); got.Verdict != "matched" {
			t.Errorf("review response example does not use the real image counts: %+v", got)
		}
		parts := mapValue(mapValue(messages[1])["content"])["parts"].([]any)
		attachments := 0
		for _, part := range parts {
			if mapValue(part)["asset_pointer"] != nil {
				attachments++
			}
		}
		if attachments != 8 || !strings.Contains(stringValue(parts[0]), "animate a paper plane") {
			t.Errorf("attachments=%d request=%v", attachments, parts[0])
		}
		verdict := `{"verdict":"mismatch","frames_reviewed":6,"references_reviewed":2,"reason":"unrelated_scene","expected":"纸飞机","observed":"人物","basis":"主要内容明显不同"}`
		event, _ := json.Marshal(map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}, "content": map[string]any{"parts": []string{verdict}}}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\ndata: [DONE]\n", event)
	})
	verifier, err := s.prepareVideoVerifier(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	refs := []provider.OpenAIImageInput{{Name: "a.png", MIME: "image/png", Data: tinyPNG}, {Name: "b.png", MIME: "image/png", Data: tinyPNG}}
	verdict := verifier.Verify(ctx, "animate a paper plane", refs, raw, 6)
	verifier.Close()
	if verdict.Verdict != "mismatch" || !verdict.confirmed || calls != 2 || uploads.Load() != 16 || len(s.videoReviewSlots) != 0 {
		t.Fatalf("result=%+v calls=%d uploads=%d", verdict, calls, uploads.Load())
	}
}

func TestVideoJournalRecoversInterruptionWithoutResubmission(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	s := New(cfg)
	job := &videoJob{ID: "video_interrupted_fixture", Model: "grok-imagine-video", Status: "in_progress", Prompt: "private prompt"}
	s.videoJobs[job.ID] = job
	s.monitor.start(job.ID, "/v1/videos", job.Model, "Grok 视频生成")
	if !s.videoStage(job, "video_generation", map[string]any{"video_reference_count": 1}) {
		t.Fatal("checkpoint failed")
	}
	standby := NewWithWorkers(cfg, false)
	if standby.videoJobs[job.ID].Status != "in_progress" || len(standby.loadCallLogs()) != 0 {
		t.Fatal("standby mutated active task")
	}
	restarted := New(cfg)
	if restarted.videoJobs[job.ID].Error["code"] != "video_interrupted" || len(restarted.loadCallLogs()) != 1 {
		t.Fatal("interruption missing")
	}
	if len(New(cfg).loadCallLogs()) != 1 {
		t.Fatal("duplicate recovery log")
	}
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "video_tasks", videoJournalName(job.ID)))
	if err != nil || strings.Contains(string(raw), "private prompt") {
		t.Fatal("checkpoint contains prompt")
	}
}

func TestVideoCompletionStorageFailureCannotPublishSuccess(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	s := New(cfg)
	path, _, err := s.storeVideoResult("video_storage_fixture", "request", videoTestMP4("checked content", "meta"), "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	job := &videoJob{ID: "video_storage_fixture", Model: "grok-imagine-video", Status: "in_progress", ContentPath: path}
	s.videoJobs[job.ID] = job
	s.monitor.start(job.ID, "/v1/videos", job.Model, "fixture")
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "video_tasks"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.setVideoStatus(job, "completed", 100, nil)
	if job.Status != "failed" || job.ContentPath != "" || job.Error["code"] != "video_storage_failed" {
		t.Fatalf("job=%+v", job)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("uncommitted media published")
	}
	logs := s.loadCallLogs()
	if len(logs) != 1 || mapValue(logs[0]["detail"])["status"] != "failed" {
		t.Fatal("failure log missing")
	}
}

func TestLegacyVideoDownloadsSurviveUpgradeWithoutRewritingData(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	s := New(cfg)
	id := "video_1793801222012984956"
	raw := videoTestMP4("legacy video", "legacy signature")
	path, hash, err := s.storeVideoResult(id, "legacy-request", raw, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]any{"call_id": id, "request_sha256": "legacy-request", "media_sha256": hash})
	if err := os.WriteFile(path+".meta.json", metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := New(cfg)
	job := restarted.videoJobs[id]
	if job == nil || job.Status != "completed" || job.Prompt != "" || job.Verification != nil {
		t.Fatal("legacy download not restored faithfully")
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/v1/videos/"+id+"/content", nil)
		request.Header.Set("Authorization", "Bearer api-secret")
		restarted.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || (method == http.MethodGet && response.Body.String() != string(raw)) {
			t.Fatalf("legacy %s failed: %d", method, response.Code)
		}
	}
	if len(restarted.loadCallLogs()) != 0 {
		t.Fatal("upgrade invented historical call logs")
	}
	for _, folder := range []string{"video_tasks", "video_logs"} {
		if _, err := os.Stat(filepath.Join(cfg.DataDir, folder)); !os.IsNotExist(err) {
			t.Fatal("upgrade rewrote historical business data")
		}
	}
	if after, _ := os.ReadFile(path + ".meta.json"); string(after) != string(metadata) {
		t.Fatal("upgrade rewrote media metadata")
	}
	if err := os.WriteFile(path, videoTestMP4("tampered content", "legacy signature"), 0o600); err != nil {
		t.Fatal(err)
	}
	if New(cfg).videoJobs[id] != nil {
		t.Fatal("legacy digest mismatch accepted")
	}
}
