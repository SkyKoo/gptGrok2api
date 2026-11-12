package adobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestSubmissionDiagnosticRetainsSafeFailureEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body, access, wantCode, wantSource, wantAccess, wantError string
		status, major, minor                                            int
		unknown                                                         bool
	}{
		{"timeout", `{"error_code":"timeout_error","message":"PRIVATE_BODY"}`, "PRIVATE_HEADER", "timeout_error", "top_error_code", "unknown", "adobe_submit_timeout_error (HTTP 408)", 408, 2, 0, true},
		{"taste", `{"code":"access_error","message":"PRIVATE_BODY"}`, "taste_exhausted", "access_error", "top_code", "taste_exhausted", "adobe_submit_access_error (HTTP 403)", 403, 1, 1, false},
		{"nested", `{"error":{"error_code":"timeout_error","message":"PRIVATE_BODY"}}`, "", "timeout_error", "nested_error_code", "", "adobe_submit_http_error (HTTP 408)", 408, 2, 0, true},
		{"nested_code", `{"error":{"code":"access_error","message":"PRIVATE_BODY"}}`, "", "access_error", "nested_code", "", "adobe_submit_access_denied (HTTP 403)", 403, 2, 0, false},
		{"untrusted", `{"error_code":"PRIVATE_CODE","code":"PRIVATE_CODE","error":{"code":"PRIVATE_CODE"}}`, "taste_exhausted PRIVATE_HEADER", "http_error", "http_status", "unknown", "adobe_submit_http_error (HTTP 408)", 408, 2, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := NewClient(&http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				response := jsonResponse(tc.status, tc.body)
				response.ProtoMajor, response.ProtoMinor = tc.major, tc.minor
				response.Header.Set("x-access-error", tc.access)
				response.Header.Set("x-access-user-context", "PRIVATE_CONTEXT")
				response.Header.Set("Set-Cookie", "PRIVATE_COOKIE")
				response.Header.Set("Location", "https://PRIVATE_URL")
				return response, nil
			})})
			job, err := client.SubmitImage(context.Background(), fixtureAccount(t), ImageRequest{Model: "gemini-3.1-flash-image-2.1", Prompt: "PRIVATE_PROMPT", Width: 1024, Height: 1024, N: 1})
			var upstream *Error
			if calls != 1 || !errors.As(err, &upstream) || upstream.SubmissionUnknown != tc.unknown || err.Error() != tc.wantError {
				t.Fatalf("submission behavior changed: calls=%d error=%v", calls, err)
			}
			if (job.Status == "SUBMISSION_UNKNOWN") != tc.unknown || job.PollURL != "" {
				t.Fatal("diagnostic changed unknown/replay semantics")
			}
			d := job.SubmissionDiagnostic
			if d == nil || !d.ResponseReceived || d.HTTPStatus != tc.status || d.ErrorCode != tc.wantCode || d.ErrorSource != tc.wantSource || d.AccessError != tc.wantAccess || d.ElapsedMS < 0 {
				t.Fatalf("incorrect safe diagnostic: %+v", d)
			}
			wantProto := "2"
			if tc.major == 1 {
				wantProto = "1.1"
			}
			if d.HTTPProto != wantProto {
				t.Fatalf("unexpected protocol %q", d.HTTPProto)
			}
			raw, marshalErr := json.Marshal(job)
			if marshalErr != nil || strings.Contains(string(raw), "PRIVATE") {
				t.Fatal("private request or response material retained in job")
			}
		})
	}
}

func TestSubmissionDiagnosticTransportFailureHasNoResponse(t *testing.T) {
	calls := 0
	client := NewClient(&http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("PRIVATE transport proxy URL and credentials")
	})})
	job, err := client.SubmitImage(context.Background(), fixtureAccount(t), ImageRequest{Model: "gemini-3.1-flash-image", Prompt: "fixture", Width: 1024, Height: 1024, N: 1})
	d := job.SubmissionDiagnostic
	if calls != 1 || err == nil || err.Error() != "adobe_submit_transport_error" || job.Status != "SUBMISSION_UNKNOWN" || d == nil || d.ResponseReceived || d.HTTPStatus != 0 || d.HTTPProto != "" || d.ErrorCode != "transport_error" || d.ErrorSource != "transport" {
		t.Fatalf("unexpected transport diagnostic: %+v error=%v", d, err)
	}
	raw, _ := json.Marshal(job)
	if strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("raw transport exception leaked")
	}
}

func TestSubmissionDiagnosticPersistsWithoutReleasingUnknownTask(t *testing.T) {
	store, account := readyStore(t)
	const taskID = "diagnostic-task"
	if _, err := store.Reserve(taskID); err != nil {
		t.Fatal(err)
	}
	client := NewClient(&http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		response := jsonResponse(408, `{"error_code":"timeout_error"}`)
		response.ProtoMajor = 2
		return response, nil
	})})
	job, err := client.SubmitImage(context.Background(), account, ImageRequest{Model: "gemini-3.1-flash-image", Prompt: "fixture", Width: 1024, Height: 1024, N: 1})
	if err == nil || job.SubmissionDiagnostic == nil {
		t.Fatal("expected diagnostic failure")
	}
	if err := store.SaveJob(taskID, job); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnknown(taskID); err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(store.path, store.secret)
	saved, err := restarted.LoadJob(taskID)
	if err != nil || !reflect.DeepEqual(saved, job) {
		t.Fatal("submission diagnostic did not survive encrypted persistence")
	}
	if _, err := restarted.Reserve("another-task"); !errors.Is(err, ErrNoAccount) {
		t.Fatal("persisting diagnostic released unknown reservation")
	}
	if _, err := restarted.Resume(taskID, account.ID); err == nil {
		t.Fatal("diagnostic made missing poll handle resumable")
	}
}
