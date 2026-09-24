package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestImageTaskTimeoutSettingsValidationAndHotReload(t *testing.T) {
	s := imageTaskTestServer(t)
	read := imageTaskCall(s, "GET", "/api/settings", "admin-secret", "")
	var response struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Config["image_task_timeout_secs"] != float64(600) {
		t.Fatal(read.Body.String())
	}
	ctx, cancel := s.imageTaskContext(context.Background())
	defer cancel()
	oldDeadline, _ := ctx.Deadline()
	if remaining := time.Until(oldDeadline); remaining < 599*time.Second || remaining > 600*time.Second {
		t.Fatal(remaining)
	}
	for _, value := range []string{"null", "false", `"600"`, "0", "59", "900.5", "901"} {
		w := imageTaskCall(s, "POST", "/api/settings", "admin-secret", `{"image_task_timeout_secs":`+value+`,"test_invalid_update":true}`)
		if w.Code != 400 {
			t.Fatalf("value %s: %d %s", value, w.Code, w.Body.String())
		}
	}
	settings, _ := s.store.Config()
	if _, exists := settings["test_invalid_update"]; exists {
		t.Fatal("invalid update partially saved")
	}
	w := imageTaskCall(s, "POST", "/api/settings", "admin-secret", `{"image_task_timeout_secs":900}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	settings, _ = s.store.Config()
	if configuredImageTaskTimeoutSeconds(settings) != 900 {
		t.Fatal("setting not persisted")
	}
	newContext, newCancel := s.imageTaskContext(context.Background())
	defer newCancel()
	newDeadline, _ := newContext.Deadline()
	if remaining := time.Until(newDeadline); remaining < 899*time.Second || remaining > 900*time.Second {
		t.Fatal(remaining)
	}
	nested, nestedCancel := s.imageTaskContext(ctx)
	defer nestedCancel()
	if deadline, _ := nested.Deadline(); !deadline.Equal(oldDeadline) {
		t.Fatal("in-flight deadline changed")
	}
	cancel()
	if nested.Err() != context.Canceled {
		t.Fatal("cancellation lost")
	}
}

func TestQueuedImageTaskKeepsSubmissionTimeout(t *testing.T) {
	s := imageTaskTestServer(t)
	s.imageSlots = makeImageSlots(1)
	release, err := s.acquireImageSlot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, _, _, err := s.store.AddAccounts(nil, []map[string]any{{"access_token": "jwt.header.payload", "pool": "basic"}}); err != nil {
		t.Fatal(err)
	}
	observed := make(chan time.Time, 1)
	client := &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, _ := r.Context().Deadline()
		select {
		case observed <- deadline:
		default:
		}
		return &http.Response{StatusCode: 400, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
	})}
	s.openAIImage = provider.NewOpenAIImage("http://upstream.invalid", client, nil, 30*time.Second)
	submitted := time.Now()
	w := imageTaskCall(s, "POST", "/api/image-tasks/generations", "api-secret", `{"client_task_id":"timeout-snapshot","model":"gpt-image-2","prompt":"test"}`)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	w = imageTaskCall(s, "POST", "/api/settings", "admin-secret", `{"image_task_timeout_secs":900}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	release()
	select {
	case deadline := <-observed:
		budget := deadline.Sub(submitted)
		if budget < 599*time.Second || budget > 602*time.Second {
			t.Fatalf("queued task changed budget: %s", budget)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was not called")
	}
	// Let background persistence finish before the test removes its temporary directory.
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		w = imageTaskCall(s, "GET", "/api/image-tasks/timeout-snapshot", "api-secret", "")
		if strings.Contains(w.Body.String(), `"status":"error"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not finish")
}

func TestImageTaskTimeoutCancelsQueueWait(t *testing.T) {
	s := imageTaskTestServer(t)
	s.imageSlots = makeImageSlots(1)
	release, err := s.acquireImageSlot(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := provider.WithImageTaskTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"test"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer api-secret")
	w := httptest.NewRecorder()
	s.imageGenerations(w, req)
	if w.Code != 504 || !strings.Contains(w.Body.String(), "deadline exceeded") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
