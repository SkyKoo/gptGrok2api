package register

import (
	"context"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdobeAcceptedManualReregistrationContinuesItsSessionAfterRestart(t *testing.T) {
	for _, priorOutcome := range []string{"unknown", "accepted"} {
		t.Run(priorOutcome, func(t *testing.T) {
			for _, automatic := range []bool{false, true} {
				t.Run(map[bool]string{false: "manual_continue", true: "automatic_continue"}[automatic], func(t *testing.T) {
					parent := adobeReregisterFixture(t)
					parent.AdobeRegistration.CreationOutcome = priorOutcome
					parent.AutoRetry.Attempts = 0
					path := filepath.Join(t.TempDir(), "registration.enc.json")
					var signup, resume atomic.Int32
					profileID := ""
					factory := func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
						return &adobeCompensationMail{}, adobeCompensationFlow{run: func(_ context.Context, cfg AdobeConfig, progress Progress) (map[string]any, error) {
							if cfg.RecoveryMode == "retry_registration" {
								signup.Add(1)
								profileID = cfg.BrowserProfileID
								if err := cfg.creationProgress(adobeCreationObservation{Outcome: "accepted", HTTPStatus: 200}, true); err != nil {
									return nil, err
								}
							} else {
								resume.Add(1)
								if cfg.RecoveryMode != "resume_session" || cfg.BrowserProfileID != profileID || profileID == parent.ID {
									t.Error("continuation changed the accepted session or replayed creation")
								}
							}
							if err := progress("awaiting_verification"); err != nil {
								return nil, err
							}
							return nil, fail("mail", "timeout")
						}}, nil
					}
					engine := NewAdobeEngine(path, "fixture", factory, nil, nil)
					wantSignup := int32(1)
					if priorOutcome == "accepted" {
						// Old versions admitted this child. Preserve its durable
						// continuation without allowing a new Create under this version.
						legacy := seedAdobeLegacyManualAttempt(t, engine, parent, "accepted")
						profileID, wantSignup = legacy.ID, 0
					} else {
						seedAdobeCompensation(t, engine, parent)
						if err := engine.Reregister(adobeTestConfig(), parent.ID); err != nil {
							t.Fatal(err)
						}
						waitAdobe(t, engine)
					}
					engine = NewAdobeEngine(path, "fixture", factory, nil, nil)
					accepted := latestAdobe(t, engine, parent.ID)
					if !accepted.AdobeRegistration.ManualReregistration || accepted.AdobeRegistration.BrowserProfileID != accepted.ID || adobeCreationOutcome(accepted) != "accepted" {
						t.Fatal("accepted manual origin did not retain its durable authorization")
					}
					rows := engine.Snapshot()["jobs"].([]any)
					leaf := rows[len(rows)-1].(map[string]any)
					if leaf["can_resume_registration"] != true || leaf["can_reregister"] != false || leaf["can_retry_registration"] != false {
						t.Fatal("accepted manual attempt lost recovery or exposed fresh registration")
					}
					for attempt := 0; attempt < 2; attempt++ {
						current := latestAdobe(t, engine, parent.ID)
						if automatic {
							if ran, err := engine.RunCompensation(adobeTestConfig(), time.Now().Add(time.Duration(attempt+1)*24*time.Hour)); !ran || err != nil {
								t.Fatalf("accepted session could not continue automatically: ran=%v error=%v", ran, err)
							}
						} else {
							// Advance the persisted cooldown to model the administrator
							// acting after it expires; preserve the actual retry counter.
							if err := engine.change(current.ID, func(j *RegistrationJob) {
								j.AutoRetry.NotBeforeAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
							}); err != nil {
								t.Fatal(err)
							}
							if err := engine.RetryRegistration(adobeTestConfig(), current.ID); err != nil {
								t.Fatal(err)
							}
						}
						waitAdobe(t, engine)
						engine = NewAdobeEngine(path, "fixture", factory, nil, nil)
						original, _ := engine.getJob(parent.ID)
						if original.Error != parent.Error || !reflect.DeepEqual(original.AdobeRegistration, parent.AdobeRegistration) {
							t.Fatal("continuation changed the unknown ancestor's evidence")
						}
					}
					last := latestAdobe(t, engine, parent.ID)
					wantBudget := 0
					if automatic {
						wantBudget = 2
					}
					if signup.Load() != wantSignup || resume.Load() != 2 || last.AutoRetry.Attempts != wantBudget || last.AdobeRegistration.BrowserProfileID != profileID {
						t.Fatalf("wrong lifecycle: signup=%d resume=%d budget=%d", signup.Load(), resume.Load(), last.AutoRetry.Attempts)
					}
				})
			}
		})
	}
}

