package register

// A flow-start marker records that an RPC may have reached the Worker. It is
// not evidence that Adobe accepted or rejected creation of the account.
type adobeCreationObservation struct {
	Outcome    string
	ErrorCode  string
	HTTPStatus int
}

func adobeCreationOutcome(job RegistrationJob) string {
	if state := job.AdobeRegistration; state != nil {
		switch state.CreationOutcome {
		case "not_submitted", "accepted", "unknown":
			return state.CreationOutcome
		case "rejected":
			if state.CreationErrorCode == "captcha_required" && state.CreationHTTPStatus == 400 {
				return "rejected"
			}
			return "unknown"
		}
		if state.FlowStarted || state.SubmissionStarted {
			return "unknown"
		}
	}
	if adobeLegacyMayHaveSubmitted(job) {
		return "unknown"
	}
	return "not_submitted"
}

func recordAdobeCreationOutcome(job *RegistrationJob, observed adobeCreationObservation, registrationResponse bool) {
	previous := adobeCreationOutcome(*job)
	ensureAdobeRegistrationState(job)
	next := observed.Outcome
	switch next {
	case "not_submitted", "rejected", "accepted", "unknown":
	default:
		next = "unknown"
	}
	if next == "rejected" && (observed.ErrorCode != "captcha_required" || observed.HTTPStatus != 400) {
		next = "unknown"
	}
	// A continuation or lost response cannot undo an accepted Create. Only a
	// response to this attempt's original registration command can prove that
	// creation was not submitted or was explicitly rejected.
	if previous == "accepted" || !registrationResponse && (next == "not_submitted" || next == "rejected") {
		next = previous
	}
	state := job.AdobeRegistration
	state.CreationOutcome = next
	if next != observed.Outcome {
		return
	}
	state.CreationErrorCode, state.CreationHTTPStatus = "", 0
	if observed.ErrorCode == "captcha_required" {
		state.CreationErrorCode = observed.ErrorCode
	}
	if observed.HTTPStatus >= 100 && observed.HTTPStatus <= 599 {
		state.CreationHTTPStatus = observed.HTTPStatus
	}
}

func (e *AdobeEngine) adobeMailboxCreationBlocked(job RegistrationJob) bool {
	mode := adobeRegistrationRecoveryMode(job)
	if mode == "saved_result" {
		return false
	}
	for _, other := range e.jobs {
		if grokMailboxKey(other.Mailbox.Email) != grokMailboxKey(job.Mailbox.Email) {
			continue
		}
		otherMode := adobeRegistrationRecoveryMode(other)
		if otherMode == "result_uncertain" || mode == "retry_registration" && otherMode == "resume_session" {
			return true
		}
	}
	return false
}
