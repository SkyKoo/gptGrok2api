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
		{`{"result":{"response":{"error":"Content Moderated [WKE=grok:content-moderated] SECRET"}}}`, 422},
		{`{"error":{"code":3,"details":[],"message":"Content Moderated [WKE=grok:content-moderated] SECRET"}}`, 422},
		{`data: {"error":{"code":"RESOURCE_EXHAUSTED","message":"video rate limit reached","token":"SECRET"}}`, 429},
		{`data: {"result":{"response":{"error":{"code":"UNAUTHENTICATED","cookie":"SECRET"}}}}`, 401},
		{`data: {"result":{"response":{"streamingVideoGenerationResponse":{"error":{"code":"PERMISSION_DENIED","message":"SECRET"}}}}}`, 403},
		{`data: {"result":{"response":{"streamingVideoGenerationResponse":{"rejectionReason":"VIDEO_AGE_VERIFICATION_REQUIRED","videoUrl":"https://secret.example/SECRET"}}}}`, 422},
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

func TestVideoDiagnosticsIgnoreEmptyErrorFields(t *testing.T) {
	for _, value := range []string{`null`, `false`, `""`, `"  "`, `0`, `{}`, `[]`} {
		d := NewVideoDiagnostics(200, "application/json", false)
		d.Observe(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":1,"error":` + value + `}}}}`)
		if err := d.Failure(); err != nil {
			t.Errorf("unset error %s interrupted video progress: %v", value, err)
		}
	}
}

func TestVideoDiagnosticsModerationErrorSetsSummaryFlag(t *testing.T) {
	d := NewVideoDiagnostics(200, "application/json", false)
	d.Observe(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":1,"moderated":false,"videoPostId":"PRIVATE"}}}}`)
	d.Observe(`{"error":{"code":3,"message":"Content Moderated [WKE=grok:content-moderated]"}}`)
	if !d.Moderated || d.core.Failure != "moderated" || d.Failure() == nil {
		t.Fatalf("error-level moderation was not reflected in diagnostics: %+v", d)
	}
	if strings.Contains(d.Failure().Error(), `"moderated":false`) || strings.Contains(d.Failure().Error(), "PRIVATE") {
		t.Fatal("diagnostic summary lost moderation or exposed response data")
	}
}
