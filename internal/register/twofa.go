package register

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/browserprofile"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TOTPCode implements RFC 6238 with the six-digit, 30-second SHA1 profile.
func TOTPCode(secret string, now time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimRight(strings.TrimSpace(secret), "=")))
	if err != nil || len(key) < 10 {
		return "", fail("enroll", "invalid_totp_secret")
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 15
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000), nil
}

// RestoreTwoFACookies reuses only cookies bound to the current account token.
func (r *WebRegistrar) RestoreTwoFACookies(cookies any, token, binding string) {
	if binding != "" && binding == browserprofile.TokenBinding(token) {
		r.cookies.Restore(cookies)
	}
}

// BeginTwoFA starts a fresh password reauthentication, without registering an
// account. The caller retains this registrar until activation completes.
func (r *WebRegistrar) BeginTwoFA(ctx context.Context, email string) (time.Time, error) {
	if r.resolveTimezone != nil {
		r.timezone = r.resolveTimezone(ctx, r.chat, r.proxy)
	}
	for _, origin := range []string{r.chat, r.auth} {
		r.cookie(origin, "oai-did", r.device)
	}
	csrf, err := r.request(ctx, "reauth", http.MethodGet, r.chat+"/api/auth/csrf", "", "", nil)
	if err != nil {
		return time.Time{}, err
	}
	token := stringValue(csrf.data["csrfToken"])
	if csrf.status != 200 || token == "" {
		return time.Time{}, fail("reauth", "missing_csrf")
	}
	query := url.Values{"connection": {"password"}, "login_hint": {email}, "reauth": {"password"}, "max_age": {"0"}, "ext-oai-did": {r.device}}
	body := url.Values{"csrfToken": {token}, "callbackUrl": {r.chat + "/?action=enable&factor=totp"}, "json": {"true"}}
	reply, err := r.request(ctx, "reauth", http.MethodPost, r.chat+"/api/auth/signin/openai?"+query.Encode(), body.Encode(), "application/x-www-form-urlencoded", nil)
	if err != nil {
		return time.Time{}, err
	}
	target := stringValue(reply.data["url"])
	u, err := url.Parse(target)
	if err != nil || !sameOrigin(target, r.auth) || u.Path != "/api/accounts/authorize" || u.Query().Get("state") == "" {
		return time.Time{}, fail("reauth", "invalid_authorize_url")
	}
	r.state = u.Query().Get("state")
	after := time.Now().UTC().Add(-10 * time.Second)
	landing, err := r.navigate(ctx, "authorize", target)
	if err != nil {
		return time.Time{}, err
	}
	u, _ = url.Parse(landing)
	if !sameOrigin(landing, r.auth) || strings.TrimRight(u.Path, "/") != "/email-verification" {
		return time.Time{}, fail("reauth", "email_verification_required")
	}
	if _, err = r.navigate(ctx, "send_code", r.auth+"/api/accounts/email-otp/send"); err != nil {
		return time.Time{}, err
	}
	return after, nil
}

// VerifyTwoFAEmail exchanges the email OTP for a freshly reauthenticated web
// token. Every callback is origin/state checked before it is requested.
func (r *WebRegistrar) VerifyTwoFAEmail(ctx context.Context, email, userID, code string) (string, error) {
	if len(code) != 6 || mailCode(code) != code {
		return "", fail("verify_code", "invalid_code")
	}
	reply, err := r.json(ctx, "verify_code", "/api/accounts/email-otp/validate", map[string]string{"code": code}, map[string]string{"referer": r.auth + "/email-verification"})
	if err != nil {
		return "", err
	}
	if reply.status != 200 || continuationURL(reply.data) == "" || profilePage(reply.data) {
		return "", fail("verify_code", "invalid_continuation")
	}
	if err = r.followContinuation(ctx, continuationURL(reply.data), email, false, true); err != nil {
		return "", err
	}
	reply, err = r.request(ctx, "session", http.MethodGet, r.chat+"/api/auth/session", "", "", nil)
	if err != nil {
		return "", err
	}
	user := object(reply.data["user"])
	if !strings.EqualFold(stringValue(user["email"]), email) || userID != "" && stringValue(user["id"]) != userID {
		return "", fail("session", "identity_mismatch")
	}
	token := stringValue(reply.data["accessToken"])
	if reply.status != 200 || token == "" {
		return "", fail("session", "missing_access_token")
	}
	return token, nil
}

func (r *WebRegistrar) twoFARequest(ctx context.Context, stage, path, token string, body any) (webReply, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return webReply{}, fail(stage, "invalid_request")
	}
	reply, err := r.request(ctx, stage, http.MethodPost, r.chat+path, string(raw), "application/json", map[string]string{"authorization": "Bearer " + token, "origin": r.chat, "referer": r.chat + "/", "oai-language": r.profile.NavigatorLanguage})
	if err == nil && reply.status != 200 {
		return reply, fail(stage, "unexpected_status")
	}
	return reply, err
}

func (r *WebRegistrar) EnrollTwoFA(ctx context.Context, token string) (string, string, error) {
	reply, err := r.twoFARequest(ctx, "enroll", "/backend-api/accounts/mfa/enroll", token, map[string]string{"factor_type": "totp"})
	if err != nil {
		return "", "", err
	}
	secret, id := stringValue(reply.data["secret"]), stringValue(reply.data["session_id"])
	if _, err = TOTPCode(secret, time.Now()); err != nil {
		return "", "", err
	}
	if id == "" {
		return "", "", fail("enroll", "missing_session_id")
	}
	return secret, id, nil
}

func (r *WebRegistrar) ActivateTwoFA(ctx context.Context, token, secret, id string) error {
	code, err := TOTPCode(secret, time.Now())
	if err != nil {
		return err
	}
	reply, err := r.twoFARequest(ctx, "activate", "/backend-api/accounts/mfa/user/activate_enrollment", token, map[string]string{"code": code, "factor_type": "totp", "session_id": id})
	if err != nil {
		return err
	}
	if reply.data["success"] != true {
		return fail("activate", "activation_not_confirmed")
	}
	return nil
}
