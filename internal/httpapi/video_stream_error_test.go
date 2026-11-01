package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestVideoSSEFailureIsReportedAndUpdatesScheduling(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		status     int
	}{
		{"quota", `{"error":{"code":"RESOURCE_EXHAUSTED","message":"video rate limit exceeded","secret":"PRIVATE"}}`, 429},
		{"authentication", `{"result":{"response":{"error":{"code":"UNAUTHENTICATED"}}}}`, 401},
		{"moderated", `{"result":{"response":{"streamingVideoGenerationResponse":{"moderated":true,"progress":100}}}}`, 422},
		{"empty", `{"result":{"response":{"token":"No video output"}}}`, 502},
		{"preview", `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":40,"videoUrl":"https://example.invalid/preview.mp4"}}}}`, 502},
		{"id_only", `{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"post-result"}}}}`, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetches := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/post":
					fmt.Fprint(w, `{"post":{"id":"parent"}}`)
				case "/chat":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintln(w, "data: "+tc.line)
					fmt.Fprintln(w, "data: [DONE]")
				default:
					fetches++
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			s := New(adminTestConfig(t.TempDir()))
			s.mediaProvider = provider.NewMedia(upstream.Client(), upstream.URL+"/chat", upstream.URL+"/post", upstream.URL, upstream.URL, s.cfg.RequestTimeout)
			_, _, _, err := s.grokRuntimeStore.AddAccounts(nil, []map[string]any{{"media_quota": testGrokMediaSnapshot(), "access_token": "test-token", "enabled": true, "status": "active"}})
			if err != nil {
				t.Fatal(err)
			}
			job := &videoJob{ID: "video-test", Model: "grok-imagine-video"}
			s.videoJobs[job.ID] = job
			s.runVideoJob(job, "neutral animation", "16:9", "720p", 6, "custom", nil)
			if job.Status != "failed" || intValue(job.Error["upstream_status"]) != tc.status {
				t.Fatalf("status=%s err=%v", job.Status, job.Error)
			}
			if fetches != 0 {
				t.Fatal("download attempted after stream failure")
			}
			if strings.Contains(stringValue(job.Error["message"]), "PRIVATE") {
				t.Fatal("raw error leaked")
			}
			if tc.status == 429 {
				rows, _ := s.grokRuntimeStore.AccountList()
				if accounts.IsAbnormal(rows[0]) {
					t.Fatal("quota invalidated account")
				}
				for _, kind := range []string{"chat", "video"} {
					l, e := s.grokAccountPool.ReserveIntent(context.Background(), []string{"basic"}, nil, nil, 0, accounts.Intent{Kind: kind, Model: job.Model})
					if l != nil {
						s.grokAccountPool.Release(l)
					}
					if kind == "video" && !errors.Is(e, accounts.ErrUnavailable) || kind == "chat" && e != nil {
						t.Fatalf("kind=%s err=%v", kind, e)
					}
				}
			}
		})
	}
}