func TestAdobeAcceptedManualBranchDoesNotIgnoreOtherUnknownOrActiveTasks(t *testing.T) {
	for _, scenario := range []string{"unmarked", "other_unknown", "active_sibling", "broken_backlink", "wrong_profile", "not_leaf", "unknown_child", "different_root", "different_email", "cycle"} {
		t.Run(scenario, func(t *testing.T) {
			parent := adobeReregisterFixture(t)
			parent.AutoRetry.Attempts = 0
			child := cloneJob(parent)
			child.ID, child.ParentJobID, child.RootJobID = "manual-origin", parent.ID, parent.ID
			child.AdobeRegistration.BrowserProfileID, child.AdobeRegistration.CreationOutcome, child.AdobeRegistration.ManualReregistration = child.ID, "accepted", true
			child.Error = "mail: timeout"
			child.AutoRetry = &RegistrationCompensation{State: "waiting", NextAttemptAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}
			parent.RetryJobID = child.ID
			parent.AutoRetry.State = "superseded"
			jobs := []RegistrationJob{parent, child}
			switch scenario {
			case "unmarked":
				jobs[1].AdobeRegistration.ManualReregistration = false
			case "broken_backlink":
				jobs[0].RetryJobID = "another-child"
			case "wrong_profile":
				jobs[1].AdobeRegistration.BrowserProfileID = "other-profile"
			case "not_leaf":
				jobs[1].RetryJobID = "later-child"
			case "unknown_child":
				jobs[1].AdobeRegistration.CreationOutcome = "unknown"
			case "different_root":
				jobs[1].RootJobID = "other-root"
			case "different_email":
				jobs[0].Mailbox.Email = "other@example.test"
				// Keep an unknown task for this address so a broken ancestry
				// cannot accidentally grant the exception to an unrelated task.
				sibling := cloneJob(parent)
				sibling.ID, sibling.RetryJobID = "unrelated", ""
				jobs = append(jobs, sibling)
			case "cycle":
				jobs[0].ParentJobID, jobs[1].RetryJobID = child.ID, parent.ID
			default:
				sibling := cloneJob(parent)
				sibling.ID, sibling.RetryJobID = "sibling", ""
				if scenario == "active_sibling" {
					sibling.Status, sibling.AdobeRegistration.CreationOutcome = "running", "not_submitted"
				}
				jobs = append(jobs, sibling)
			}
			var calls atomic.Int32
			engine := NewAdobeEngine(filepath.Join(t.TempDir(), "registration.enc.json"), "fixture", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				calls.Add(1)
				return nil, nil, fail("test", "unexpected_browser_call")
			}, nil, nil)
			seedAdobeCompensation(t, engine, jobs...)
			if engine.RetryRegistration(adobeTestConfig(), child.ID) == nil {
				t.Fatal("unsafe branch was manually continued")
			}
			if ran, _ := engine.RunCompensation(adobeTestConfig(), time.Now().Add(time.Hour)); ran || calls.Load() != 0 {
				t.Fatal("unsafe branch was automatically continued")
			}
		})
	}
}

