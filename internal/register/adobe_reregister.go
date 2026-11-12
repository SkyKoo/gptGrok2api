package register

import "time"

// SetReregistrationAccountCheck includes accounts imported outside the task
// journal in the manual registration guard. It never performs an upstream login.
func (e *AdobeEngine) SetReregistrationAccountCheck(check func(string) (bool, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reregistrationAccountCheck = check
}

// Reregister is an explicit administrator action for a failed attempt. Unlike
// recovery, it starts a new signup without treating an unknown prior result as
// proof of failure. The original result and automatic no-replay rules remain.
func (e *AdobeEngine) Reregister(raw map[string]any, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.adobeCompensationIdleLocked(); err != nil {
		return err
	}
	for i, job := range e.jobs {
		if job.ID != id {
			continue
		}
		if err := e.adobeReregistrationEligibleLocked(i); err != nil {
			return err
		}
		if e.reregistrationAccountCheck != nil {
			exists, err := e.reregistrationAccountCheck(job.Mailbox.Email)
			if err != nil {
				return fail("storage", "account_lookup_failed")
			}
			if exists {
				return fail("task", "mailbox_already_has_saved_account")
			}
		}
		cfg, err := ParseAdobeConfig(raw)
		if err != nil {
			return err
		}
		return e.startAdobeAttemptLocked(i, cfg, false, true, time.Now())
	}
	return fail("task", "not_found")
}

// Share the structural predicate between the API and its public capability.
// Only the latest leaf for an email can start a new attempt. A running engine
// or an unexpired cooldown temporarily disables that action, not its visibility.
func (e *AdobeEngine) adobeReregistrationEligibleLocked(index int) error {
	parent := e.jobs[index]
	if parent.RetryJobID != "" {
		return fail("task", "retry_already_created")
	}
	if !adobeCompensationPending(parent) {
		return fail("task", "no_saved_mailbox")
	}
	if !adobeCompensationEnvironment(parent) {
		return fail("task", "saved_environment_missing")
	}
	for i, other := range e.jobs {
		if grokMailboxKey(other.Mailbox.Email) != grokMailboxKey(parent.Mailbox.Email) {
			continue
		}
		if len(other.Account) > 0 || other.Imported || other.Verified || adobeCreationOutcome(other) == "accepted" {
			return fail("task", "mailbox_already_has_saved_account")
		}
		if other.Status == "running" || other.Status == "queued" {
			return fail("task", "already_running")
		}
		if i > index && other.RetryJobID == "" {
			return fail("task", "mailbox_retry_already_scheduled")
		}
		if other.AutoRetry != nil {
			if other.AutoRetry.Attempts < 0 {
				return fail("task", "invalid_retry_state")
			}
			if value := other.AutoRetry.NotBeforeAt; value != "" {
				if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
					return fail("task", "invalid_retry_state")
				}
			}
		}
	}
	return nil
}

func (e *AdobeEngine) adobeReregistrationNotBeforeLocked(email string) time.Time {
	var latest time.Time
	for _, other := range e.jobs {
		if grokMailboxKey(other.Mailbox.Email) != grokMailboxKey(email) {
			continue
		}
		if other.ProxyBinding != nil && other.ProxyBinding.BlockedUntil.After(latest) {
			latest = other.ProxyBinding.BlockedUntil
		}
		if other.AutoRetry != nil {
			if due, err := time.Parse(time.RFC3339Nano, other.AutoRetry.NotBeforeAt); err == nil && due.After(latest) {
				latest = due
			}
		}
	}
	return latest
}

// Once an explicitly requested fresh attempt is accepted, its own session may
// continue despite historical uncertainty in its ancestors. This never admits
// another Create, another branch, or a legacy child lacking the durable marker.
func (e *AdobeEngine) adobeAcceptedReregistrationAncestors(job RegistrationJob) map[string]bool {
	state := job.AdobeRegistration
	if job.RetryJobID != "" || adobeRegistrationRecoveryMode(job) != "resume_session" || state == nil || !state.ManualReregistration || state.BrowserProfileID == "" {
		return nil
	}
	byID := make(map[string]RegistrationJob, len(e.jobs))
	for _, candidate := range e.jobs {
		if _, duplicate := byID[candidate.ID]; duplicate {
			return nil
		}
		byID[candidate.ID] = candidate
		if candidate.ID != job.ID && grokMailboxKey(candidate.Mailbox.Email) == grokMailboxKey(job.Mailbox.Email) && (candidate.Status == "running" || candidate.Status == "queued") {
			return nil
		}
	}
	root, origin := mailboxClaimJobID(job), state.BrowserProfileID
	current := job
	originSeen := false
	ancestors, visited := map[string]bool{}, map[string]bool{}
	for {
		if visited[current.ID] || mailboxClaimJobID(current) != root || grokMailboxKey(current.Mailbox.Email) != grokMailboxKey(job.Mailbox.Email) {
			return nil
		}
		visited[current.ID] = true
		if !originSeen {
			currentState := current.AdobeRegistration
			if adobeCreationOutcome(current) != "accepted" || currentState == nil || !currentState.ManualReregistration || currentState.BrowserProfileID != origin {
				return nil
			}
			if current.ID == origin {
				if current.ParentJobID == "" {
					return nil
				}
				originSeen = true
			}
		} else {
			ancestors[current.ID] = true
		}
		if current.ParentJobID == "" {
			if !originSeen || current.ID != root {
				return nil
			}
			return ancestors
		}
		parent, exists := byID[current.ParentJobID]
		if !exists || parent.RetryJobID != current.ID {
			return nil
		}
		current = parent
	}
}
