package httpapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestVideoLookupRecoversCallerKeyWithoutSubmitting(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = filepath.Join(root, "data")
	cfg.VideoDataDir = filepath.Join(root, "videos")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.GrokRuntimeAccountsPath = filepath.Join(root, "grok.json")
	cfg.AuthKeysPath = filepath.Join(root, "auth.json")
	cfg.ConfigPath = filepath.Join(root, "config.json")
	s := newVideoTestServer(cfg)
	key := "lumovart-task-1"
	id := "video_" + imageTaskKey("api", key)
	job := &videoJob{ID: id, Object: "video", OwnerID: "api", ClientKey: key, Status: "in_progress", Model: "veo-3.1-lite", Progress: 100,
		RequestHash: "PRIVATE_REQUEST_HASH", ContentPath: "/PRIVATE_CONTENT_PATH", VideoURL: "https://private.example.test/video?token=PRIVATE_TOKEN",
		Route:        &mediaRoute{Channel: "PRIVATE_CHANNEL", AccountID: "PRIVATE_ACCOUNT", JobKey: "PRIVATE_UPSTREAM_JOB"},
		Verification: &videoVerification{Verdict: "matched", expected: "PRIVATE_EXPECTED", observed: "PRIVATE_OBSERVED"}}
	s.videoJobs[id] = job
	read := func(server *Server, query, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/videos"+query, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)
		return w
	}
	for _, tc := range []struct {
		query, token string
		status       int
	}{
		{"?client_task_id=" + key, "api-secret", 200},
		{"?client_task_id=" + key, "admin-secret", 404},
		{"?client_task_id=missing", "api-secret", 404},
		{"?client_task_id=" + key, "", 401},
		{"", "api-secret", 400},
		{"?client_task_id=", "api-secret", 400},
		{"?client_task_id=%20%09", "api-secret", 400},
		{"?client_task_id=a&client_task_id=b", "api-secret", 400},
		{"?client_task_id=" + strings.Repeat("a", 129), "api-secret", 400},
		{"?client_task_id=" + url.QueryEscape(strings.Repeat("中", 43)), "api-secret", 400},
	} {
		w := read(s, tc.query, tc.token)
		if w.Code != tc.status {
			t.Fatalf("query status %d want %d: %s", w.Code, tc.status, w.Body.String())
		}
		if tc.status == 200 {
			var result map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result["id"] != id || result["status"] != "in_progress" || result["progress"] != float64(100) {
				t.Fatalf("lookup changed original state: %v", result)
			}
			for _, name := range []string{"owner_id", "client_key", "request_hash", "route"} {
				if _, ok := result[name]; ok {
					t.Fatalf("private field exposed: %s", name)
				}
			}
			if strings.Contains(w.Body.String(), "PRIVATE_") {
				t.Fatal("lookup leaked private route, media location or verification evidence")
			}
		}
	}
	if len(s.videoJobs) != 1 || job.Status != "in_progress" || job.Progress != 100 {
		t.Fatal("lookup mutated jobs")
	}
	// The lookup also works from a durable checkpoint, with no submission replay.
	job.Status = "failed"
	job.Error = map[string]any{"code": "video_unsafe", "message": "rejected"}
	if err := writeVideoJSON(filepath.Join(cfg.DataDir, "video_tasks", videoJournalName(id)), videoCheckpoint{OwnerID: job.OwnerID, ClientKey: key, Job: *job, Record: monitorRecord{CallID: id}, LogWritten: true}); err != nil {
		t.Fatal(err)
	}
	restarted := newVideoTestServer(cfg)
	w := read(restarted, "?client_task_id="+key, "api-secret")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"failed"`) {
		t.Fatalf("checkpoint lookup failed: %d %s", w.Code, w.Body.String())
	}
	// Even a malformed record indexed at the derived ID cannot cross ownership.
	restarted.videoJobs[id].OwnerID = "someone-else"
	if w = read(restarted, "?client_task_id="+key, "api-secret"); w.Code != 404 {
		t.Fatal("foreign record exposed")
	}
}

func TestVideoLookupSeparatesRegisteredLegacyAndAnonymousCallers(t *testing.T) {
	cfg := adminTestConfig(t.TempDir())
	cfg.AllowAnonymous = true
	registered := []map[string]any{
		{"id": "registered-a", "key_hash": fmt.Sprintf("%x", sha256.Sum256([]byte("registered-token-a")))},
		{"id": "registered-b", "key_hash": fmt.Sprintf("%x", sha256.Sum256([]byte("registered-token-b")))},
		{"key_hash": fmt.Sprintf("%x", sha256.Sum256([]byte("legacy-hash-a")))},
		{"key_hash": fmt.Sprintf("%x", sha256.Sum256([]byte("legacy-hash-b")))},
		{"key": "legacy-plaintext"},
		{"id": "disabled", "key_hash": fmt.Sprintf("%x", sha256.Sum256([]byte("disabled-token"))), "enabled": false},
	}
	if err := writeVideoJSON(cfg.AuthKeysPath, map[string]any{"items": registered}); err != nil {
		t.Fatal(err)
	}
	s := newVideoTestServer(cfg)
	key := "same-client-key"
	callers := []struct{ token, owner string }{
		{"registered-token-a", "registered-a"}, {"registered-token-b", "registered-b"},
		{"legacy-hash-a", fmt.Sprintf("legacy-key:%x", sha256.Sum256([]byte("legacy-hash-a")))},
		{"legacy-hash-b", fmt.Sprintf("legacy-key:%x", sha256.Sum256([]byte("legacy-hash-b")))},
		{"legacy-plaintext", fmt.Sprintf("legacy-key:%x", sha256.Sum256([]byte("legacy-plaintext")))},
		{"api-secret", "api"}, {"admin-secret", "admin"},
	}
	read := func(server *Server, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.Header.Set("X-API-Key", token)
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	for _, caller := range callers {
		id := "video_" + imageTaskKey(caller.owner, key)
		path := filepath.Join(cfg.VideoDataDir, id+".mp4")
		if err := os.MkdirAll(cfg.VideoDataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic-video"), 0o600); err != nil {
			t.Fatal(err)
		}
		job := &videoJob{ID: id, OwnerID: caller.owner, ClientKey: key, Status: "completed", Model: "grok-imagine-video", ContentPath: path,
			RequestHash: videoRequestFingerprint("grok-imagine-video", "a paper plane", "9:16", "720p", 6, "custom", nil)}
		s.videoJobs[id] = job
		if err := writeVideoJSON(filepath.Join(cfg.DataDir, "video_tasks", videoJournalName(id)), videoCheckpoint{OwnerID: caller.owner, ClientKey: key, RequestHash: job.RequestHash, Job: *job, ContentFile: filepath.Base(path), Record: monitorRecord{CallID: id}, LogWritten: true}); err != nil {
			t.Fatal(err)
		}
	}
	// Historical anonymous ownership cannot identify a specific legacy key.
	oldID := "video_" + imageTaskKey("anonymous", key)
	s.videoJobs[oldID] = &videoJob{ID: oldID, OwnerID: "anonymous", ClientKey: key, Status: "in_progress"}
	restarted := newVideoTestServer(cfg)
	for _, caller := range callers {
		t.Run(caller.owner, func(t *testing.T) {
			id := "video_" + imageTaskKey(caller.owner, key)
			for _, server := range []*Server{s, restarted} {
				before, _ := os.ReadFile(filepath.Join(cfg.DataDir, "video_tasks", videoJournalName(id)))
				for _, path := range []string{"/v1/videos?client_task_id=" + key, "/v1/videos/" + id, "/v1/videos/" + id + "/content"} {
					w := read(server, path, caller.token)
					if w.Code != http.StatusOK {
						t.Fatalf("own task unavailable at %s: %d %s", path, w.Code, w.Body.String())
					}
					if !strings.HasSuffix(path, "/content") && !strings.Contains(w.Body.String(), `"id":"`+id+`"`) {
						t.Fatalf("lookup returned another caller's task: %s", w.Body.String())
					}
				}
				// Repeating an acknowledged POST must resolve the same owner and
				// return the stored result, without scheduling another generation.
				r := videoOptionsRequest(t, url.Values{"client_task_id": {key}})
				r.Header.Set("Authorization", "Bearer "+caller.token)
				w := httptest.NewRecorder()
				server.Handler().ServeHTTP(w, r)
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"`+id+`"`) {
					t.Fatalf("POST ownership differs from GET: %d %s", w.Code, w.Body.String())
				}
				after, _ := os.ReadFile(filepath.Join(cfg.DataDir, "video_tasks", videoJournalName(id)))
				if !reflect.DeepEqual(before, after) || server.DeploymentStatus()["detached"] != 0 {
					t.Fatal("lookup or existing-task retry changed checkpoint or launched work")
				}
				for _, other := range callers {
					if other.owner == caller.owner {
						continue
					}
					for _, suffix := range []string{"", "/content"} {
						if w := read(server, "/v1/videos/"+id+suffix, other.token); w.Code != http.StatusNotFound {
							t.Fatalf("foreign task exposed: %d", w.Code)
						}
					}
				}
			}
			if w := read(s, "/v1/videos/"+oldID, caller.token); w.Code != http.StatusNotFound {
				t.Fatal("legacy anonymous task was automatically claimed")
			}
		})
	}
	for _, token := range []string{"", "disabled-token", "invalid-token"} {
		if w := read(s, "/v1/videos?client_task_id="+key, token); w.Code != http.StatusUnauthorized {
			t.Fatalf("unidentified caller could look up task: %d", w.Code)
		}
	}
	registered[0]["key_hash"] = fmt.Sprintf("%x", sha256.Sum256([]byte("rotated-token-a")))
	if err := writeVideoJSON(cfg.AuthKeysPath, map[string]any{"items": registered}); err != nil {
		t.Fatal(err)
	}
	if w := read(s, "/v1/videos?client_task_id="+key, "rotated-token-a"); w.Code != http.StatusOK {
		t.Fatal("registered identity changed when its credential rotated")
	}
	if w := read(s, "/v1/videos?client_task_id="+key, "registered-token-a"); w.Code != http.StatusUnauthorized {
		t.Fatal("retired credential remained authorized")
	}
	for _, useHeader := range []bool{false, true} {
		r := videoOptionsRequest(t, nil)
		r.Header.Del("Authorization")
		if useHeader {
			r.Header.Set("Idempotency-Key", key)
		} else {
			r = videoOptionsRequest(t, url.Values{"client_task_id": {key}})
			r.Header.Del("Authorization")
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || len(s.videoJobs) != len(callers)+1 {
			t.Fatal("anonymous keyed POST admitted a task")
		}
	}
	if w := read(s, "/v1/videos/"+oldID, ""); w.Code != http.StatusOK {
		t.Fatal("existing anonymous by-ID access changed")
	}
	// Do not turn an unattributable pre-upgrade task into a new submission.
	oldKey := "old-unattributed"
	oldID = "video_" + imageTaskKey("anonymous", oldKey)
	oldJob := &videoJob{ID: oldID, OwnerID: "anonymous", ClientKey: oldKey, Status: "failed"}
	s.videoJobs[oldID] = oldJob
	if err := writeVideoJSON(filepath.Join(cfg.DataDir, "video_tasks", videoJournalName(oldID)), videoCheckpoint{OwnerID: "anonymous", ClientKey: oldKey, Job: *oldJob, Record: monitorRecord{CallID: oldID}, LogWritten: true}); err != nil {
		t.Fatal(err)
	}
	for _, server := range []*Server{s, newVideoTestServer(cfg)} {
		count := len(server.videoJobs)
		if w := read(server, "/v1/videos?client_task_id="+oldKey, "legacy-hash-a"); w.Code != http.StatusNotFound {
			t.Fatal("unattributable task exposed by key")
		}
		r := videoOptionsRequest(t, url.Values{"client_task_id": {oldKey}})
		r.Header.Set("Authorization", "Bearer legacy-hash-a")
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusConflict || len(server.videoJobs) != count {
			t.Fatal("old anonymous task was resubmitted under a new owner")
		}
	}
}
