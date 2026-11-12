package adobe

import (
	"context"
	"time"
)

// A short local backoff avoids repeating one failed entitlement observation for
// concurrent or immediately following requests. It never represents a zero quota.
const modelPermissionFailureBackoff = 30 * time.Second

type modelPermissionFailure struct {
	Fingerprint string
	RetryAfter  time.Time
}

func (s *Store) modelPermissionsCoolingLocked(ctx context.Context, a Account, now time.Time) bool {
	failure, ok := s.modelFailures[a.ID]
	fingerprint, fixed := mediaContextFingerprint(ctx, a)
	return ok && fixed && failure.Fingerprint == fingerprint && failure.RetryAfter.After(now)
}

// Called before releasing the reader's maintenance gate, after the observation
// was persisted. Stale credentials or changed egress cannot create a failure for
// a newer account context. No raw upstream response is retained.
func (s *Store) recordModelPermissionObservation(ctx context.Context, expected Account, observed GenerationEntitlements, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return
	}
	for _, a := range d.Accounts {
		if a.ID == expected.ID {
			s.recordModelPermissionObservationLocked(ctx, expected, a, observed, target)
			return
		}
	}
}

func (s *Store) recordModelPermissionObservationLocked(ctx context.Context, expected, current Account, observed GenerationEntitlements, target string) {
	fingerprint, fixed := mediaContextFingerprint(ctx, expected)
	currentFingerprint, _ := mediaContextFingerprint(ctx, current)
	if !fixed || current.ID != expected.ID || currentFingerprint != fingerprint || current.Quota.Generation == nil || current.Quota.Generation.CheckedAt != observed.CheckedAt {
		return
	}
	incomplete := observed.Status != "known" || target != "" && observed.ModelSelectionState(target, time.Now()) == ModelNeedsRefresh
	if !incomplete {
		delete(s.modelFailures, current.ID)
		return
	}
	if s.modelFailures == nil {
		s.modelFailures = map[string]modelPermissionFailure{}
	}
	s.modelFailures[current.ID] = modelPermissionFailure{Fingerprint: fingerprint, RetryAfter: time.Now().Add(modelPermissionFailureBackoff)}
}
