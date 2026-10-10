package provider

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func TestModelObserverOnlyStructuralMetadata(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"message":{"author":{"role":"assistant"},"metadata":{"model_slug":"gpt-6"}}}`, "gpt-6"},
		{`{"message":{"author":{"role":"user"},"metadata":{"model_slug":"gpt-fake"}}}`, ""},
		{`{"message":{"author":{"role":"assistant"},"content":{"model_slug":"gpt-fake","parts":["gpt-fake"]}}}`, ""},
		{`{"p":"/message/metadata/model_slug","o":"replace","v":"gpt-6"}`, "gpt-6"},
		{`{"p":"","o":"patch","v":[{"p":"/message/metadata","v":{"model_slug":"gpt-6"}}]}`, "gpt-6"},
		{`{"v":{"message":{"author":{"role":"assistant"},"metadata":{"model_slug":"gpt-6"}}}}`, "gpt-6"},
		{`{"p":"/message/content/parts/0","v":{"model_slug":"gpt-fake"}}`, ""},
		{`{"p":"/message/metadata/model_slug","v":"Bearer secret"}`, ""},
	} {
		var v any
		_ = json.Unmarshal([]byte(tc.raw), &v)
		got := ""
		notifyUpstreamModel(WithModelObserver(context.Background(), func(s string) { got = s }), v)
		if got != tc.want {
			t.Fatalf("%s: %s", tc.raw, got)
		}
	}
}
func TestOnlyDefiniteModelRejection(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{404, `{"error":{"code":"model_not_found"}}`, true},
		{400, `{"detail":{"code":"unsupported_model"}}`, true},
		{404, `{"error":{"code":"file_not_found"}}`, false},
		{400, `{"message":"model_not_found"}`, false},
		{504, `{"error":{"code":"model_not_found"}}`, false},
	} {
		if IsModelRejected(&protocol.UpstreamError{Status: tc.status, Body: tc.body}) != tc.want {
			t.Fatalf("bad classification %+v", tc)
		}
	}
	if IsModelRejected(errors.New("model_not_found")) {
		t.Fatal("unstructured error accepted")
	}
}
