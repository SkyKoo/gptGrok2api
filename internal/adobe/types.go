// Package adobe implements the verified Adobe Firefly Web Free protocol.
package adobe

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

var (
	ErrNotFound  = errors.New("adobe_account_not_found")
	ErrNoAccount = errors.New("adobe_no_available_account")
	ErrBusy      = errors.New("adobe_account_busy")
	ErrRevision  = errors.New("adobe_credentials_changed")
	ErrIdentity  = errors.New("adobe_account_identity_mismatch")
)

type Quota struct {
	Generation     *GenerationEntitlements `json:"generation,omitempty"`
	LastSuccessAt  string                  `json:"last_success_at,omitempty"`
	Status         string                  `json:"status"`
	Total          *float64                `json:"total,omitempty"`
	Used           *float64                `json:"used,omitempty"`
	Available      *float64                `json:"available,omitempty"`
	CheckedAt      string                  `json:"checked_at,omitempty"`
	AvailableUntil string                  `json:"available_until,omitempty"`
	Error          string                  `json:"error,omitempty"`
}

// Account is private. HTTP responses must use Public, never marshal Account.
type Account struct {
	MediaSessionStatus    string                `json:"media_session_status,omitempty"`
	MediaSessionUpdatedAt int64                 `json:"media_session_updated_at,omitempty"`
	Proxy                 string                `json:"proxy,omitempty"`
	ProxyBinding          *proxyruntime.Binding `json:"proxy_binding,omitempty"`
	ID                    string                `json:"id"`
	AccountID             string                `json:"account_id"`
	Email                 string                `json:"email"`
	Token                 string                `json:"token"`
	Cookie                string                `json:"cookie"`
	RefreshCookie         string                `json:"refresh_cookie,omitempty"`
	Password              string                `json:"password,omitempty"`
	UserAgent             string                `json:"user_agent,omitempty"`
	ProfileID             string                `json:"profile_id,omitempty"`
	RegistrationJobID     string                `json:"registration_job_id,omitempty"`
	RegistrationMailbox   map[string]any        `json:"registration_mailbox,omitempty"`
	Enabled               bool                  `json:"enabled"`
	Revision              uint64                `json:"credential_revision"`
	ExpiresAt             int64                 `json:"expires_at"`
	Quota                 Quota                 `json:"quota"`
	Status                string                `json:"status"`
	LastError             string                `json:"last_error,omitempty"`
	CreatedAt             int64                 `json:"created_at"`
	UpdatedAt             int64                 `json:"updated_at"`
	LastUsedAt            int64                 `json:"last_used_at,omitempty"`
	LastRefreshAt         int64                 `json:"last_refresh_at,omitempty"`
	NextRefreshAt         int64                 `json:"next_refresh_at,omitempty"`
	InFlight              string                `json:"in_flight,omitempty"`
	SettlementUntil       int64                 `json:"settlement_until,omitempty"`
}

type ImageRequest struct {
	Model, Prompt           string
	Width, Height, N        int
	AspectRatio, Resolution string
	References              []ReferenceImage
}
type VideoRequest struct {
	Model, Prompt           string
	Width, Height, Duration int
	Resolution, AspectRatio string
	GenerateAudio           bool
	References              []ReferenceImage
}
type Job struct {
	Width          int      `json:"width,omitempty"`
	Height         int      `json:"height,omitempty"`
	AspectRatio    string   `json:"aspect_ratio,omitempty"`
	Resolution     string   `json:"resolution,omitempty"`
	Duration       int      `json:"duration,omitempty"`
	GenerateAudio  bool     `json:"generate_audio,omitempty"`
	ReferenceCount int      `json:"reference_count,omitempty"`
	Kind           string   `json:"kind"`
	Model          string   `json:"model"`
	ModelID        string   `json:"model_id"`
	ModelVersion   string   `json:"model_version"`
	RequestID      string   `json:"request_id"`
	PollURL        string   `json:"poll_url,omitempty"`
	Status         string   `json:"status"`
	Progress       float64  `json:"progress"`
	OutputURLs     []string `json:"output_urls,omitempty"`
}

// Error never includes response bodies, URLs, headers, prompts or credentials.
type Error struct {
	Stage             string
	Code              string
	Status            int
	SubmissionUnknown bool
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("adobe_%s_%s (HTTP %d)", e.Stage, e.Code, e.Status)
	}
	return "adobe_" + e.Stage + "_" + e.Code
}
func SafeError(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Error()
	}
	switch {
	case errors.Is(err, ErrIdentity):
		return ErrIdentity.Error()
	case errors.Is(err, ErrBusy):
		return ErrBusy.Error()
	case errors.Is(err, ErrRevision):
		return ErrRevision.Error()
	case errors.Is(err, ErrNotFound):
		return ErrNotFound.Error()
	case errors.Is(err, ErrNoAccount):
		return ErrNoAccount.Error()
	}
	return "adobe_operation_failed"
}

