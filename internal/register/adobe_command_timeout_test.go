package register

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
)

type adobeTimeoutRoundTrip func(*http.Request) (*http.Response, error)

func (fn adobeTimeoutRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestAdobeBrowserCommandKeepsRegistrationBudgetWhenSavedClientIsShort(t *testing.T) {
	var seen []time.Duration
	client := &http.Client{Timeout: 120 * time.Second, Transport: adobeTimeoutRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"status":"ready"}`
		if r.URL.Path == "/v1/adobe/run" {
			if deadline, ok := r.Context().Deadline(); ok {
				seen = append(seen, time.Until(deadline))
			} else {
				t.Error("registration request had no HTTP deadline")
			}
			body = `{"stage":"completed","creation_outcome":"accepted"}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	lease, err := browserqueue.Acquire(context.Background(), client, "https://worker.test", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	flow := AdobeBrowserFlow{Client: client, lease: lease}
	cfg := AdobeConfig{WorkerURL: "https://worker.test"}
	for _, action := range []string{"run", "resume", "verify"} {
		if _, err := flow.command(context.Background(), cfg, action, ""); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	lease.Release()
	if len(seen) != 3 {
		t.Fatalf("saw %d registration commands, want 3", len(seen))
	}
	for _, remaining := range seen {
		if remaining < 315*time.Second {
			t.Fatalf("registration HTTP deadline has only %s remaining, want at least 315s", remaining)
		}
	}
}
