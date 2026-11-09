package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

type imageReviewFunc func(context.Context, string, []provider.OpenAIImageInput, provider.OpenAIImageInput) videoVerification

func (f imageReviewFunc) VerifyImage(c context.Context, p string, r []provider.OpenAIImageInput, o provider.OpenAIImageInput) videoVerification {
	return f(c, p, r, o)
}
func (f imageReviewFunc) Close() {}
func reviewFixture(verdict string, raw []byte) videoVerification {
	reason := "consistent"
	if verdict == "mismatch" {
		reason = "unrelated_subject"
	}
	if verdict == "uncertain" {
		reason = "invalid_review_response"
	}
	return videoVerification{Verdict: verdict, Reason: reason, Frames: 1, Model: "fixture", confirmed: verdict == "mismatch", expected: "人像", observed: "菜市场", basis: "主体和场景不同", samples: []provider.OpenAIImageInput{{MIME: "image/png", Data: raw}}, checks: []grokReviewResponse{{Verdict: verdict, Reason: reason, Response: `{"verdict":"` + verdict + `"}`}}}
}
func coloredPNG(t *testing.T, c color.Color, level png.CompressionLevel) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, c)
		}
	}
	if err := (&png.Encoder{CompressionLevel: level}).Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func enableGrokReview(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.store.UpdateConfig("video_review_enabled", true); err != nil {
		t.Fatal(err)
	}
}
func grokTestAccounts(t *testing.T, s *Server) {
	t.Helper()
	for _, token := range []string{"grok-one", "grok-two", "grok-three"} {
		if _, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"access_token": token, "enabled": true, "status": "active", "media_quota": testGrokMediaSnapshot()}}); err != nil {
			t.Fatal(err)
		}
	}
}
func TestGrokImageReviewRetryBatchAndFailure(t *testing.T) {
	good := coloredPNG(t, color.White, png.DefaultCompression)
	bad := coloredPNG(t, color.Black, png.DefaultCompression)
	for _, scenario := range []string{"mixed-retry-success", "retry-exhausted", "uncertain", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			s := New(adminTestConfig(t.TempDir()))
			grokTestAccounts(t, s)
			if scenario != "disabled" {
				enableGrokReview(t, s)
			}
			callID := "image-review-fixture"
			s.monitor.start(callID, "/v1/images/edits", "grok-imagine-image-pro", "fixture")
			req := httptest.NewRequest("POST", "/v1/images/edits", nil).WithContext(context.WithValue(context.Background(), monitorCallIDKey{}, callID))
			reviews := 0
			s.newImageVerifier = func(context.Context, int) (imageResultVerifier, error) {
				return imageReviewFunc(func(_ context.Context, _ string, _ []provider.OpenAIImageInput, o provider.OpenAIImageInput) videoVerification {
					reviews++
					if scenario == "uncertain" {
						return reviewFixture("uncertain", o.Data)
					}
					if bytes.Equal(o.Data, bad) {
						return reviewFixture("mismatch", o.Data)
					}
					return reviewFixture("matched", o.Data)
				}), nil
			}
			counts := []int{}
			result, err := s.generateReviewedGrokImages(req, "reference portrait", "grok-imagine-image-pro", "1024x1024", "url", 2, nil, func(_ context.Context, lease *accounts.Lease, n int) ([]provider.ImageResult, error) {
				counts = append(counts, n)
				s.grokAccountPool.MarkSent(lease, "image_gen")
				outputs := []provider.ImageResult{}
				for i := 0; i < n; i++ {
					raw := good
					if len(counts) == 1 && i == 1 || scenario == "retry-exhausted" {
						raw = bad
					}
					outputs = append(outputs, provider.ImageResult{Base64: base64.StdEncoding.EncodeToString(raw), MIME: "image/png"})
				}
				return outputs, nil
			})
			record, _ := s.monitor.detail(callID)
			if scenario == "mixed-retry-success" {
				if err != nil || len(result) != 2 || fmt.Sprint(counts) != "[2 1]" || reviews != 2 {
					t.Fatalf("err=%v result=%d counts=%v reviews=%d", err, len(result), counts, reviews)
				}
				// Retried identical good content reuses the same-intent positive hash cache.
				if len(record.GrokMediaReviews) != 3 || !record.GrokMediaReviews[2].CacheHit {
					t.Fatalf("evidence=%+v", record.GrokMediaReviews)
				}
			} else if scenario == "disabled" {
				if err != nil || len(result) != 2 || reviews != 0 || len(record.GrokMediaReviews) != 0 || len(counts) != 1 {
					t.Fatalf("disabled: %v %d %d %v", err, len(result), reviews, counts)
				}
			} else {
				if err == nil || len(result) != 0 || len(listMediaItems(s.cfg.ImageDataDir, "image", "image/")) != 0 {
					t.Fatal("failed output published")
				}
				want := 1
				if scenario == "retry-exhausted" {
					want = 2
				}
				if len(counts) != want {
					t.Fatalf("generation count=%v", counts)
				}
				if len(record.GrokMediaReviews) == 0 || len(record.GrokMediaReviews[0].Images) == 0 {
					t.Fatal("missing failed output evidence")
				}
			}
			for _, review := range record.GrokMediaReviews {
				if len(review.MediaHash) != 64 || len(review.FileHash) != 64 || len(review.RequestHash) != 64 {
					t.Fatal("missing hashes")
				}
				for _, preview := range review.Images {
					anon := httptest.NewRecorder()
					s.Handler().ServeHTTP(anon, httptest.NewRequest("GET", preview.URL, nil))
					if anon.Code == 200 {
						t.Fatal("evidence public")
					}
					authed := adminRequest(s.Handler(), "GET", preview.URL, nil)
					if authed.Code != 200 || authed.Header().Get("Cache-Control") != "private, no-store" {
						t.Fatal("private evidence unavailable")
					}
				}
			}
		})
	}
}

