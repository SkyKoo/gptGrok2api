package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func adobeFixtureAccount(t *testing.T, s *Server) adobe.Account {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{"user_id": "fixture-adobe", "exp": time.Now().Add(time.Hour).Unix()})
	cookie, _ := json.Marshal(map[string]string{"sid": "fixture", "ark": "fixture", "bfp": "fixture", "ftr": "fixture"})
	rows, err := s.adobeStore.Import([]adobe.Account{{AccountID: "fixture-adobe", Email: "fixture@example.test", Enabled: true, ProfileID: "fixture-profile", Token: "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature", Cookie: "sherlockToken=" + base64.StdEncoding.EncodeToString(cookie)}})
	if err != nil {
		t.Fatal(err)
	}
	q := 10.0
	a := rows[0]
	err = s.adobeStore.UpdateQuota(a.ID, a.Revision, adobe.Quota{Status: "known", Available: &q, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	s.adobeStore.SetBrowserRefresher(adobeFixtureMediaRefresh)
	return adobeFixtureModelPermissions(t, s, a)
}

func adobeFixtureModelPermissions(t *testing.T, s *Server, a adobe.Account) adobe.Account {
	t.Helper()
	one, available := 1.0, 10.0
	const resource = "firefly_complimentary_generations"
	permissions := adobe.GenerationEntitlements{Status: "known", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Pools: []adobe.GenerationPool{{ID: "fixture-free-pool", ResourceType: resource, Available: &available}}}
	for _, m := range adobe.FreeModels() {
		permissions.Models = append(permissions.Models, adobe.ModelEntitlement{Model: m.ID, Kind: m.Kind, Authorized: true, ResourceType: resource, PoolID: "fixture-free-pool", Cost: &one})
	}
	if err := s.adobeStore.UpdateGenerationPermissions(a.ID, a.Revision, permissions); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.adobeStore.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func adobeFixtureMediaRefresh(_ context.Context, current adobe.Account) (adobe.Account, error) {
	current.Cookie = "sherlockToken=fixture-fresh-media-session"
	current.MediaSessionRefreshed = true
	current.MediaSessionStatus = "complete"
	current.MediaSessionARPSource = "spl_runtime"
	current.MediaSessionARPMode = "present"
	return current, nil
}
func adobeFixtureClient(t *testing.T, submits, polls *atomic.Int32, failed bool) *adobe.Client {
	t.Helper()
	var output bytes.Buffer
	_ = png.Encode(&output, image.NewRGBA(image.Rect(0, 0, 1024, 1024)))
	return adobe.NewClient(&http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := "{}"
		header := http.Header{"Content-Type": []string{"application/json"}}
		status := 200
		switch {
		case r.URL.Path == "/v2/models/generation-preflight":
			body = adobeFreePreflightFixture(false)
		case strings.Contains(r.URL.Path, "generate-async"):
			submits.Add(1)
			if failed {
				return nil, fmt.Errorf("simulated connection loss after submit")
			}
			body = `{"links":{"result":{"href":"https://bks-epo8552.adobe.io/v2/jobs/result/fixture"}}}`
		case strings.Contains(r.URL.Path, "/v2/jobs/result/"):
			polls.Add(1)
			body = `{"status":"COMPLETED","outputs":[{"image":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/fixture.png"}}]}`
		case r.URL.Hostname() == "pre-signed-firefly-prod.s3-accelerate.amazonaws.com":
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("credentials sent to download host")
			}
			body = output.String()
			header.Set("Content-Type", "image/png")
		default:
			t.Errorf("unexpected upstream request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})})
}
func waitAdobeImage(t *testing.T, s *Server, id string) *imageTaskState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.imageTaskMu.RLock()
		var copy imageTaskState
		found := false
		for _, task := range s.imageTasks {
			if task.ID == id {
				copy = *task
				found = true
				break
			}
		}
		s.imageTaskMu.RUnlock()
		if found && (copy.Status == "success" || copy.Status == "error") {
			return &copy
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("image task did not terminate")
	return nil
}
func TestAdobeAsyncDuplicateNeverResubmits(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			s := imageTaskTestServer(t)
			adobeFixtureAccount(t, s)
			var submits, polls, prepares atomic.Int32
			s.adobeStore.SetBrowserRefresher(func(ctx context.Context, current adobe.Account) (adobe.Account, error) {
				prepares.Add(1)
				return adobeFixtureMediaRefresh(ctx, current)
			})
			s.adobeClient = adobeFixtureClient(t, &submits, &polls, failed)
			body := `{"client_task_id":"adobe-once","model":"gemini-3.1-flash-image","prompt":"one blue circle","size":"1024x1024"}`
			first := imageTaskCall(s, "POST", "/api/image-tasks/generations", "api-secret", body)
			if first.Code != 202 {
				t.Fatalf("accept: %d %s", first.Code, first.Body.String())
			}
			task := waitAdobeImage(t, s, "adobe-once")
			if failed && task.Status != "error" || !failed && task.Status != "success" {
				t.Fatalf("unexpected result: %s %s", task.Status, task.Error)
			}
			for i := 0; i < 2; i++ {
				again := imageTaskCall(s, "POST", "/api/image-tasks/generations", "api-secret", body)
				if again.Code != 202 {
					t.Fatal(again.Body.String())
				}
			}
			if submits.Load() != 1 || prepares.Load() != 1 {
				t.Fatalf("generation or media preparation replayed: submits=%d prepares=%d", submits.Load(), prepares.Load())
			}
			if task.Route == nil || task.Route.Channel != "adobe" {
				t.Fatal("route not persisted")
			}
			if strings.Contains(first.Body.String(), "account_id") || strings.Contains(first.Body.String(), "channel") {
				t.Fatal("routing leaked into public response")
			}
			conflict := imageTaskCall(s, "POST", "/api/image-tasks/generations", "api-secret", strings.Replace(body, "blue circle", "red square", 1))
			if conflict.Code != 409 {
				t.Fatal("idempotency conflict accepted")
			}
		})
	}
}
func TestAdobeRestartResumesHandleWithoutGeneration(t *testing.T) {
	s := imageTaskTestServer(t)
	a := adobeFixtureAccount(t, s)
	key := "image-recovery"
	if _, err := s.adobeStore.Reserve(key); err != nil {
		t.Fatal(err)
	}
	handle := adobe.Job{Kind: "image", Model: "gemini-3.1-flash-image", PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: "SUBMITTED"}
	if err := s.adobeStore.SaveJob(key, handle); err != nil {
		t.Fatal(err)
	}
	task := &imageTaskState{ID: "recovery", OwnerID: "owner", Mode: "generate", Model: handle.Model, N: 1, Status: "running", Route: &mediaRoute{Channel: "adobe", AccountID: a.ID, JobKey: key, Stage: "polling"}}
	s.imageTaskMu.Lock()
	err := s.saveImageTaskLocked(task)
	s.imageTaskMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewWithWorkers(s.cfg, false)
	var submits, polls, prepares atomic.Int32
	fresh.adobeStore.SetBrowserRefresher(func(_ context.Context, current adobe.Account) (adobe.Account, error) {
		prepares.Add(1)
		return adobe.Account{}, fmt.Errorf("recovery must not prepare a new media session")
	})
	fresh.adobeClient = adobeFixtureClient(t, &submits, &polls, false)
	fresh.recoverAdobeImageTasks()
	got := waitAdobeImage(t, fresh, "recovery")
	if got.Status != "success" {
		t.Fatalf("recovery: %s", got.Error)
	}
	if submits.Load() != 0 || polls.Load() != 1 || prepares.Load() != 0 {
		t.Fatalf("restart submitted=%d polls=%d prepares=%d", submits.Load(), polls.Load(), prepares.Load())
	}
}
func TestAdobeUnsupportedCombinationNeverSelectsAccount(t *testing.T) {
	s := imageTaskTestServer(t)
	adobeFixtureAccount(t, s)
	var submits, polls atomic.Int32
	s.adobeClient = adobeFixtureClient(t, &submits, &polls, false)
	for _, extra := range []string{`,"size":"2048x2048"`, `,"n":2`, `,"quality":"high"`, `,"background":"transparent"`, `,"output_format":"gif"`, `,"provider":"adobe"`} {
		result := imageTaskCall(s, "POST", "/v1/images/generations", "api-secret", `{"model":"gemini-3.1-flash-image","prompt":"blue circle"`+extra+`}`)
		if result.Code != 400 {
			t.Fatalf("%s: %d %s", extra, result.Code, result.Body.String())
		}
	}
	if submits.Load() != 0 {
		t.Fatal("unsupported combination submitted")
	}
}
func TestAdobeVideoFreeParameters(t *testing.T) {
	good := "model=veo-3.1-lite&seconds=8&resolution_name=1080p&size=1920x1080&generate_audio=false"
	for _, q := range []string{good, strings.Replace(good, "seconds=8", "seconds=4", 1), strings.Replace(good, "1080p", "720p", 1), strings.Replace(good, "false", "invalid", 1), good + "&preset=normal", good + "&provider=adobe"} {
		r := httptest.NewRequest("POST", "/v1/videos", nil)
		r.Form, _ = url.ParseQuery(q)
		_, err := parseVideoOptions(r)
		if (q == good) != (err == nil) {
			t.Fatalf("invalid combination handling: %s %v", q, err)
		}
	}
}
func TestModelPriorityAndFreeCatalog(t *testing.T) {
	if err := validateChannelOrder([]string{"chatgpt", "adobe"}, []string{"adobe", "chatgpt"}); err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]string{{"adobe"}, {"adobe", "adobe"}, {"grok", "adobe"}} {
		if validateChannelOrder([]string{"chatgpt", "adobe"}, order) == nil {
			t.Fatal("invalid priority accepted")
		}
	}
	s := imageTaskTestServer(t)
	w := adminRequest(s.Handler(), "PUT", "/api/model-routing", strings.NewReader(`{"priorities":{"veo-3.1-lite":["adobe"]}}`))
	if w.Code != 400 {
		t.Fatalf("single channel configurable: %d", w.Code)
	}
	if strings.Join(model.MediaChannels("gpt-image-2.5-sunburst"), ",") != "chatgpt" {
		t.Fatal("unqualified Free Sunburst route exposed")
	}
	for _, m := range model.Catalog() {
		if strings.HasPrefix(m.ID, "adobe/") {
			t.Fatal("provider alias exposed")
		}
	}
	if adobeImageFits("gemini-3.1-flash-image", "1024x1024", "auto", 1, 7, provider.ImageOutputOptions{}, false) {
		t.Fatal("unsupported reference input accepted")
	}
}