func TokenIdentity(token string) (string, int64, error) {
	p := strings.Split(token, ".")
	if len(p) != 3 {
		return "", 0, errors.New("adobe_token_invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(p[1])
	if err != nil {
		return "", 0, errors.New("adobe_token_invalid")
	}
	var c map[string]json.RawMessage
	if json.Unmarshal(raw, &c) != nil {
		return "", 0, errors.New("adobe_token_invalid")
	}
	id := ""
	for _, k := range []string{"user_id", "aa_id", "sub"} {
		if json.Unmarshal(c[k], &id) == nil && id != "" {
			break
		}
	}
	if id == "" {
		return "", 0, errors.New("adobe_token_identity_missing")
	}
	number := func(k string) float64 {
		var n float64
		if json.Unmarshal(c[k], &n) == nil {
			return n
		}
		var s string
		_ = json.Unmarshal(c[k], &s)
		n, _ = strconv.ParseFloat(s, 64)
		return n
	}
	exp := int64(number("exp"))
	created, life := number("created_at"), number("expires_in")
	if created > 0 && life > 0 { // Adobe encodes these two fields in milliseconds.
		if created > 1e12 {
			exp = int64((created + life) / 1000)
		} else {
			exp = int64(created + life)
		}
	}
	return id, exp, nil
}

// Nonce mirrors JavaScript prompt.slice(0,256), which counts UTF-16 units.
func Nonce(id, prompt string) string {
	u := utf16.Encode([]rune(prompt))
	if len(u) > 256 {
		u = u[:256]
	}
	p := string(utf16.Decode(u))
	sum := sha256.Sum256([]byte(id + "-" + p))
	return hex.EncodeToString(sum[:])
}

// Sherlock forwards the official opaque ARP session token unchanged. Its internal
// JSON fields are not a public schema and must not be invented or required.
func Sherlock(cookie string) (string, error) {
	for _, part := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 || kv[0] != "sherlockToken" {
			continue
		}
		if len(kv[1]) == 0 || len(kv[1]) > 65536 {
			break
		}
		valid := true
		for _, c := range kv[1] {
			if c < 33 || c > 126 {
				valid = false
				break
			}
		}
		if valid {
			return kv[1], nil
		}
		break
	}
	return "", errors.New("adobe_sherlock_session_incomplete")
}
func Public(a Account) map[string]any {
	q := a.Quota
	now := time.Now()
	if q.Status == "known" {
		at, _ := time.Parse(time.RFC3339Nano, q.CheckedAt)
		if at.IsZero() || now.Sub(at) > 5*time.Minute {
			q.Status = "stale"
		}
	}
	if q.Generation != nil {
		generation := *q.Generation
		if generation.Status == "known" {
			at, _ := time.Parse(time.RFC3339Nano, generation.CheckedAt)
			if at.IsZero() || now.Sub(at) > 5*time.Minute || q.Status != "known" {
				generation.Status = "stale"
			}
		}
		q.Generation = &generation
	}
	status := a.Status
	if !a.Enabled {
		status = "disabled"
	} else if a.Status == "submission_unknown" || a.Status == "invalid" {
		status = a.Status
	} else if a.ExpiresAt <= now.Unix() {
		status = "expired"
	} else if a.InFlight != "" {
		status = "busy"
	} else if q.Status != "known" {
		status = "pending"
	} else if q.Generation != nil && q.Generation.Exhausted() {
		status = "exhausted"
	}
	return map[string]any{"id": a.ID, "account_id": a.AccountID, "email": a.Email, "provider": "adobe", "media_session_refreshed": a.MediaSessionStatus == "refreshed", "media_session_status": a.MediaSessionStatus, "media_session_updated_at": a.MediaSessionUpdatedAt, "status": status, "disabled": !a.Enabled, "enabled": a.Enabled, "expires_at": a.ExpiresAt, "credential_revision": a.Revision, "has_token": a.Token != "", "has_cookie": a.Cookie != "", "has_refresh_cookie": a.RefreshCookie != "", "has_profile": a.ProfileID != "", "quota": q, "last_error": a.LastError, "created_at": a.CreatedAt, "updated_at": a.UpdatedAt, "last_used_at": a.LastUsedAt, "last_refresh_at": a.LastRefreshAt, "next_refresh_at": a.NextRefreshAt, "pending": a.InFlight != "" || a.SettlementUntil > now.Unix(), "in_flight": a.InFlight != ""}
}
