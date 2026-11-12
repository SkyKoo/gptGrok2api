// Package browserqueue reserves a Worker-owned browser for its entire lifetime.
package browserqueue

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type Lease struct {
	ID, base, key string
	client        *http.Client
}
type status struct {
	Status         string `json:"status"`
	Code           string `json:"code"`
	DiagnosticCode string `json:"diagnostic_code"`
}

// StartupError preserves only allowlisted diagnostics. The generic Error text
// keeps existing callers' retry classifications unchanged.
type StartupError struct{ Code string }

func (*StartupError) Error() string { return "browser_start_failed" }

func startupFailure(code string) error {
	switch code {
	case "state_missing", "run_email_mismatch", "run_locked", "unsafe_storage":
		return &StartupError{Code: code}
	default:
		return errors.New("browser_start_failed")
	}
}

// Acquire prepares the browser before any registration or device-code action.
// Polling only observes this reservation; it never replays a business request.
func Acquire(ctx context.Context, client *http.Client, base, key string, options map[string]any, progress func(string) error) (*Lease, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("browser_worker_config_invalid")
	}
	if client == nil {
		client = http.DefaultClient
	}
	dedicated := *client
	dedicated.Timeout = 12 * time.Second
	dedicated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dedicated.Jar = nil
	rawID := make([]byte, 24)
	if _, err := rand.Read(rawID); err != nil {
		return nil, errors.New("browser_reservation_failed")
	}
	lease := &Lease{ID: hex.EncodeToString(rawID), base: strings.TrimRight(base, "/") + "/v1/browser/reservations", key: key, client: &dedicated}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	keep := false
	defer func() {
		if !keep {
			lease.Release()
		}
	}()
	payload := map[string]any{"id": lease.ID}
	for k, v := range options {
		if k != "id" {
			payload[k] = v
		}
	}
	if progress != nil {
		if err := progress("waiting_browser"); err != nil {
			return nil, err
		}
	}
	current, err := lease.request(ctx, http.MethodPost, lease.base, payload)
	if err != nil {
		return nil, err
	}
	previous := ""
	for {
		stage := current.Status
		if stage == "ready" {
			keep = true
			return lease, nil
		}
		if stage != "waiting_browser" && stage != "starting_browser" {
			return nil, startupFailure(current.DiagnosticCode)
		}
		if progress != nil && previous != stage {
			if err := progress(stage); err != nil {
				return nil, err
			}
			previous = stage
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.New("browser_wait_timeout")
		case <-timer.C:
		}
		// Call even when the stage did not change, so cancellation/account guards run.
		if progress != nil {
			if err := progress(stage); err != nil {
				return nil, err
			}
		}
		current, err = lease.request(ctx, http.MethodGet, lease.base+"/"+lease.ID, nil)
		if err != nil {
			return nil, err
		}
	}
}

func (l *Lease) request(ctx context.Context, method, path string, payload any) (status, error) {
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	req, err := http.NewRequestWithContext(proxyruntime.WithURL(ctx, ""), method, path, bytes.NewReader(body))
	if err != nil {
		return status{}, errors.New("browser_worker_request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	if l.key != "" {
		req.Header.Set("Authorization", "Bearer "+l.key)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return status{}, errors.New("browser_worker_unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return status{}, errors.New("browser_worker_rejected")
	}
	var result status
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result) != nil {
		return status{}, errors.New("browser_worker_invalid_response")
	}
	return result, nil
}

// Release is idempotent. If the request is lost, the Worker's reservation TTL
// and cleanup still own the browser; unconfirmed close never frees its slot.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	_, _ = l.request(ctx, http.MethodDelete, l.base+"/"+l.ID, nil)
}