func TestGrokReviewHashCacheSurvivesRestartAndIsScopedToIntent(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	s := New(cfg)
	raw := coloredPNG(t, color.Black, png.DefaultCompression)
	hash, _ := grokImageFingerprint(raw)
	s.monitor.start("first", "/v1/images/generations", "grok-imagine-image", "fixture")
	s.newImageVerifier = func(context.Context, int) (imageResultVerifier, error) {
		return imageReviewFunc(func(context.Context, string, []provider.OpenAIImageInput, provider.OpenAIImageInput) videoVerification {
			return reviewFixture("mismatch", raw)
		}), nil
	}
	if got := s.reviewGrokMedia(context.Background(), "first", "image", "intent-a", hash, "person", nil, raw, 0, 1, 1); got.Verdict != "mismatch" {
		t.Fatal(got)
	}
	restarted := New(cfg)
	calls := 0
	restarted.newImageVerifier = func(context.Context, int) (imageResultVerifier, error) {
		calls++
		return imageReviewFunc(func(context.Context, string, []provider.OpenAIImageInput, provider.OpenAIImageInput) videoVerification {
			return reviewFixture("matched", raw)
		}), nil
	}
	restarted.monitor.start("second", "/v1/images/generations", "grok-imagine-image", "fixture")
	got := restarted.reviewGrokMedia(context.Background(), "second", "image", "intent-a", hash, "person", nil, raw, 0, 2, 1)
	if got.Verdict != "mismatch" || calls != 0 {
		t.Fatalf("cache missed: %+v %d", got, calls)
	}
	got = restarted.reviewGrokMedia(context.Background(), "second", "image", "intent-b", hash, "vegetable market", nil, raw, 0, 1, 1)
	if got.Verdict != "matched" || calls != 1 {
		t.Fatal("different intent was falsely rejected")
	}
	for _, verdict := range []string{"uncertain", "mismatch"} {
		restarted.newImageVerifier = func(context.Context, int) (imageResultVerifier, error) {
			return imageReviewFunc(func(context.Context, string, []provider.OpenAIImageInput, provider.OpenAIImageInput) videoVerification {
				v := reviewFixture(verdict, raw)
				v.confirmed = false
				return v
			}), nil
		}
		got = restarted.reviewGrokMedia(context.Background(), "second", "image", "unconfirmed-"+verdict, hash, "person", nil, raw, 0, 1, 1)
		if got.Verdict != "uncertain" {
			t.Fatal("unconfirmed mismatch became rejection")
		}
		if _, hit := restarted.cachedGrokReview("image", hash, "unconfirmed-"+verdict); hit {
			t.Fatal("technical failure poisoned cache")
		}
	}
}
func TestGrokImageHashIgnoresEncodingButNotContent(t *testing.T) {
	a := coloredPNG(t, color.White, png.NoCompression)
	b := coloredPNG(t, color.White, png.BestCompression)
	c := coloredPNG(t, color.Black, png.DefaultCompression)
	ha, err := grokImageFingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := grokImageFingerprint(b)
	hc, _ := grokImageFingerprint(c)
	if bytes.Equal(a, b) || ha != hb || ha == hc {
		t.Fatal("bad pixel fingerprint")
	}
	if _, err := grokImageFingerprint([]byte("not an image")); err == nil {
		t.Fatal("invalid image accepted")
	}
}

