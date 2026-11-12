package register

import "time"

// Adobe mailbox timing is shared by registration and existing-account login.
// Neither flow exposes configurable waits that could keep a browser indefinitely.
const (
	AdobeVerificationWaitTimeout  = defaultMailWaitTimeout
	AdobeVerificationPollInterval = defaultMailPollInterval
)

// IsAdobeVerificationSender uses the same direct and HME-forwarded sender rules
// for both registration and re-login. It never matches an arbitrary iCloud mail.
func IsAdobeVerificationSender(sender string) bool { return isAdobeSender(sender) }

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
