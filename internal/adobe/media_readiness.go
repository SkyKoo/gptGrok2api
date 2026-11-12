package adobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// These are local scheduling limits, not a claimed Adobe media-token lifetime.
const MediaSessionReuseWindow = 5 * time.Minute
const MediaSessionWarmLead = time.Minute

type mediaReadiness struct {
	Revision    uint64
	Fingerprint string
	ReadyAt     time.Time
	ReusableTo  time.Time
}

type mediaPreparationFailure struct {
	Revision    uint64                      `json:"credential_revision"`
	Fingerprint string                      `json:"fingerprint"`
	RetryAfter  int64                       `json:"retry_after"`
	Code        string                      `json:"code"`
	ObservedAt  int64                       `json:"observed_at,omitempty"`
	Diagnostic  *MediaPreparationDiagnostic `json:"diagnostic,omitempty"`
}

type mediaPreparationFlight struct {
	done        chan struct{}
	account     Account
	err         error
	fingerprint string
}

// The actual pinned proxy URL matters even for accounts inheriting the global
// default. Only its digest is retained; credentials and proxy passwords never
// appear in readiness or retry metadata. Quota revisions are deliberately absent.
func mediaContextFingerprint(ctx context.Context, a Account) (string, bool) {
	endpoint, selected := proxyruntime.URLSelectionFromContext(ctx)
	if !selected {
		return "", false
	}
	raw, err := json.Marshal(struct {
		ID, Identity, Email, Profile, Token, Cookie, UserAgent, ARPMode, ARPSource string
		Proxy, ProxyPolicy, Endpoint                                               string
		ProxyBinding                                                               *proxyruntime.Binding
		Revision                                                                   uint64
		CreatedAt                                                                  int64
	}{a.ID, a.AccountID, strings.ToLower(strings.TrimSpace(a.Email)), a.ProfileID, a.Token, a.Cookie, a.UserAgent, a.MediaSessionARPMode, a.MediaSessionARPSource, a.Proxy, a.ProxyPolicy, endpoint, a.ProxyBinding, a.Revision, a.CreatedAt})
	if err != nil {
		return "", false
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), true
}

func (s *Store) mediaReadyLocked(ctx context.Context, a Account, minRemaining time.Duration, now time.Time) bool {
	r, ok := s.mediaReady[a.ID]
	if !ok || !a.Enabled || a.Status == "invalid" || a.Status == "submission_unknown" || a.ExpiresAt <= now.Add(time.Minute).Unix() || r.Revision != a.Revision || now.Before(r.ReadyAt) {
		return false
	}
	if minRemaining < 0 {
		minRemaining = 0
	}
	if !r.ReusableTo.After(now.Add(minRemaining)) {
		return false
	}
	fingerprint, selected := mediaContextFingerprint(ctx, a)
	return selected && fingerprint == r.Fingerprint
}

// MediaSessionReady checks only trusted in-process browser evidence. Imported
// fields cannot warm the cache; restarting the process always starts cold.
func (s *Store) MediaSessionReady(ctx context.Context, a Account, minRemaining time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return false
	}
	for _, current := range d.Accounts {
		if current.ID == a.ID && current.Revision == a.Revision {
			return s.mediaReadyLocked(ctx, current, minRemaining, time.Now())
		}
	}
	return false
}

func (s *Store) mediaBackoffLocked(d diskState, ctx context.Context, a Account, now time.Time) bool {
	failure, ok := d.MediaFailures[a.ID]
	fingerprint, selected := mediaContextFingerprint(ctx, a)
	return ok && selected && failure.Revision == a.Revision && failure.Fingerprint == fingerprint && failure.RetryAfter > now.Unix()
}

