package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AccountEndpointError contains only sanitized diagnostic data. The private
// cause preserves errors.Is/As for authentication and transport classification.
type AccountEndpointError struct {
	Stage             string `json:"stage"`
	Endpoint          string `json:"endpoint"`
	Status            int    `json:"status,omitempty"`
	Kind              string `json:"kind"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	cause             error
}

func (e *AccountEndpointError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s %s HTTP %d: %s", e.Stage, e.Endpoint, e.Status, e.Message)
	}
	return fmt.Sprintf("%s %s: %s", e.Stage, e.Endpoint, e.Message)
}
func (e *AccountEndpointError) Unwrap() error { return e.cause }

func accountEndpointError(path string, status int, retryAfter string, err error, account map[string]any, secrets ...string) *AccountEndpointError {
	stage := "account_request"
	switch path {
	case "/backend-api/me":
		stage = "user_info"
	case "/backend-api/conversation/init":
		stage = "quota"
	case "/backend-api/accounts/check/v4-2023-04-27":
		stage = "account_plan"
	}
	for _, key := range []string{"access_token", "accessToken", "token", "refresh_token", "id_token", "cookie_header", "cookie", "cookies", "session_token", "password", "login_password", "email", "user_id"} {
		secrets = append(secrets, aiString(account, key))
	}
	secrets = append(secrets, webSessionSecrets(account, "")...)
	// Redact before truncation, including opaque tokens and proxy credentials.
	message := strings.TrimPrefix(imageErrorDetail(err.Error(), secrets...).Message, imageTerminalPrefix)
	if runes := []rune(message); len(runes) > 2048 {
		message = string(runes[:2048]) + " [truncated]"
	}
	if message == "" && status > 0 {
		message = http.StatusText(status)
	}
	e := &AccountEndpointError{Stage: stage, Endpoint: path, Status: status, Kind: "invalid_response", Message: message, cause: err}
	var network net.Error
	switch {
	case status == 401:
		e.Kind = "auth_invalid"
	case status == 403:
		e.Kind = "forbidden"
	case status == 429:
		e.Kind = "rate_limited"
		e.Retryable = true
	case status == 408 || status >= 500:
		e.Kind = "upstream_unavailable"
		e.Retryable = true
	case status > 0:
		e.Kind = "http_error"
	case errors.Is(err, context.Canceled):
		e.Kind = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		e.Kind = "timeout"
		e.Retryable = true
	case errors.As(err, &network):
		e.Kind = "network_error"
		e.Retryable = true
		if network.Timeout() {
			e.Kind = "timeout"
		}
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		e.Kind = "incomplete_response"
		e.Retryable = true
	}
	seconds, parseErr := strconv.Atoi(strings.TrimSpace(retryAfter))
	if parseErr != nil {
		if deadline, parseErr := http.ParseTime(retryAfter); parseErr == nil {
			seconds = int(time.Until(deadline).Seconds()) + 1
		}
	}
	if seconds > 365*24*60*60 {
		seconds = 365 * 24 * 60 * 60
	}
	if seconds > 0 {
		e.RetryAfterSeconds = seconds
	}
	return e
}