func TestGrokReviewRecheckRecoversFormatAndDisagreesSafely(t *testing.T) {
	matched := `{"verdict":"matched","frames_reviewed":1,"references_reviewed":0,"reason":"consistent","expected":"person","observed":"person","basis":"broadly related"}`
	mismatch := `{"verdict":"mismatch","frames_reviewed":1,"references_reviewed":0,"reason":"unrelated_subject","expected":"person","observed":"market","basis":"different subject"}`
	for _, tc := range []struct {
		name      string
		replies   []string
		want      string
		confirmed bool
	}{
		{"format-recovered", []string{"Here is the result (incomplete)", matched}, "matched", false},
		{"conflicting-conclusions-recovered", []string{matched + mismatch, matched}, "matched", false},
		{"truncated-decision-recovered", []string{`{"data":{"verdict":"matched"}`, matched}, "matched", false},
		{"false-rejection-recovered", []string{mismatch, matched}, "matched", false},
		{"confirmed", []string{mismatch, mismatch}, "mismatch", true},
		{"recheck-failed", []string{mismatch, "Cannot assess this image"}, "uncertain", false},
		{"only-one-negative", []string{"malformed", mismatch}, "uncertain", false},
		{"both-invalid", []string{"malformed", "also malformed"}, "uncertain", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, uploads := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if calls >= len(tc.replies) {
					t.Error("more than one recheck")
					http.Error(w, "excess", 500)
					return
				}
				event, _ := json.Marshal(map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}, "content": map[string]any{"parts": []string{tc.replies[calls]}}}})
				calls++
				fmt.Fprintf(w, "data: %s\ndata: [DONE]\n", event)
			})
			verifier, err := s.prepareImageVerifier(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			got := verifier.VerifyImage(context.Background(), "portrait", nil, provider.OpenAIImageInput{Name: "output.png", MIME: "image/png", Data: tinyPNG})
			verifier.Close()
			if got.Verdict != tc.want || got.confirmed != tc.confirmed || calls != 2 || uploads.Load() != 2 || len(got.checks) != 2 || len(got.samples) != 1 || len(s.videoReviewSlots) != 0 {
				t.Fatalf("got=%+v calls=%d uploads=%d", got, calls, uploads.Load())
			}
			if got.checks[0].Response != tc.replies[0] {
				t.Fatal("lost diagnostic reply")
			}
		})
	}
}

func TestGrokReviewEvidenceSurvivesVideoJournalAndStaysPrivate(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	s := New(cfg)
	id := "video_evidence"
	job := &videoJob{ID: id, Model: "grok-imagine-video", Status: "in_progress"}
	s.videoJobs[id] = job
	s.monitor.start(id, "/v1/videos", job.Model, "fixture")
	v := reviewFixture("mismatch", tinyPNG)
	e := grokMediaReview{Kind: "video", Attempt: 1, Verdict: v.Verdict, Checks: v.checks, Images: s.storeLogImages(id, v.samples)}
	s.recordGrokReview(id, e)
	job.Verification = &v
	if !s.videoStage(job, "video_content_reviewed", nil) {
		t.Fatal("checkpoint")
	}
	s.setVideoStatus(job, "failed", 0, map[string]any{"code": "video_content_mismatch", "message": "fixture"})
	logs := New(cfg).loadCallLogs()
	if len(logs) != 1 || len(anyList(mapValue(logs[0]["detail"])["grok_media_reviews"])) != 1 {
		t.Fatal("evidence lost at restart")
	}
	public, _ := json.Marshal(job)
	if strings.Contains(string(public), "checks") || strings.Contains(string(public), "input-images") {
		t.Fatal("public API exposed review evidence")
	}
	for _, preview := range e.Images {
		if _, err := os.Stat(filepath.Join(s.logInputImagesDir(), filepath.Base(preview.URL))); err != nil {
			t.Fatal(err)
		}
	}
}

type videoReviewFunc func(context.Context, string, []provider.OpenAIImageInput, []byte, int) videoVerification

func (f videoReviewFunc) Verify(c context.Context, p string, refs []provider.OpenAIImageInput, raw []byte, seconds int) videoVerification {
	return f(c, p, refs, raw, seconds)
}
func (f videoReviewFunc) Close() {}

