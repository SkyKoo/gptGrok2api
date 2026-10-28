package provider

import (
	"encoding/json"
	"fmt"
	"mime"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

// Only counts and fixed classifications leave this collector. Never retain response
// text, arbitrary JSON keys/values, headers, asset URLs, or authentication material.
type liteDiagnostics struct {
	HTTPStatus  int    `json:"http_status"`
	Format      string `json:"format"`
	Lines       int    `json:"lines"`
	JSONEvents  int    `json:"json_events"`
	InvalidJSON int    `json:"invalid_json"`
	TextEvents  int    `json:"text_events"`
	ImageEvents int    `json:"image_events"`
	Previews    int    `json:"previews"`
	Moderated   bool   `json:"moderated"`
	Done        bool   `json:"done"`
	Failure     string `json:"failure,omitempty"`
}

func newLiteDiagnostics(status int, contentType string) *liteDiagnostics {
	format := "other"
	mt, _, _ := mime.ParseMediaType(contentType)
	switch mt {
	case "text/event-stream":
		format = "sse"
	case "application/json":
		format = "json"
	case "application/x-ndjson", "application/jsonl":
		format = "ndjson"
	case "text/html":
		format = "html"
	}
	return &liteDiagnostics{HTTPStatus: status, Format: format}
}

func (d *liteDiagnostics) observe(line string) {
	d.Lines++
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
	if line == "[DONE]" {
		d.Done = true
		return
	}
	if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") {
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(line), &obj) != nil {
		d.InvalidJSON++
		return
	}
	d.JSONEvents++
	nodes := []map[string]any{obj}
	if result, ok := obj["result"].(map[string]any); ok {
		nodes = append(nodes, result)
		if response, ok := result["response"].(map[string]any); ok {
			nodes = append(nodes, response)
		}
	}
	for _, node := range nodes {
		if v, ok := node["token"].(string); ok && v != "" {
			d.TextEvents++
		}
		if model, ok := node["modelResponse"].(map[string]any); ok {
			if v, ok := model["message"].(string); ok && v != "" {
				d.TextEvents++
			}
		}
		if failure, ok := node["error"]; ok && failure != nil {
			// Inspect locally, emit only this closed set of categories.
			raw, _ := json.Marshal(failure)
			text := strings.ToLower(string(raw))
			switch {
			case strings.Contains(text, "rate_limit"), strings.Contains(text, "rate limit"), strings.Contains(text, "quota"), strings.Contains(text, "resource_exhausted"):
				d.Failure = "rate_limited"
			case strings.Contains(text, "unauthenticated"), strings.Contains(text, "unauthorized"):
				d.Failure = "authentication_failed"
			case strings.Contains(text, "permission_denied"), strings.Contains(text, "forbidden"):
				d.Failure = "permission_denied"
			default:
				if d.Failure == "" {
					d.Failure = "upstream_error"
				}
			}
		}
	}
}

func (d *liteDiagnostics) err(reason string) error {
	if d.Failure != "" {
		reason = d.Failure
	} else if d.Moderated {
		reason = "moderated"
	}
	d.Failure = reason
	raw, _ := json.Marshal(d)
	status := 502
	switch reason {
	case "rate_limited":
		status = 429
	case "authentication_failed":
		status = 401
	case "permission_denied":
		status = 403
	case "moderated":
		status = 422
	}
	return &protocol.UpstreamError{Status: status, Message: fmt.Sprintf("Grok Lite returned no usable images; diagnostics=%s", raw)}
}