// MediaPreparationDue is a read-only hint. WarmMediaSession rechecks it under
// the account gate, so a stale scheduler snapshot cannot start a second refresh.
func (s *Store) MediaPreparationDue(ctx context.Context, a Account) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return false
	}
	for _, current := range d.Accounts {
		if current.ID != a.ID || current.Revision != a.Revision {
			continue
		}
		now := time.Now()
		_, selected := mediaContextFingerprint(ctx, current)
		return selected && current.ProfileID != "" && Eligible(current, now) && !s.maintenance[current.ID] && !s.quotaQueries[current.ID] && !s.mediaBackoffLocked(d, ctx, current, now) && !s.mediaReadyLocked(ctx, current, MediaSessionWarmLead, now)
	}
	return false
}

// InvalidateMediaSession cannot invalidate a newer login or browser collection.
// It never changes credentials, task state, or extends the reuse window.
func (s *Store) InvalidateMediaSession(id string, revision uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.mediaReady[id]; ok && r.Revision == revision {
		delete(s.mediaReady, id)
	}
}

func (s *Store) recordMediaReadyLocked(ctx context.Context, a Account) {
	delete(s.mediaReady, a.ID)
	fingerprint, selected := mediaContextFingerprint(ctx, a)
	now := time.Now()
	if !selected || !a.Enabled || a.Status == "submission_unknown" || a.Status == "invalid" || a.ExpiresAt <= now.Add(time.Minute).Unix() {
		return
	}
	until := now.Add(MediaSessionReuseWindow)
	if expiry := time.Unix(a.ExpiresAt, 0).Add(-time.Minute); expiry.Before(until) {
		until = expiry
	}
	if s.mediaReady == nil {
		s.mediaReady = map[string]mediaReadiness{}
	}
	s.mediaReady[a.ID] = mediaReadiness{Revision: a.Revision, Fingerprint: fingerprint, ReadyAt: now, ReusableTo: until}
}

// WarmMediaSession uses the same account gate and browser validation as a new
// task, but never reserves an account, creates a job, or submits generation.
func (s *Store) WarmMediaSession(ctx context.Context, accountID string, revision uint64) (Account, error) {
	if ctx.Err() != nil {
		return Account{}, &Error{Stage: "media_prepare", Code: "cancelled"}
	}
	s.mu.Lock()
	d, err := s.load()
	if err != nil {
		s.mu.Unlock()
		return Account{}, err
	}
	var a Account
	for _, candidate := range d.Accounts {
		if candidate.ID == accountID {
			a = candidate
			break
		}
	}
	if a.ID == "" {
		s.mu.Unlock()
		return a, ErrNotFound
	}
	if a.Revision != revision {
		s.mu.Unlock()
		return a, ErrRevision
	}
	if a.InFlight != "" || !Eligible(a, time.Now()) {
		s.mu.Unlock()
		return a, ErrBusy
	}
	if _, selected := mediaContextFingerprint(ctx, a); !selected {
		s.mu.Unlock()
		return a, &Error{Stage: "media_prepare", Code: "egress_required"}
	}
	if flight := s.mediaFlights[accountID]; flight != nil {
		fingerprint, _ := mediaContextFingerprint(ctx, a)
		if fingerprint != flight.fingerprint {
			s.mu.Unlock()
			return a, ErrBusy
		}
		s.mu.Unlock()
		return waitMediaFlight(ctx, flight)
	}
	if s.maintenance[accountID] || s.quotaQueries[accountID] {
		s.mu.Unlock()
		return a, ErrBusy
	}
	if s.mediaReadyLocked(ctx, a, MediaSessionWarmLead, time.Now()) {
		s.mu.Unlock()
		return a, nil
	}
	if s.mediaBackoffLocked(d, ctx, a, time.Now()) {
		s.mu.Unlock()
		return a, &Error{Stage: "media_prepare", Code: "cooldown"}
	}
	if strings.TrimSpace(a.ProfileID) == "" || s.browserRefresher == nil {
		s.mu.Unlock()
		code := "profile_required"
		if a.ProfileID != "" {
			code = "browser_unavailable"
		}
		return a, &Error{Stage: "media_prepare", Code: code}
	}
	guard := newMediaPreparationGuard(ctx, "", a)
	guard.warm = true
	flight, refresher := s.startMediaFlightLocked(ctx, a), s.browserRefresher
	s.mu.Unlock()
	return s.collectMediaSession(ctx, a, guard, flight, refresher)
}