func TestAdobeUnacceptedManualAttemptDoesNotAutomaticallyReplayPriorCreation(t *testing.T) {
	for _, priorOutcome := range []string{"unknown", "accepted"} {
		for _, outcome := range []string{"unknown", "rejected"} {
			t.Run(priorOutcome+"/"+outcome, func(t *testing.T) {
				parent := adobeReregisterFixture(t)
				parent.AdobeRegistration.CreationOutcome = priorOutcome
				parent.AutoRetry.Attempts = 0
				var calls atomic.Int32
				factory := func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
					return &adobeCompensationMail{}, adobeCompensationFlow{run: func(_ context.Context, cfg AdobeConfig, _ Progress) (map[string]any, error) {
						calls.Add(1)
						if cfg.RecoveryMode != "retry_registration" || cfg.BrowserProfileID == parent.ID {
							t.Error("manual action reused an ancestor")
						}
						observation := adobeCreationObservation{Outcome: outcome}
						if outcome == "rejected" {
							observation.ErrorCode, observation.HTTPStatus = "captcha_required", 400
						}
						if err := cfg.creationProgress(observation, true); err != nil {
							return nil, err
						}
						return nil, fail("adobe", "worker_result_unknown")
					}}, nil
				}
				path := filepath.Join(t.TempDir(), "journal")
				engine := NewAdobeEngine(path, "fixture", factory, nil, nil)
				wantCalls := int32(1)
				if priorOutcome == "accepted" {
					seedAdobeLegacyManualAttempt(t, engine, parent, outcome)
					wantCalls = 0
				} else {
					seedAdobeCompensation(t, engine, parent)
					if err := engine.Reregister(adobeTestConfig(), parent.ID); err != nil {
						t.Fatal(err)
					}
					waitAdobe(t, engine)
				}
				engine = NewAdobeEngine(path, "fixture", factory, nil, nil)
				child := latestAdobe(t, engine, parent.ID)
				if adobeCreationOutcome(child) != outcome || child.AutoRetry.Attempts != 0 {
					t.Fatal("manual result or retry budget was rewritten")
				}
				if ran, err := engine.RunCompensation(adobeTestConfig(), time.Now().Add(48*time.Hour)); ran || err != nil || calls.Load() != wantCalls {
					t.Fatalf("new uncertain/refused attempt caused blind automatic replay: ran=%v error=%v calls=%d", ran, err, calls.Load())
				}
				if engine.Snapshot()["jobs"].([]any)[1].(map[string]any)["can_reregister"] != (priorOutcome != "accepted") {
					t.Fatal("manual creation capability ignored the accepted ancestor")
				}
			})
		}
	}
}

// Model the legacy persisted child directly: a regression test must not use a
// now-forbidden API call to manufacture history from an earlier release.
func seedAdobeLegacyManualAttempt(t *testing.T, engine *AdobeEngine, parent RegistrationJob, outcome string) RegistrationJob {
	t.Helper()
	child := cloneJob(parent)
	child.ID, child.ParentJobID, child.RootJobID, child.RetryJobID = "legacy-manual-origin", parent.ID, mailboxClaimJobID(parent), ""
	child.AdobeRegistration.BrowserProfileID = child.ID
	child.AdobeRegistration.ManualReregistration = true
	child.AdobeRegistration.CreationOutcome = outcome
	child.AdobeRegistration.CreationErrorCode, child.AdobeRegistration.CreationHTTPStatus = "", 0
	if outcome == "rejected" {
		child.AdobeRegistration.CreationErrorCode, child.AdobeRegistration.CreationHTTPStatus = "captcha_required", 400
	}
	child.Stage, child.Error = "awaiting_verification", "mail: timeout"
	child.AutoRetry = &RegistrationCompensation{State: "waiting", NextAttemptAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}
	parent.RetryJobID = child.ID
	parent.AutoRetry.State = "superseded"
	seedAdobeCompensation(t, engine, parent, child)
	return child
}
