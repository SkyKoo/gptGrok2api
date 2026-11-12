package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

func adobePreparationRecoveryFixture(t *testing.T, kind, stage string, prepared bool) (*Server, adobe.Account, adobeReconciliationTask) {
	t.Helper()
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir, cfg.DataDir = root, root
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "auth_keys.json")
	cfg.ImageDataDir = filepath.Join(root, "images")
	cfg.VideoDataDir = filepath.Join(root, "videos")
	cfg.QueuePath = filepath.Join(root, "queue.json")
	s := NewWithWorkers(cfg, false)
	s.PauseDeployment(false)
	adobeFixtureAccount(t, s)
	a, err := s.adobeStore.Reserve("preparation-crash")
	if err != nil {
		t.Fatal(err)
	}
	if prepared {
		fixed, contextErr := s.adobeAccountContext(context.Background(), a)
		if contextErr != nil {
			t.Fatal(contextErr)
		}
		a, err = s.adobeStore.PrepareMediaForTask(fixed, a.InFlight, a.ID, a.Revision)
		if err != nil {
			t.Fatal(err)
		}
	}
	route := &mediaRoute{Channel: "adobe", AccountID: a.ID, JobKey: a.InFlight, Stage: stage}
	task := adobeReconciliationTask{found: true}
	if kind == "image" {
		task.image = &imageTaskState{ID: "preparation-crash", OwnerID: "owner", Status: "running", Model: "gemini-3.1-flash-image", Mode: "generate", N: 1, Size: "1024x1024", Route: route}
		s.imageTasks[imageTaskKey(task.image.OwnerID, task.image.ID)] = task.image
	} else {
		task.video = &videoJob{ID: "video_preparation_crash", OwnerID: "owner", Status: "in_progress", Model: "veo-3.1-lite", Route: route, savedRecord: &monitorRecord{CallID: "video_preparation_crash"}}
		s.videoJobs[task.video.ID] = task.video
	}
	saveAdobePreparationRecoveryTask(t, s, task)
	return s, a, task
}