func TestGrokVideoRegenerationRetainsOneTaskAndBothAttempts(t *testing.T) {
	for _, scenario := range []string{"retry-success", "same-placeholder", "review-unavailable", "disabled-known-hash"} {
		t.Run(scenario, func(t *testing.T) {
			chats := 0
			bad := videoTestMP4("unrelated market", "metadata")
			good := videoTestMP4("related portrait", "metadata")
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/post":
					fmt.Fprint(w, `{"post":{"id":"parent"}}`)
				case "/chat":
					chats++
					fmt.Fprintln(w, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"parentPostId":"parent","videoId":"video-result","videoUrl":"/video.mp4","progress":100}}}}`)
					fmt.Fprintln(w, "data: [DONE]")
				case "/video.mp4":
					w.Header().Set("Content-Type", "video/mp4")
					if scenario == "retry-success" && chats == 2 {
						w.Write(good)
					} else {
						w.Write(bad)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer up.Close()
			cfg := adminTestConfig(t.TempDir())
			s := New(cfg)
			grokTestAccounts(t, s)
			s.mediaProvider = provider.NewMedia(up.Client(), up.URL+"/chat", up.URL+"/post", up.URL+"/upload", up.URL, cfg.RequestTimeout)
			if scenario != "disabled-known-hash" {
				enableGrokReview(t, s)
			}
			hash, _ := s.checkVideoResult("request", bad)
			// Even legacy global rejection records must be controlled by the switch.
			if err := s.rejectVideoResult("video_old", "request", hash, videoVerification{Verdict: "mismatch", Reason: "placeholder"}); err != nil {
				t.Fatal(err)
			}
			reviews := 0
			s.newVideoVerifier = func(context.Context, int) (videoResultVerifier, error) {
				reviews++
				if scenario == "disabled-known-hash" {
					t.Fatal("disabled review reserved an account")
				}
				if scenario == "review-unavailable" {
					return nil, fmt.Errorf("no account")
				}
				return videoReviewFunc(func(_ context.Context, _ string, _ []provider.OpenAIImageInput, raw []byte, _ int) videoVerification {
					verdict := "mismatch"
					if bytes.Equal(raw, good) {
						verdict = "matched"
					}
					v := reviewFixture(verdict, tinyPNG)
					v.Frames = 6
					return v
				}), nil
			}
			job := &videoJob{ID: "video_retry", Model: "grok-imagine-video", Status: "queued"}
			s.videoJobs[job.ID] = job
			s.runVideoJob(job, "portrait", "9:16", "720p", 6, "custom", nil)
			wantStatus, wantCalls := "failed", 2
			if scenario == "retry-success" {
				wantStatus = "completed"
			}
			if scenario == "disabled-known-hash" {
				wantStatus, wantCalls = "completed", 1
			}
			if scenario == "review-unavailable" {
				wantCalls = 1
			}
			if job.Status != wantStatus || chats != wantCalls {
				t.Fatalf("job=%+v calls=%d", job, chats)
			}
			logs := New(cfg).loadCallLogs()
			if len(logs) != 1 {
				t.Fatalf("duplicate terminal logs: %d", len(logs))
			}
			entries := anyList(mapValue(logs[0]["detail"])["grok_media_reviews"])
			if scenario == "same-placeholder" && (reviews != 1 || len(entries) != 2 || mapValue(entries[1])["cache_hit"] != true) {
				t.Fatalf("hash did not prevent repeated review: reviews=%d entries=%v", reviews, entries)
			}
			if scenario == "retry-success" {
				if reviews != 2 || len(entries) != 2 || job.Error != nil || job.Verification.Verdict != "matched" {
					t.Fatal("retry leaked first failure")
				}
				if mapValue(entries[0])["verdict"] != "mismatch" || mapValue(entries[1])["verdict"] != "matched" {
					t.Fatal("attempt history overwritten")
				}
			}
			if scenario == "review-unavailable" && (job.Error["code"] != "video_content_unverified" || len(entries) != 1) {
				t.Fatal("review outage misclassified as content mismatch")
			}
		})
	}
}

func TestGrokAsyncImageRetryTerminalAndEvidence(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			good := coloredPNG(t, color.White, png.DefaultCompression)
			bad := coloredPNG(t, color.Black, png.DefaultCompression)
			generations := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/upload":
					fmt.Fprintf(w, `{"fileMetadataId":"01234567-89ab-cdef-0123-456789abcdef","fileUri":"http://%s/ref.png"}`, r.Host)
				case "/post":
					fmt.Fprint(w, `{"post":{"id":"image-parent"}}`)
				case "/chat":
					generations++
					fmt.Fprintln(w, `data: {"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"/result.png","progress":100}}}}`)
					fmt.Fprintln(w, "data: [DONE]")
				case "/result.png":
					w.Header().Set("Content-Type", "image/png")
					if success && generations == 2 {
						w.Write(good)
					} else {
						w.Write(bad)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer up.Close()
			s := New(adminTestConfig(t.TempDir()))
			enableGrokReview(t, s)
			grokTestAccounts(t, s)
			s.mediaProvider = provider.NewMedia(up.Client(), up.URL+"/chat", up.URL+"/post", up.URL+"/upload", up.URL, s.cfg.RequestTimeout)
			s.newImageVerifier = func(context.Context, int) (imageResultVerifier, error) {
				return imageReviewFunc(func(_ context.Context, _ string, refs []provider.OpenAIImageInput, o provider.OpenAIImageInput) videoVerification {
					if len(refs) != 1 {
						t.Error("edit reference lost")
					}
					verdict := "mismatch"
					if bytes.Equal(o.Data, good) {
						verdict = "matched"
					}
					return reviewFixture(verdict, o.Data)
				}), nil
			}
			task := &imageTaskState{ID: "retry-task", OwnerID: "owner", Mode: "edit", Model: "grok-imagine-image-edit", Prompt: "portrait", N: 1, Size: "1024x1024", Images: [][]byte{tinyPNG}, ImageNames: []string{"ref.png"}}
			s.runImageTask(context.Background(), task, "Bearer api-secret", "")
			want := "error"
			if success {
				want = "success"
			}
			if task.Status != want || generations != 2 {
				t.Fatalf("task=%+v calls=%d", task, generations)
			}
			if !success && (len(task.Data) != 0 || !strings.Contains(task.Error, "image_content_mismatch")) {
				t.Fatal("wrong terminal failure")
			}
			logs := s.loadCallLogs()
			if len(logs) != 1 || len(anyList(mapValue(logs[0]["detail"])["grok_media_reviews"])) != 2 {
				t.Fatalf("lost retry evidence: %v", logs)
			}
			restarted := New(s.cfg)
			if len(restarted.loadCallLogs()) != 1 {
				t.Fatal("image log lost on restart")
			}
		})
	}
}