func waitMediaFlight(ctx context.Context, flight *mediaPreparationFlight) (Account, error) {
	if err := waitMediaSignal(ctx, flight.done); err != nil {
		return Account{}, err
	}
	return flight.account, flight.err
}

func waitMediaSignal(ctx context.Context, signal <-chan struct{}) error {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return &Error{Stage: "media_prepare", Code: "cancelled"}
	case <-timer.C:
		return &Error{Stage: "media_prepare", Code: "wait_timeout"}
	case <-signal:
		if ctx.Err() != nil {
			return &Error{Stage: "media_prepare", Code: "cancelled"}
		}
		return nil
	}
}

func (s *Store) accountChangeSignalLocked(id string) <-chan struct{} {
	if s.accountChanged == nil {
		s.accountChanged = map[string]chan struct{}{}
	}
	if s.accountChanged[id] == nil {
		s.accountChanged[id] = make(chan struct{})
	}
	return s.accountChanged[id]
}

func (s *Store) signalAccountChangedLocked(id string) {
	if s.accountChanged[id] != nil {
		close(s.accountChanged[id])
		delete(s.accountChanged, id)
	}
}

func (s *Store) endAccountMaintenance(id string) {
	s.mu.Lock()
	delete(s.maintenance, id)
	if !s.quotaQueries[id] {
		s.signalAccountChangedLocked(id)
	}
	s.mu.Unlock()
}

func (s *Store) endAccountQuotaQuery(id string) {
	s.mu.Lock()
	delete(s.quotaQueries, id)
	if !s.maintenance[id] {
		s.signalAccountChangedLocked(id)
	}
	s.mu.Unlock()
}

func (s *Store) startMediaFlightLocked(ctx context.Context, a Account) *mediaPreparationFlight {
	if s.mediaFlights == nil {
		s.mediaFlights = map[string]*mediaPreparationFlight{}
	}
	fingerprint, _ := mediaContextFingerprint(ctx, a)
	flight := &mediaPreparationFlight{done: make(chan struct{}), fingerprint: fingerprint}
	s.mediaFlights[a.ID] = flight
	s.maintenance[a.ID] = true
	delete(s.mediaReady, a.ID)
	return flight
}

func (s *Store) collectMediaSession(ctx context.Context, a Account, guard *mediaPreparationGuard, flight *mediaPreparationFlight, refresher func(context.Context, Account) (Account, error)) (result Account, err error) {
	result = a
	defer func() {
		s.mu.Lock()
		if err != nil {
			s.recordMediaFailureLocked(ctx, a, err)
		}
		flight.account, flight.err = result, err
		delete(s.mediaFlights, a.ID)
		delete(s.maintenance, a.ID)
		close(flight.done)
		s.signalAccountChangedLocked(a.ID)
		s.mu.Unlock()
	}()
	fresh, err := refresher(ctx, a)
	if ctx.Err() != nil {
		return a, &Error{Stage: "media_prepare", Code: "cancelled"}
	}
	if err != nil {
		var typed *Error
		if errors.As(err, &typed) {
			return a, typed
		}
		if errors.Is(err, ErrIdentity) {
			return a, ErrIdentity
		}
		return a, &Error{Stage: "media_prepare", Code: "browser_failed"}
	}
	updated, err := s.replaceCredentialsWithPreparation(a, fresh, true, guard)
	if err != nil {
		return a, err
	}
	return updated, nil
}