func saveAdobePreparationRecoveryTask(t *testing.T, s *Server, task adobeReconciliationTask) {
	t.Helper()
	var err error
	if task.image != nil {
		err = s.saveImageTaskLocked(task.image)
	} else {
		err = s.saveVideoCheckpointLocked(task.video)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func failAdobePreparationRecoveryTask(t *testing.T, s *Server, task adobeReconciliationTask) {
	t.Helper()
	if task.image != nil {
		task.image.Status, task.image.Error = "error", "original preparation interruption"
	} else {
		task.video.Status, task.video.Error = "failed", map[string]any{"code": "preparation_interrupted", "message": "original preparation interruption"}
	}
	saveAdobePreparationRecoveryTask(t, s, task)
}

func TestAdobePreparationCrashAutomaticallyReleasesOnlyUnsubmittedReservation(t *testing.T) {
	for _, kind := range []string{"image", "video"} {
		for _, state := range []struct {
			stage    string
			prepared bool
		}{{"selected", false}, {"media_session_preparing", false}, {"media_session_preparing", true}} {
			name := kind + "/" + state.stage
			if state.prepared {
				name += "/credentials_saved"
			}
			t.Run(name, func(t *testing.T) {
				s, before, _ := adobePreparationRecoveryFixture(t, kind, state.stage, state.prepared)
				fresh := NewWithWorkers(s.cfg, false)
				fresh.PauseDeployment(false)
				var calls atomic.Int32
				fresh.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
				fresh.adobeStore.SetBrowserRefresher(func(context.Context, adobe.Account) (adobe.Account, error) {
					t.Error("crash recovery started a browser")
					return adobe.Account{}, errors.New("unexpected browser")
				})
				fresh.recoverVideoTasks()
				task := fresh.adobeReconciliationTaskLocked(before)
				if !task.found || task.busy {
					t.Fatal("restart did not mark interrupted task terminal")
				}
				historyBefore, _ := json.Marshal([]any{task.image, task.video})
				fresh.reconcileAdobeSubmissions(context.Background(), time.Now())
				after, err := fresh.adobeStore.Get(before.ID)
				audits, auditErr := fresh.adobeStore.SubmissionResolutions(before.ID)
				if err != nil || auditErr != nil || after.InFlight != "" || after.Status != before.Status || after.SettlementUntil != before.SettlementUntil || after.QuotaRevision != before.QuotaRevision || after.Revision != before.Revision || after.Token != before.Token || after.Cookie != before.Cookie {
					t.Fatal("pre-submit recovery changed credentials/quota or left the reservation")
				}
				if len(audits) != 1 || audits[0].ReviewedBy != "system" || audits[0].Method != "automatic_before_submit" || audits[0].Outcome != "not_submitted" || audits[0].QuotaStatus != "not_checked" {
					t.Fatalf("wrong pre-submit recovery audit: %+v", audits)
				}
				historyAfter, _ := json.Marshal([]any{task.image, task.video})
				if string(historyAfter) != string(historyBefore) || calls.Load() != 0 {
					t.Fatal("recovery rewrote original task history or contacted upstream")
				}
				fresh.reconcileAdobeSubmissions(context.Background(), time.Now().Add(time.Minute))
				restarted := NewWithWorkers(s.cfg, false)
				restarted.PauseDeployment(false)
				restarted.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
				restarted.reconcileAdobeSubmissions(context.Background(), time.Now().Add(2*time.Minute))
				audits, _ = restarted.adobeStore.SubmissionResolutions(before.ID)
				if len(audits) != 1 || calls.Load() != 0 {
					t.Fatal("repeat/restart duplicated recovery work")
				}
				if _, err = restarted.adobeStore.Reserve(before.InFlight); err == nil || errors.Is(err, adobe.ErrNoAccount) {
					t.Fatal("recovery lost the original task's durable no-replay binding")
				}
			})
		}
	}
}

func TestAdobePreparationRecoveryRequiresTerminalDurableUnsubmittedEvidence(t *testing.T) {
	for _, kind := range []string{"image", "video"} {
		for _, scenario := range []string{"running", "recovering", "successful", "missing_task", "duplicate_route", "submit_started", "polling", "missing_checkpoint", "corrupt_checkpoint", "disk_submit_started", "wrong_account", "wrong_task", "stale_revision", "invalid_account", "unknown_error", "saved_empty_job", "saved_handle", "corrupt_job", "load_error", "paused", "maintenance", "cancelled"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				s, account, task := adobePreparationRecoveryFixture(t, kind, "media_session_preparing", false)
				failAdobePreparationRecoveryTask(t, s, task)
				route := task.image
				var selected *mediaRoute
				var checkpoint string
				if route != nil {
					selected = route.Route
					checkpoint = filepath.Join(s.imageTasksDir(), imageTaskKey(route.OwnerID, route.ID)+".json")
				} else {
					selected = task.video.Route
					checkpoint = filepath.Join(s.cfg.DataDir, "video_tasks", videoJournalName(task.video.ID))
				}
				ctx := context.Background()
				switch scenario {
				case "running":
					if task.image != nil {
						task.image.Status = "running"
					} else {
						task.video.Status = "in_progress"
					}
				case "recovering":
					if task.image != nil {
						task.image.recovering = true
					} else {
						task.video.recovering = true
					}
				case "successful":
					if task.image != nil {
						task.image.Status = "success"
					} else {
						task.video.Status = "completed"
					}
				case "missing_task":
					s.imageTasks, s.videoJobs = map[string]*imageTaskState{}, map[string]*videoJob{}
				case "duplicate_route":
					s.imageTasks["duplicate"] = &imageTaskState{ID: "duplicate", OwnerID: "owner", Status: "error", Route: selected}
				case "submit_started", "polling":
					selected.Stage = scenario
				case "missing_checkpoint":
					if err := os.Remove(checkpoint); err != nil {
						t.Fatal(err)
					}
				case "corrupt_checkpoint":
					if err := os.WriteFile(checkpoint, []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				case "disk_submit_started":
					selected.Stage = "submit_started"
					saveAdobePreparationRecoveryTask(t, s, task)
					selected.Stage = "media_session_preparing"
				case "wrong_account":
					selected.AccountID = "different-account"
				case "wrong_task":
					selected.JobKey = "different-task"
				case "stale_revision":
					account.Revision++
				case "invalid_account":
					editAdobeReconciliationFixture(t, s, account.ID, func(a *adobe.Account) { a.Status = "invalid" })
				case "unknown_error":
					editAdobeReconciliationFixture(t, s, account.ID, func(a *adobe.Account) { a.LastError = "adobe_submission_unknown" })
				case "saved_empty_job", "saved_handle":
					job := adobe.Job{Kind: kind, Model: "fixture", Status: "SUBMITTED"}
					if scenario == "saved_handle" {
						job.PollURL = "https://bks-epo8552.adobe.io/v2/jobs/result/fixture"
					}
					if err := s.adobeStore.SaveJob(account.InFlight, job); err != nil {
						t.Fatal(err)
					}
				case "corrupt_job":
					digest := sha256.Sum256([]byte(account.InFlight))
					path := filepath.Join(s.cfg.DataDir, "adobe_jobs", hex.EncodeToString(digest[:])+".json.enc")
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("invalid encrypted handle"), 0600); err != nil {
						t.Fatal(err)
					}
				case "load_error":
					s.imageTaskLoadErr = errors.New("fixture task load error")
				case "paused":
					s.PauseDeployment(true)
				case "maintenance":
					if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "cfm-maintenance"), []byte("fixture"), 0600); err != nil {
						t.Fatal(err)
					}
				case "cancelled":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				var calls atomic.Int32
				s.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
				s.reconcileAdobeSubmission(ctx, account, time.Now())
				after, err := s.adobeStore.Get(account.ID)
				audits, _ := s.adobeStore.SubmissionResolutions(account.ID)
				if err != nil || after.InFlight != account.InFlight || len(audits) != 0 || calls.Load() != 0 {
					t.Fatalf("unsafe preparation release: reservation=%q audits=%d requests=%d err=%v", after.InFlight, len(audits), calls.Load(), err)
				}
			})
		}
	}
}

