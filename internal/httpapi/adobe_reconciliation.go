package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

type adobeReconciliationTask struct {
	image *imageTaskState
	video *videoJob
	found bool
	busy  bool
}

// Caller holds imageTaskMu then videoMu, the same order used by manual review.
func (s *Server) adobeReconciliationTaskLocked(a adobe.Account) adobeReconciliationTask {
	result := adobeReconciliationTask{}
	if s.imageTaskLoadErr != nil || s.videoLoadErr != nil {
		result.busy = true
		return result
	}
	for _, task := range s.imageTasks {
		if task.Route == nil || task.Route.Channel != "adobe" || task.Route.JobKey != a.InFlight || task.Route.AccountID != a.ID {
			continue
		}
		if result.found || task.recovering || (task.Status != "error" && task.Status != "success") {
			result.busy = true
		}
		result.found, result.image = true, task
	}
	for _, task := range s.videoJobs {
		if task.Route == nil || task.Route.Channel != "adobe" || task.Route.JobKey != a.InFlight || task.Route.AccountID != a.ID {
			continue
		}
		if result.found || task.recovering || (task.Status != "failed" && task.Status != "completed") {
			result.busy = true
		}
		result.found, result.video = true, task
	}
	return result
}

// A persisted pre-submit checkpoint is stronger evidence than a missing job
// handle. Check it while holding both task locks, and only after the runner has
// stopped. In-memory changes or an unreadable checkpoint cannot prove that a
// generation was never submitted.
func (s *Server) adobeUnsubmittedTaskLocked(a adobe.Account, task adobeReconciliationTask) bool {
	if !task.found || task.busy {
		return false
	}
	validRoute := func(route *mediaRoute) bool {
		return route != nil && route.Channel == "adobe" && route.AccountID == a.ID && route.JobKey == a.InFlight &&
			(route.Stage == "selected" || route.Stage == "media_session_preparing")
	}
	if task.image != nil {
		current := task.image
		if current.Status != "error" || current.recovering || !validRoute(current.Route) {
			return false
		}
		raw, err := os.ReadFile(filepath.Join(s.imageTasksDir(), imageTaskKey(current.OwnerID, current.ID)+".json"))
		var saved imageTaskState
		return err == nil && json.Unmarshal(raw, &saved) == nil && saved.ID == current.ID && saved.OwnerID == current.OwnerID &&
			saved.Status == "error" && validRoute(saved.Route) && saved.Route.Stage == current.Route.Stage
	}
	if task.video != nil {
		current := task.video
		if current.Status != "failed" || current.recovering || !validRoute(current.Route) {
			return false
		}
		raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "video_tasks", videoJournalName(current.ID)))
		var saved videoCheckpoint
		return err == nil && json.Unmarshal(raw, &saved) == nil && saved.Job.ID == current.ID && saved.OwnerID == current.OwnerID &&
			saved.Record.CallID == current.ID && saved.Job.Status == "failed" && validRoute(saved.Route) && saved.Route.Stage == current.Route.Stage
	}
	return false
}

func (s *Server) adobeReconciliationAllowed() bool {
	if s.adobeStore == nil || s.adobeClient == nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "cfm-maintenance")); !os.IsNotExist(err) {
		return false
	}
	s.deployMu.Lock()
	allowed := !s.deployPaused && !s.deploySealed
	s.deployMu.Unlock()
	return allowed
}

// Reconciliation is task housekeeping, independent of credential-maintenance
// preferences. It only polls existing jobs or closes an expired local lease;
// it never calls a generation submission method or matches history by guesses.
func (s *Server) reconcileAdobeSubmissions(ctx context.Context, now time.Time) {
	if !s.adobeReconciliationAllowed() || ctx.Err() != nil {
		return
	}
	items, err := s.adobeStore.List()
	if err != nil {
		return
	}
	// Oldest checks first so a few persistently failing accounts cannot starve
	// later reservations. Store checkpoints also bound retries across restarts.
	sort.SliceStable(items, func(i, j int) bool {
		left, right := "", ""
		if items[i].TaskReconciliation != nil {
			left = items[i].TaskReconciliation.LastCheckedAt
		}
		if items[j].TaskReconciliation != nil {
			right = items[j].TaskReconciliation.LastCheckedAt
		}
		return left < right
	})
	processed := 0
	for _, a := range items {
		if ctx.Err() != nil || !s.adobeReconciliationAllowed() || processed >= 4 {
			return
		}
		if a.InFlight == "" {
			// Auto-release keeps the normal settlement fence. Once it ends,
			// refresh invalidated quota even when credential maintenance is off.
			checkedAt, _ := time.Parse(time.RFC3339Nano, a.Quota.CheckedAt)
			// A routing failure prevented both reads. An older generation error
			// must not bypass that shared, persisted five-minute cooldown.
			if a.Quota.Status == "error" && a.Quota.Error == "adobe_routing_proxy_unavailable" && !checkedAt.IsZero() && now.Sub(checkedAt) < 5*time.Minute {
				continue
			}
			quotaDue := a.Quota.Status == "stale" || (a.Quota.Status == "error" && (checkedAt.IsZero() || now.Sub(checkedAt) >= 5*time.Minute))
			if generation := a.Quota.Generation; generation != nil && generation.Status == "error" {
				checkedGeneration, _ := time.Parse(time.RFC3339Nano, generation.CheckedAt)
				quotaDue = quotaDue || checkedGeneration.IsZero() || now.Sub(checkedGeneration) >= 5*time.Minute
			}
			if a.SettlementUntil <= now.Unix() && quotaDue {
				records, readErr := s.adobeStore.SubmissionResolutions(a.ID)
				var releasedAt time.Time
				if len(records) > 0 {
					releasedAt, _ = time.Parse(time.RFC3339Nano, records[0].ResolvedAt)
				}
				if readErr == nil && len(records) > 0 && records[0].ReviewedBy == "system" && !releasedAt.IsZero() && releasedAt.Unix() >= a.LastUsedAt {
					_, _ = s.queryAdobeReconciliationQuota(ctx, a.ID)
					processed++
				}
			}
			continue
		}
		if s.reconcileAdobeSubmission(ctx, a, now) {
			processed++
		}
	}
}