func (s *Store) recordMediaFailureLocked(ctx context.Context, expected Account, failure error) {
	if ready, ok := s.mediaReady[expected.ID]; ok && ready.Revision == expected.Revision {
		delete(s.mediaReady, expected.ID)
	}
	if ctx.Err() != nil || errors.Is(failure, ErrRevision) || errors.Is(failure, ErrBusy) || errors.Is(failure, ErrResolutionConflict) || errors.Is(failure, ErrTaskResolved) {
		return
	}
	var typed *Error
	if errors.As(failure, &typed) && (typed.Code == "context_changed" || typed.Code == "cancelled" || typed.Code == "account_disabled" || typed.Code == "submission_started" || typed.Code == "task_state_invalid") {
		return
	}
	d, err := s.load()
	if err != nil {
		return
	}
	fingerprint, selected := mediaContextFingerprint(ctx, expected)
	if !selected {
		return
	}
	for _, a := range d.Accounts {
		currentFingerprint, _ := mediaContextFingerprint(ctx, a)
		if a.ID != expected.ID || !a.Enabled || a.InFlight != expected.InFlight || a.Status != expected.Status || currentFingerprint != fingerprint {
			continue
		}
		backoff := 5 * time.Minute
		if errors.As(failure, &typed) {
			switch typed.Code {
			case "login_required", "verification_required", "email_verification_required":
				backoff = 15 * time.Minute
			}
		}
		if d.MediaFailures == nil {
			d.MediaFailures = map[string]mediaPreparationFailure{}
		}
		now := time.Now()
		d.MediaFailures[a.ID] = mediaPreparationFailure{Revision: a.Revision, Fingerprint: fingerprint, RetryAfter: now.Add(backoff).Unix(), Code: SafeError(failure), ObservedAt: now.Unix(), Diagnostic: MediaDiagnosticFromError(failure)}
		_ = s.save(d)
		return
	}
}

type mediaReservationCandidate struct {
	account Account
	ctx     context.Context
}

func (s *Store) mediaAccountSelectableLocked(d diskState, ctx context.Context, a Account, model string, now time.Time) bool {
	if strings.TrimSpace(a.ProfileID) == "" || !Eligible(a, now) || s.maintenance[a.ID] || s.quotaQueries[a.ID] || s.mediaBackoffLocked(d, ctx, a, now) {
		return false
	}
	if _, fixed := mediaContextFingerprint(ctx, a); !fixed {
		return false
	}
	return model == "" || a.Quota.Generation.ModelSelectionState(model, now) == ModelUsable
}

// MediaAccountSelectable is the same final admission check used by reservation,
// without creating a binding. Its context must contain the resolved daily exit.
func (s *Store) MediaAccountSelectable(ctx context.Context, expected Account, model string) bool {
	if ctx.Err() != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return false
	}
	before, fixed := mediaContextFingerprint(ctx, expected)
	if !fixed {
		return false
	}
	for _, current := range d.Accounts {
		if current.ID != expected.ID {
			continue
		}
		after, _ := mediaContextFingerprint(ctx, current)
		return before == after && s.mediaAccountSelectableLocked(d, ctx, current, model, time.Now())
	}
	return false
}

