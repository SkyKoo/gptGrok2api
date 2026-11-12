package register

import (
	"context"
	"time"
)

// Adobe mailbox timing is shared by registration and existing-account login.
// Neither flow exposes configurable waits that could keep a browser indefinitely.
const (
	AdobeVerificationWaitTimeout  = defaultMailWaitTimeout
	AdobeVerificationPollInterval = defaultMailPollInterval
)

// IsAdobeVerificationSender uses the same direct and HME-forwarded sender rules
// for both registration and re-login. It never matches an arbitrary iCloud mail.
func IsAdobeVerificationSender(sender string) bool { return isAdobeSender(sender) }

// Keep the mailbox wait deadline distinct from cancellation or expiry of the
// enclosing registration task. The timeout cause belongs only to this wait.
func waitAdobeVerificationCode(ctx context.Context, source MailSource, box Mailbox, after time.Time, timeout time.Duration) (string, error) {
	var wait func(context.Context, Mailbox, time.Time) (string, error)
	switch mail := source.(type) {
	case interface {
		WaitAdobeCode(context.Context, Mailbox, time.Time) (string, error)
	}:
		wait = mail.WaitAdobeCode
	case interface {
		WaitCodeFor(context.Context, Mailbox, time.Time, func(string) bool) (string, error)
	}:
		wait = func(ctx context.Context, box Mailbox, after time.Time) (string, error) {
			return mail.WaitCodeFor(ctx, box, after, isAdobeSender)
		}
	default:
		return "", fail("mail", "adobe_sender_filter_required")
	}
	mailTimeout := fail("mail", "timeout")
	waitCtx, cancel := context.WithTimeoutCause(ctx, timeout, mailTimeout)
	defer cancel()
	code, err := wait(waitCtx, box, after)
	if parentErr := ctx.Err(); parentErr != nil {
		return "", parentErr
	}
	if err != nil && context.Cause(waitCtx) == mailTimeout {
		return "", mailTimeout
	}
	return code, err
}

// AdobeVerificationMailAfter validates the Worker's saved send timestamp before
// selecting mail. Missing timestamps retain the current attempt's start fence.
func AdobeVerificationMailAfter(start time.Time, requestedAt string) (time.Time, error) {
	if requestedAt == "" {
		return start, nil
	}
	requested, err := time.Parse(time.RFC3339Nano, requestedAt)
	now := time.Now().UTC()
	if err != nil || requested.Before(now.Add(-15*time.Minute)) || requested.After(now.Add(5*time.Second)) {
		return time.Time{}, fail("adobe", "recovery_verification_time_invalid")
	}
	// The Worker records its normal send action before clicking. A small clock
	// margin includes the resulting message, rather than codes from old attempts.
	return requested.Add(-5 * time.Second), nil
}