func (s *Server) queryAdobeReconciliationQuota(ctx context.Context, id string) (adobe.Account, error) {
	a, err := s.queryAdobeAccountQuota(ctx, id)
	var upstream *adobe.Error
	if errors.As(err, &upstream) && upstream.Stage == "routing" && upstream.Code == "proxy_unavailable" {
		// No HTTP request occurred, so QueryQuota never had a chance to save
		// its failed observation. Persist only this fixed routing category.
		observed, recordErr := s.adobeStore.RecordQuotaQueryFailure(a, "proxy_unavailable", time.Now())
		if recordErr != nil {
			return observed, recordErr
		}
		return observed, err
	}
	return a, err
}

func (s *Server) reconcileAdobeSubmission(ctx context.Context, a adobe.Account, now time.Time) bool {
	started := time.Now()
	s.imageTaskMu.Lock()
	s.videoMu.Lock()
	task := s.adobeReconciliationTaskLocked(a)
	if task.busy || ctx.Err() != nil || !s.adobeReconciliationAllowed() {
		s.videoMu.Unlock()
		s.imageTaskMu.Unlock()
		return false
	}
	handle, handleErr := s.adobeStore.LoadJob(a.InFlight)
	if a.Status != "submission_unknown" && errors.Is(handleErr, os.ErrNotExist) {
		// Reserve precedes route persistence. Do not claim that short window
		// as a reconciliation: its new runner still needs to prepare normally.
		// A crashed preparation is releasable only with positive durable proof
		// of the terminal pre-submit phase, even if credentials were refreshed.
		resolved := false
		if s.adobeUnsubmittedTaskLocked(a, task) && ctx.Err() == nil && s.adobeReconciliationAllowed() {
			_, _, err := s.adobeStore.ResolveUnsubmittedMediaTask(a.InFlight, a.ID, a.Revision)
			resolved = err == nil
		}
		s.videoMu.Unlock()
		s.imageTaskMu.Unlock()
		return resolved
	}
	record, err := s.adobeStore.BeginSubmissionReconciliation(a.ID, a.InFlight, a.Revision, now)
	s.videoMu.Unlock()
	s.imageTaskMu.Unlock()
	if err != nil {
		return false
	}
	observation := adobe.SubmissionReconciliationObservation{
		Attempt: record.Attempts, State: "retry_wait", CheckedAt: now,
		NextCheckAt: now.Add(5 * time.Minute), QuotaStatus: "not_checked",
	}
	storeObservation := func() bool {
		return s.adobeStore.RecordSubmissionReconciliation(a.ID, a.InFlight, a.Revision, observation) == nil
	}
	if handleErr != nil && !errors.Is(handleErr, os.ErrNotExist) {
		observation.LastError = "handle_invalid"
		storeObservation()
		return true
	}
	if handleErr == nil && handle.PollURL != "" {
		// Resume helpers check the exact route again while taking their own
		// locks. They may only poll/download the persisted original request.
		if !s.adobeReconciliationAllowed() {
			return true
		}
		resumed := false
		if task.image != nil {
			resumed = s.resumeAdobeImageTask(task.image)
		}
		if task.video != nil {
			resumed = s.resumeAdobeVideoJob(task.video)
		}
		if resumed {
			observation.State = "polling"
		} else {
			observation.LastError = "task_record_missing"
		}
		storeObservation()
		return true
	}
	if a.Status != "submission_unknown" {
		observation.LastError = "check_failed"
		storeObservation()
		return true
	}
	// A quota observation is useful accounting evidence, not proof of no
	// generation or refund. The release decision is a local expiry policy.
	queryContext, cancelQuery := context.WithTimeout(ctx, 45*time.Second)
	_, quotaErr := s.queryAdobeReconciliationQuota(queryContext, a.ID)
	cancelQuery()
	if ctx.Err() != nil || !s.adobeReconciliationAllowed() {
		return true
	}
	observation.CheckedAt = now.Add(time.Since(started))
	observation.NextCheckAt = observation.CheckedAt.Add(5 * time.Minute)
	observation.QuotaStatus = "known"
	observation.State = "waiting"
	if quotaErr != nil {
		observation.QuotaStatus, observation.LastError = "error", "quota_query_failed"
	}
	releaseAfter, parseErr := time.Parse(time.RFC3339Nano, record.ReleaseAfter)
	if parseErr != nil {
		return true
	}
	if observation.CheckedAt.Before(releaseAfter) {
		observation.NextCheckAt = releaseAfter
	}
	if !storeObservation() || observation.CheckedAt.Before(releaseAfter) {
		return true
	}
	s.imageTaskMu.Lock()
	defer s.imageTaskMu.Unlock()
	s.videoMu.Lock()
	defer s.videoMu.Unlock()
	// A concurrent resume/login/task change invalidates the earlier read.
	latestTask := s.adobeReconciliationTaskLocked(a)
	if latestTask.busy || ctx.Err() != nil || !s.adobeReconciliationAllowed() {
		return true
	}
	state := "terminal"
	if !latestTask.found {
		state = "missing"
	}
	_, _, _ = s.adobeStore.AutoResolveSubmission(a.ID, a.InFlight, a.Revision, state, now.Add(time.Since(started)))
	return true
}
