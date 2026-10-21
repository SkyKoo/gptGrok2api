package register

import (
	"context"
	"net/url"
	"strings"
	"time"
)

// SetLoginTOTPSecret supplies an existing authenticator seed for this login.
// It never enrolls a factor or puts the seed in a request, result or progress.
func (r *WebRegistrar) SetLoginTOTPSecret(secret string) {
	r.loginTOTPSecret = strings.TrimSpace(secret)
}

func loginMFARequired(data map[string]any, next string) bool {
	switch strings.ToLower(stringValue(object(data["page"])["type"])) {
	case "mfa", "mfa_challenge", "mfa-challenge", "totp":
		return true
	}
	u, err := url.Parse(next)
	if err != nil {
		return false
	}
	path := strings.TrimRight(u.Path, "/")
	return path == "/mfa-challenge" || strings.HasPrefix(path, "/mfa-challenge/")
}

// Only explicit MFA pages are handled. Factor IDs come from their structured
// payload or known route; unrelated HTML is never guessed to be a challenge.
func (r *WebRegistrar) completeLoginMFA(ctx context.Context, data map[string]any, next string) (webReply, error) {
	if r.loginProgress != nil {
		if err := r.loginProgress("mfa"); err != nil {
			return webReply{}, err
		}
	}
	if r.loginMFACompleted {
		return webReply{}, fail("mfa", "verification_not_completed")
	}
	base, _ := url.Parse(r.auth + "/")
	u, err := base.Parse(next)
	if err != nil || (next != "" && !sameOrigin(u.String(), r.auth)) {
		return webReply{}, fail("mfa", "untrusted_continuation")
	}
	payload := object(object(data["page"])["payload"])
	factor := stringValue(data["factor_id"])
	if factor == "" {
		factor = stringValue(data["id"])
	}
	if factor == "" {
		factor = stringValue(payload["factor_id"])
	}
	if factor == "" {
		factor = stringValue(payload["id"])
	}
	if factor == "" && strings.HasPrefix(u.Path, "/mfa-challenge/") {
		factor = strings.TrimPrefix(strings.TrimRight(u.Path, "/"), "/mfa-challenge/")
	}
	if factor == "" || len(factor) > 256 || strings.ContainsAny(factor, "/\\\r\n") {
		return webReply{}, fail("mfa", "factor_id_missing")
	}
	if r.loginTOTPSecret == "" {
		return webReply{}, fail("mfa", "secret_required")
	}
	if _, err := TOTPCode(r.loginTOTPSecret, time.Now()); err != nil {
		return webReply{}, fail("mfa", "invalid_secret")
	}
	// One attempt per login, including a rejection. Never retry a code or enroll
	// a replacement factor when an existing authenticator challenge is rejected.
	r.loginMFACompleted = true
	issued, err := r.json(ctx, "mfa", "/api/accounts/mfa/issue_challenge", map[string]any{"id": factor, "type": "totp", "force_fresh_challenge": false}, map[string]string{"referer": r.auth + "/mfa-challenge"})
	if err != nil {
		return webReply{}, err
	}
	if issued.status != 200 {
		return webReply{}, fail("mfa", "unexpected_challenge_status")
	}
	code, err := TOTPCode(r.loginTOTPSecret, time.Now())
	if err != nil {
		return webReply{}, fail("mfa", "invalid_secret")
	}
	verified, err := r.json(ctx, "mfa", "/api/accounts/mfa/verify", map[string]string{"id": factor, "type": "totp", "code": code}, map[string]string{"referer": r.auth + "/mfa-challenge/" + url.PathEscape(factor)})
	if err != nil {
		return webReply{}, err
	}
	if verified.status != 200 {
		return webReply{}, fail("mfa", "unexpected_verification_status")
	}
	if continuationURL(verified.data) == "" && verified.headers.Get("Location") == "" && !loginMFARequired(verified.data, "") {
		return webReply{}, fail("mfa", "continuation_missing")
	}
	return verified, nil
}
