package provider

import (
	"errors"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func TestVideoDiagnosticsClassifySSEErrorsWithoutLeakingBody(t *testing.T) {
	for _, tc := range []struct {
		line   string
		status int
	}{
		{`data: {"error":{"code":"RESOURCE_EXHAUSTED","message":"video rate limit reached","token":"SECRET"}}`, 429},
		{`data: {"result":{"response":{"error":{"code":"UNAUTHENTICATED","cookie":"SECRET"}}}}`, 401},
		{`data: {"result":{"response":{"streamingVideoGenerationResponse":{"error":{"code":"PERMISSION_DENIED","message":"SECRET"}}}}}`, 403},
		{`data: {"result":{"response":{"streamingVideoGenerationResponse":{"moderated":true,"videoUrl":"https://secret.example/SECRET"}}}}`, 422},
	} {
		d := NewVideoDiagnostics(200, "text/event-stream", false)
		d.Observe(tc.line)
		e := d.Failure()
		var u *protocol.UpstreamError
		if !errors.As(e, &u) || u.Status != tc.status {
			t.Fatalf("status=%d err=%v", tc.status, e)
		}
		if strings.Contains(e.Error(), "SECRET") || strings.Contains(e.Error(), "secret.example") {
			t.Fatal("response data leaked")
		}
	}
}

func TestVideoDiagnosticsDistinguishEmptyStreamAndIDOnly(t *testing.T) {
	d := NewVideoDiagnostics(200, "text/event-stream", false)
	d.Observe(`data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"SECRET"}}}}`)
	d.Observe("data: [DONE]")
	if d.Failure() != nil || d.VideoEvents != 1 || d.MaxProgress != 100 || !d.PostIDPresent || d.URLPresent {
		t.Fatalf("wrong diagnostics: %+v", d)
	}
	if strings.Contains(d.Err("user_id_missing").Error(), "SECRET") {
		t.Fatal("post id leaked")
	}
}
