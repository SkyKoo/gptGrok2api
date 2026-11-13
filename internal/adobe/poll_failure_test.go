package adobe

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPollOnlyKnownVideoUnsafeResponseIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, kind, body string
		status           int
		terminal         bool
	}{
		{"observed", "video", `{"error_code":"video_unsafe","message":"PRIVATE_BODY"}`, 451, true},
		{"nested", "video", `{"error":{"code":"video_unsafe","message":"PRIVATE_BODY"}}`, 451, true},
		{"bare_status", "video", `{"message":"PRIVATE_BODY"}`, 451, false},
		{"unknown_code", "video", `{"error_code":"PRIVATE_CODE"}`, 451, false},
		{"different_status", "video", `{"error_code":"video_unsafe"}`, 403, false},
		{"different_kind", "image", `{"error_code":"video_unsafe"}`, 451, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := NewClient(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/v2/jobs/result/fixture" {
					t.Fatal("poll performed an unexpected request")
				}
				return jsonResponse(tc.status, tc.body), nil
			})})
			job, err := client.Poll(context.Background(), fixtureAccount(t), Job{Kind: tc.kind, PollURL: "https://bks-epo8552.adobe.io/v2/jobs/result/fixture", Status: "IN_PROGRESS"})
			if calls != 1 || (err == nil) != tc.terminal || (job.Status == "FAILED") != tc.terminal {
				t.Fatalf("terminal=%v calls=%d status=%s error=%v", tc.terminal, calls, job.Status, err)
			}
			if tc.terminal && (job.PollDiagnostic == nil || job.PollDiagnostic.ErrorCode != "video_unsafe" || TerminalPollError(job).Status != 451) {
				t.Fatal("known rejection lost safe diagnostic")
			}
			raw, _ := json.Marshal(job)
			if strings.Contains(string(raw), "PRIVATE") {
				t.Fatal("raw upstream response leaked")
			}
		})
	}
}
