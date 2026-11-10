package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEveryImageEntryRejectsCallerChannelBeforeWork(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/api/image-tasks/generations", "/v1/images/edits", "/api/image-tasks/edits"} {
		for _, encoding := range []string{"json", "multipart"} {
			if encoding == "multipart" && !strings.HasSuffix(path, "/edits") {
				continue
			}
			for _, model := range []string{"gpt-image-2", "grok-imagine-image-edit", "gemini-3.1-flash-image"} {
				for _, field := range []string{"provider", "channel"} {
					t.Run(path+"/"+encoding+"/"+model+"/"+field, func(t *testing.T) {
						s := imageTaskTestServer(t)
						fields := map[string]any{"client_task_id": "caller-channel", "model": model, "prompt": "test", field: "adobe"}
						if field == "channel" {
							fields["provider"] = "  "
						}
						w := httptest.NewRecorder()
						s.Handler().ServeHTTP(w, imageOutputRequest(t, path, encoding, fields))
						if w.Code != 400 || !strings.Contains(w.Body.String(), `"param":"`+field+`"`) || !strings.Contains(w.Body.String(), "channel routing is managed by CFM") {
							t.Fatalf("%d %s", w.Code, w.Body.String())
						}
						if len(s.imageTasks) != 0 {
							t.Fatal("channel override was accepted as an asynchronous task")
						}
					})
				}
			}
		}
	}
}

func TestImageMultipartRepeatedChannelCannotHideOverride(t *testing.T) {
	for _, path := range []string{"/v1/images/edits", "/api/image-tasks/edits"} {
		for _, field := range []string{"provider", "channel"} {
			t.Run(path+"/"+field, func(t *testing.T) {
				var body bytes.Buffer
				form := multipart.NewWriter(&body)
				_ = form.WriteField("client_task_id", "duplicate-channel")
				_ = form.WriteField("model", "gpt-image-2")
				_ = form.WriteField("prompt", "test")
				_ = form.WriteField(field, " ")
				_ = form.WriteField(field, "adobe")
				_ = form.Close()
				r := httptest.NewRequest("POST", path, &body)
				r.Header.Set("Authorization", "Bearer api-secret")
				r.Header.Set("Content-Type", form.FormDataContentType())
				s, w := imageTaskTestServer(t), httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				if w.Code != 400 || !strings.Contains(w.Body.String(), `"param":"`+field+`"`) {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}