// ReserveMediaForModel prefers ready accounts before LRU, and may wait for one
// in-progress warmup before binding. After binding it never switches accounts.
// Resolver must pin the actual daily egress, including an explicit direct exit.
func (s *Store) ReserveMediaForModel(ctx context.Context, taskKey, model string, resolver func(context.Context, Account) (context.Context, error)) (Account, context.Context, error) {
	if taskKey == "" || resolver == nil {
		return Account{}, ctx, &Error{Stage: "media_prepare", Code: "selection_invalid"}
	}
	waited, reloaded := false, false
	for {
		if ctx.Err() != nil {
			return Account{}, ctx, &Error{Stage: "media_prepare", Code: "cancelled"}
		}
		s.mu.Lock()
		d, err := s.load()
		refreshing := map[string]bool{}
		for _, a := range d.Accounts {
			refreshing[a.ID] = s.maintenance[a.ID] || s.quotaQueries[a.ID]
		}
		s.mu.Unlock()
		if err != nil {
			return Account{}, ctx, err
		}
		if _, exists := d.Bindings[taskKey]; exists {
			return Account{}, ctx, errors.New("adobe_task_already_selected")
		}
		candidates := []mediaReservationCandidate{}
		var routingErr error
		for _, a := range d.Accounts {
			state := a.Quota.Generation.ModelSelectionState(model, time.Now())
			if strings.TrimSpace(a.ProfileID) == "" || !Eligible(a, time.Now()) || (model != "" && state != ModelUsable && !(state == ModelNeedsRefresh && refreshing[a.ID] && !waited)) {
				continue
			}
			fixed, resolveErr := resolver(ctx, a)
			if resolveErr != nil {
				if routingErr == nil {
					routingErr = resolveErr
				}
				continue
			}
			if _, selected := mediaContextFingerprint(fixed, a); selected {
				candidates = append(candidates, mediaReservationCandidate{account: a, ctx: fixed})
			} else if routingErr == nil {
				routingErr = &Error{Stage: "media_prepare", Code: "egress_required"}
			}
		}
		s.mu.Lock()
		d, err = s.load()
		if err != nil {
			s.mu.Unlock()
			return Account{}, ctx, err
		}
		if _, exists := d.Bindings[taskKey]; exists {
			s.mu.Unlock()
			return Account{}, ctx, errors.New("adobe_task_already_selected")
		}
		now := time.Now()
		selected, selectedReady := -1, false
		var selectedCtx context.Context
		var waitFor <-chan struct{}
		changed := false
		for _, candidate := range candidates {
			for i, current := range d.Accounts {
				if current.ID != candidate.account.ID {
					continue
				}
				if current.QuotaRevision != candidate.account.QuotaRevision || current.Revision != candidate.account.Revision {
					changed = true
					continue
				}
				if strings.TrimSpace(current.ProfileID) == "" || !Eligible(current, now) {
					continue
				}
				if model != "" && current.Quota.Generation.ModelSelectionState(model, now) != ModelUsable {
					if !waited && current.Quota.Generation.ModelSelectionState(model, now) == ModelNeedsRefresh && (s.maintenance[current.ID] || s.quotaQueries[current.ID]) && waitFor == nil {
						waitFor = s.accountChangeSignalLocked(current.ID)
					}
					continue
				}
				before, _ := mediaContextFingerprint(candidate.ctx, candidate.account)
				after, _ := mediaContextFingerprint(candidate.ctx, current)
				if before != after {
					changed = true
					continue
				}
				if !waited && s.mediaFlights[current.ID] != nil {
					if waitFor == nil {
						waitFor = s.mediaFlights[current.ID].done
					}
					continue
				}
				if s.maintenance[current.ID] || s.quotaQueries[current.ID] {
					if !waited && waitFor == nil {
						waitFor = s.accountChangeSignalLocked(current.ID)
					}
					continue
				}
				if !s.mediaAccountSelectableLocked(d, candidate.ctx, current, model, now) {
					continue
				}
				ready := s.mediaReadyLocked(candidate.ctx, current, 0, now)
				if selected < 0 || ready && !selectedReady || ready == selectedReady && current.LastUsedAt < d.Accounts[selected].LastUsedAt {
					selected, selectedReady, selectedCtx = i, ready, candidate.ctx
				}
			}
		}
		if !selectedReady && waitFor != nil && !waited {
			s.mu.Unlock()
			if err := waitMediaSignal(ctx, waitFor); err != nil {
				return Account{}, ctx, err
			}
			waited = true
			continue
		}
		if !selectedReady && changed && !reloaded {
			s.mu.Unlock()
			reloaded = true
			continue
		}
		if selected < 0 {
			s.mu.Unlock()
			if routingErr != nil {
				return Account{}, ctx, routingErr
			}
			return Account{}, ctx, ErrNoAccount
		}
		if ctx.Err() != nil {
			s.mu.Unlock()
			return Account{}, ctx, &Error{Stage: "media_prepare", Code: "cancelled"}
		}
		a := d.Accounts[selected]
		a.InFlight, a.LastUsedAt, a.UpdatedAt = taskKey, now.Unix(), now.Unix()
		d.Accounts[selected], d.Bindings[taskKey] = a, a.ID
		err = s.save(d)
		s.mu.Unlock()
		if err != nil {
			return Account{}, ctx, err
		}
		return a, selectedCtx, nil
	}
}