func TestGrokReviewPreviewExpiryKeepsHashAndProtectsBytes(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	cfg.ImageRetentionDays = 1
	s := New(cfg)
	s.monitor.start("cache-source", "/v1/images/generations", "grok-imagine-image-pro", "fixture")
	s.newImageVerifier = func(context.Context, int) (imageResultVerifier, error) {
		return imageReviewFunc(func(context.Context, string, []provider.OpenAIImageInput, provider.OpenAIImageInput) videoVerification {
			return reviewFixture("mismatch", tinyPNG)
		}), nil
	}
	hash, err := grokImageFingerprint(tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	s.reviewGrokMedia(context.Background(), "cache-source", "image", "request", hash, "portrait", nil, tinyPNG, 0, 1, 1)
	cached, hit := s.cachedGrokReview("image", hash, "request")
	if !hit || len(cached.Images) != 1 {
		t.Fatal("no persisted evidence")
	}
	path := filepath.Join(s.logInputImagesDir(), filepath.Base(cached.Images[0].URL))
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("evidence file is not private")
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	s.cleanupExpiredImages()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expired bytes retained")
	}
	if _, hit := New(cfg).cachedGrokReview("image", hash, "request"); !hit {
		t.Fatal("preview cleanup deleted hash evidence")
	}
	if got := adminRequest(s.Handler(), "GET", cached.Images[0].URL, nil); got.Code != 404 {
		t.Fatalf("expired preview %d", got.Code)
	}
}

func TestGrokReviewerDiagnosticRedactsSecretsWithoutInventingVerdict(t *testing.T) {
	s, _ := multimodalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		text := "Cannot inspect. Authorization: Bearer private-secret\nhttps://example.com/image?token=private-token\nfirst"
		event, _ := json.Marshal(map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}, "content": map[string]any{"parts": []string{text}}}})
		fmt.Fprintf(w, "data: %s\ndata: [DONE]\n", event)
	})
	verifier, err := s.prepareImageVerifier(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := verifier.VerifyImage(context.Background(), "portrait", nil, provider.OpenAIImageInput{Name: "output.png", MIME: "image/png", Data: tinyPNG})
	verifier.Close()
	if got.Verdict != "uncertain" || len(got.checks) != 2 {
		t.Fatalf("got=%+v", got)
	}
	for _, check := range got.checks {
		if strings.Contains(check.Response, "private-secret") || strings.Contains(check.Response, "https://example.com") || strings.Contains(check.Response, "private-token") {
			t.Fatal("credential/URL leaked")
		}
		if check.ParseError != "invalid_json_object" {
			t.Fatal("lost parse diagnostic")
		}
	}
}