func TestAdobePreparationRecoveryDoesNotClaimNewReservationOrMislabelUnknown(t *testing.T) {
	for _, scenario := range []string{"before_route", "running", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			s, account, task := adobePreparationRecoveryFixture(t, "image", "media_session_preparing", false)
			if scenario == "before_route" {
				delete(s.imageTasks, imageTaskKey(task.image.OwnerID, task.image.ID))
			}
			if scenario == "unknown" {
				failAdobePreparationRecoveryTask(t, s, task)
				if err := s.adobeStore.MarkUnknown(account.InFlight); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			s.adobeClient = adobeReconciliationQuotaClient(t, &calls, false)
			s.reconcileAdobeSubmissions(context.Background(), time.Now())
			after, _ := s.adobeStore.Get(account.ID)
			audits, _ := s.adobeStore.SubmissionResolutions(account.ID)
			if after.InFlight != account.InFlight || len(audits) != 0 {
				t.Fatal("new or unknown reservation was falsely resolved as unsubmitted")
			}
			if scenario == "unknown" {
				if after.Status != "submission_unknown" || after.TaskReconciliation == nil || calls.Load() != 2 {
					t.Fatal("unknown task skipped normal uncertain-result policy")
				}
				return
			}
			if after.TaskReconciliation != nil || calls.Load() != 0 {
				t.Fatal("automatic reconciliation claimed the new reservation")
			}
			fixed, err := s.adobeAccountContext(context.Background(), account)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.adobeStore.PrepareMediaForTask(fixed, account.InFlight, account.ID, account.Revision); err != nil {
				t.Fatalf("new task preparation blocked by reconciliation: %v", err)
			}
		})
	}
}
