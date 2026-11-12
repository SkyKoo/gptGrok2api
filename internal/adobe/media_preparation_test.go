package adobe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

func mediaTestContext() context.Context { return proxyruntime.WithURL(context.Background(), "") }

func reservedMediaStore(t *testing.T) (*Store, Account) {
	t.Helper()
	s, a := readyStore(t)
	if err := s.update(a.ID, a.Revision, func(a *Account) { a.ProfileID = "fixture-browser-profile" }); err != nil {
		t.Fatal(err)
	}
	a, err := s.Reserve("new-media-task")
	if err != nil {
		t.Fatal(err)
	}
	return s, a
}

func freshMediaAccount(a Account) Account {
	a = verifiedBrowserAccount(a)
	a.Token += "-fresh"
	a.Cookie = "sherlockToken=fresh-runtime-session"
	a.MediaSessionARPMode, a.MediaSessionARPSource = "present", "spl_runtime"
	a.UserAgent = "fixture-new-browser"
	return a
}

func rewriteMediaTestState(t *testing.T, s *Store, mutate func(*diskState)) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	mutate(&d)
	if err := s.save(d); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareMediaForTaskCollectsOnceAndPreservesReservation(t *testing.T) {
	for _, mode := range []string{"present", "omitted"} {
		t.Run(mode, func(t *testing.T) {
			s, before := reservedMediaStore(t)
			calls := 0
			s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) {
				calls++
				if a.ID != before.ID || a.InFlight != before.InFlight || a.Revision != before.Revision || a.ProfileID != before.ProfileID {
					t.Fatal("browser received a different reservation")
				}
				a = freshMediaAccount(a)
				if mode == "omitted" {
					a.MediaSessionARPMode, a.Cookie = "omitted", ""
				}
				return a, nil
			})
			fresh, err := s.PrepareMediaForTask(mediaTestContext(), before.InFlight, before.ID, before.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || fresh.Revision != before.Revision+1 || fresh.QuotaRevision != before.QuotaRevision+1 || fresh.Token != before.Token+"-fresh" || fresh.MediaSessionStatus != "refreshed" || fresh.MediaSessionUpdatedAt == 0 || fresh.UserAgent != "fixture-new-browser" {
				t.Fatal("normal browser collection was not committed")
			}
			if fresh.Enabled != before.Enabled || fresh.InFlight != before.InFlight || fresh.Status != before.Status || fresh.ProfileID != before.ProfileID || fresh.SettlementUntil != before.SettlementUntil || !reflect.DeepEqual(fresh.Quota, before.Quota) {
				t.Fatal("media preparation changed reservation or quota")
			}
			if value, present, err := MediaSessionARP(fresh); err != nil || present != (mode == "present") || (mode == "omitted" && value != "") {
				t.Fatal("official runtime header decision was not preserved")
			}
			d, err := s.load()
			if err != nil || d.Bindings[before.InFlight] != before.ID || d.PreparedMediaTasks[before.InFlight] != fresh.Revision {
				t.Fatal("credential update and preparation marker were not saved together")
			}
			if _, err := s.LoadJob(before.InFlight); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("preparation invented a submission")
			}
			for _, candidate := range []*Store{s, NewStore(s.path, s.secret)} {
				candidate.SetBrowserRefresher(func(context.Context, Account) (Account, error) {
					t.Error("duplicate preparation reached the browser")
					return Account{}, nil
				})
				if _, err := candidate.PrepareMediaForTask(mediaTestContext(), before.InFlight, before.ID, fresh.Revision); SafeError(err) != "adobe_media_prepare_already_prepared" {
					t.Fatalf("duplicate or restarted task got %v", err)
				}
				if err := candidate.ReleaseUnsubmittedTask(before.InFlight, before.ID); SafeError(err) != "adobe_media_prepare_already_prepared" {
					t.Fatal("successful preparation released as unsubmitted")
				}
			}
		})
	}
}

func TestPrepareMediaForTaskNewTaskReusesTrustedRecentCollection(t *testing.T) {
	s, a := reservedMediaStore(t)
	calls := 0
	s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) {
		calls++
		return freshMediaAccount(a), nil
	})
	if _, err := s.PrepareMediaForTask(mediaTestContext(), a.InFlight, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(a.InFlight); err != nil {
		t.Fatal(err)
	}
	if err := s.update(a.ID, 0, func(a *Account) { a.SettlementUntil = 0 }); err != nil {
		t.Fatal(err)
	}
	next, err := s.Reserve("another-new-task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareMediaForTask(mediaTestContext(), next.InFlight, next.ID, next.Revision); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("trusted recent collection was not reused")
	}
}

func TestPrepareMediaForTaskRejectsOldOrUnboundWorkBeforeBrowser(t *testing.T) {
	cases := []struct {
		name string
		edit func(*testing.T, *Store, *Account)
		want string
	}{
		{"revision", func(t *testing.T, s *Store, a *Account) { a.Revision++ }, ErrRevision.Error()},
		{"missing account", func(t *testing.T, s *Store, a *Account) { a.ID = "absent" }, ErrNotFound.Error()},
		{"missing task", func(t *testing.T, s *Store, a *Account) { a.InFlight = "" }, ErrResolutionConflict.Error()},
		{"disabled", func(t *testing.T, s *Store, a *Account) { _ = s.SetEnabled([]string{a.ID}, false) }, "adobe_media_prepare_account_disabled"},
		{"profile", func(t *testing.T, s *Store, a *Account) { _ = s.update(a.ID, 0, func(a *Account) { a.ProfileID = "" }) }, "adobe_media_prepare_profile_required"},
		{"unknown", func(t *testing.T, s *Store, a *Account) { _ = s.MarkUnknown(a.InFlight) }, ErrBusy.Error()},
		{"binding", func(t *testing.T, s *Store, a *Account) {
			rewriteMediaTestState(t, s, func(d *diskState) { d.Bindings[a.InFlight] = "other-account" })
		}, ErrResolutionConflict.Error()},
		{"saved job without handle", func(t *testing.T, s *Store, a *Account) {
			if err := s.SaveJob(a.InFlight, Job{Kind: "image", Status: "unknown"}); err != nil {
				t.Fatal(err)
			}
		}, "adobe_media_prepare_submission_started"},
		{"saved job with handle", func(t *testing.T, s *Store, a *Account) {
			if err := s.SaveJob(a.InFlight, Job{Kind: "video", PollURL: "https://example.invalid/result", Status: "RUNNING"}); err != nil {
				t.Fatal(err)
			}
		}, "adobe_media_prepare_submission_started"},
		{"corrupt job", func(t *testing.T, s *Store, a *Account) {
			if err := os.MkdirAll(filepath.Dir(s.jobPath(a.InFlight)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.jobPath(a.InFlight), []byte("broken encrypted state"), 0600); err != nil {
				t.Fatal(err)
			}
		}, "adobe_media_prepare_task_state_invalid"},
		{"resolution", func(t *testing.T, s *Store, a *Account) {
			rewriteMediaTestState(t, s, func(d *diskState) { d.Resolutions = map[string]SubmissionResolution{a.InFlight: {}} })
		}, ErrTaskResolved.Error()},
		{"reconciliation", func(t *testing.T, s *Store, a *Account) {
			rewriteMediaTestState(t, s, func(d *diskState) { d.Reconciliations = map[string]SubmissionReconciliation{a.InFlight: {}} })
		}, "adobe_media_prepare_submission_started"},
		{"quota query", func(t *testing.T, s *Store, a *Account) { s.quotaQueries[a.ID] = true }, ErrBusy.Error()},
		{"maintenance", func(t *testing.T, s *Store, a *Account) { s.maintenance[a.ID] = true }, ErrBusy.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a := reservedMediaStore(t)
			s.SetBrowserRefresher(func(context.Context, Account) (Account, error) {
				t.Error("unsafe task reached browser")
				return Account{}, nil
			})
			tc.edit(t, s, &a)
			before, _ := os.ReadFile(s.path)
			if _, err := s.PrepareMediaForTask(mediaTestContext(), a.InFlight, a.ID, a.Revision); SafeError(err) != tc.want {
				t.Fatalf("got %v want %s", err, tc.want)
			}
			after, _ := os.ReadFile(s.path)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected preparation modified account state")
			}
		})
	}
	s, a := reservedMediaStore(t)
	if _, err := s.PrepareMediaForTask(mediaTestContext(), a.InFlight, a.ID, a.Revision); SafeError(err) != "adobe_media_prepare_browser_unavailable" {
		t.Fatalf("missing browser got %v", err)
	}
}

func TestPrepareMediaForTaskFailuresPreserveSavedCredentials(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Account)
		err    error
		want   string
	}{
		{"incomplete", func(a *Account) { a.MediaSessionRefreshed = false }, nil, "adobe_media_prepare_media_session_incomplete"},
		{"retained", func(a *Account) { a.MediaSessionStatus = "retained_previous" }, nil, "adobe_media_prepare_media_session_incomplete"},
		{"missing arp", func(a *Account) { a.Cookie = "" }, nil, "adobe_media_prepare_media_session_incomplete"},
		{"invalid omission", func(a *Account) { a.MediaSessionARPMode = "omitted" }, nil, "adobe_media_prepare_media_session_incomplete"},
		{"token", func(a *Account) { a.Token = "bad" }, nil, "adobe_media_prepare_token_invalid"},
		{"identity", func(a *Account) { a.AccountID = "other" }, nil, ErrIdentity.Error()},
		{"email", func(a *Account) { a.Email = "other@example.test" }, nil, ErrIdentity.Error()},
		{"typed browser error", nil, &Error{Stage: "relogin", Code: "verification_required"}, "adobe_relogin_verification_required"},
		{"raw browser error", nil, errors.New("https://private.invalid/?token=secret"), "adobe_media_prepare_browser_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, a := reservedMediaStore(t)
			before, _ := os.ReadFile(s.path)
			s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) {
				a = freshMediaAccount(a)
				if tc.mutate != nil {
					tc.mutate(&a)
				}
				return a, tc.err
			})
			_, err := s.PrepareMediaForTask(mediaTestContext(), a.InFlight, a.ID, a.Revision)
			if SafeError(err) != tc.want {
				t.Fatalf("got %v want %s", err, tc.want)
			}
			after, _ := os.ReadFile(s.path)
			if bytes.Equal(before, after) {
				t.Fatal("browser failure did not persist independent backoff")
			}
			got, _ := s.Get(a.ID)
			if !reflect.DeepEqual(got, a) {
				t.Fatal("failed collection overwrote account credentials or state")
			}
			if err := s.ReleaseUnsubmittedTask(a.InFlight, a.ID); err != nil {
				t.Fatal("failed collection did not release maintenance", err)
			}
		})
	}
}

func TestPrepareMediaForTaskRejectsConcurrentChangesAtCommit(t *testing.T) {
	for _, mode := range []string{"revision", "disabled", "quota", "profile", "proxy", "identity", "email", "inflight", "unknown", "binding", "job", "delete", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, a := reservedMediaStore(t)
			ctx, cancel := context.WithCancel(mediaTestContext())
			defer cancel()
			var concurrent []byte
			s.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
				switch mode {
				case "cancel":
					cancel()
				case "disabled":
					if err := s.SetEnabled([]string{a.ID}, false); err != nil {
						t.Fatal(err)
					}
				case "job":
					if err := s.SaveJob(a.InFlight, Job{Kind: "image", Status: "unknown"}); err != nil {
						t.Fatal(err)
					}
				default:
					rewriteMediaTestState(t, s, func(d *diskState) {
						a := &d.Accounts[0]
						switch mode {
						case "revision":
							a.Revision++
							a.Cookie += "; newer-login=yes"
						case "quota":
							a.QuotaRevision++
							a.Quota.Status = "error"
						case "profile":
							a.ProfileID = "different-profile"
						case "proxy":
							a.Proxy = "http://different.invalid:8080"
						case "identity":
							a.AccountID = "other"
						case "email":
							a.Email = "other@example.test"
						case "inflight":
							a.InFlight = "another-task"
						case "unknown":
							a.Status = "submission_unknown"
						case "binding":
							d.Bindings[a.InFlight] = "other-account"
						case "delete":
							d.Accounts = nil
						}
					})
				}
				concurrent, _ = os.ReadFile(s.path)
				return freshMediaAccount(current), nil
			})
			if _, err := s.PrepareMediaForTask(ctx, a.InFlight, a.ID, a.Revision); err == nil {
				t.Fatal("concurrent change accepted stale browser result")
			}
			after, _ := os.ReadFile(s.path)
			if !bytes.Equal(concurrent, after) {
				t.Fatal("stale collection changed concurrent state")
			}
		})
	}
}

func TestPrepareMediaForTaskMaintenanceBlocksParallelAccountWork(t *testing.T) {
	s, a := reservedMediaStore(t)
	s.SetBrowserRefresher(func(ctx context.Context, current Account) (Account, error) {
		if _, err := s.PrepareMediaForTask(ctx, a.InFlight, a.ID, a.Revision); !errors.Is(err, ErrBusy) {
			t.Fatal("duplicate collection not blocked")
		}
		if _, err := s.Relogin(ctx, a.ID); !errors.Is(err, ErrBusy) {
			t.Fatal("parallel login not blocked")
		}
		if _, err := s.QueryQuota(ctx, NewClient(nil), a.ID); !errors.Is(err, ErrBusy) {
			t.Fatal("parallel quota read not blocked")
		}
		if _, err := s.Import([]Account{a}); !errors.Is(err, ErrBusy) {
			t.Fatal("parallel import not blocked")
		}
		if _, err := s.Delete([]string{a.ID}); !errors.Is(err, ErrBusy) {
			t.Fatal("parallel delete not blocked")
		}
		if err := s.ReleaseUnsubmittedTask(a.InFlight, a.ID); !errors.Is(err, ErrBusy) {
			t.Fatal("parallel release not blocked")
		}
		return freshMediaAccount(current), nil
	})
	if _, err := s.PrepareMediaForTask(mediaTestContext(), a.InFlight, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	if s.maintenance[a.ID] {
		t.Fatal("collection retained maintenance lock")
	}
}

func TestReleaseUnsubmittedTaskRetainsQuotaAndBinding(t *testing.T) {
	s, a := reservedMediaStore(t)
	if err := s.ReleaseUnsubmittedTask(a.InFlight, a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InFlight != "" || got.SettlementUntil != a.SettlementUntil || got.QuotaRevision != a.QuotaRevision || !reflect.DeepEqual(got.Quota, a.Quota) || got.Token != a.Token || got.Cookie != a.Cookie || got.Enabled != a.Enabled {
		t.Fatal("unsubmitted release changed account or charged settlement")
	}
	d, _ := s.load()
	if d.Bindings[a.InFlight] != a.ID {
		t.Fatal("unsubmitted release removed durable task binding")
	}
	if _, err := s.Reserve(a.InFlight); err == nil {
		t.Fatal("failed original task could replay")
	}
	if _, err := s.Reserve("new-independent-task"); err != nil {
		t.Fatal("new work cannot use safely released account", err)
	}
}

func TestReleaseUnsubmittedTaskRejectsSubmittedAndUnknownState(t *testing.T) {
	for _, mode := range []string{"job", "corrupt job", "unknown", "binding", "inflight", "resolution", "reconciliation"} {
		t.Run(mode, func(t *testing.T) {
			s, a := reservedMediaStore(t)
			switch mode {
			case "job":
				if err := s.SaveJob(a.InFlight, Job{Kind: "video", Status: "unknown"}); err != nil {
					t.Fatal(err)
				}
			case "corrupt job":
				if err := os.MkdirAll(filepath.Dir(s.jobPath(a.InFlight)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.jobPath(a.InFlight), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				rewriteMediaTestState(t, s, func(d *diskState) {
					switch mode {
					case "unknown":
						d.Accounts[0].Status = "submission_unknown"
					case "binding":
						d.Bindings[a.InFlight] = "other-account"
					case "inflight":
						d.Accounts[0].InFlight = "other-task"
					case "resolution":
						d.Resolutions = map[string]SubmissionResolution{a.InFlight: {}}
					case "reconciliation":
						d.Reconciliations = map[string]SubmissionReconciliation{a.InFlight: {}}
					}
				})
			}
			before, _ := os.ReadFile(s.path)
			if err := s.ReleaseUnsubmittedTask(a.InFlight, a.ID); err == nil {
				t.Fatal("unsafe state released")
			}
			after, _ := os.ReadFile(s.path)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected release modified stored state")
			}
		})
	}
}

func TestPrepareMediaForTaskPreCancelledDoesNotCollect(t *testing.T) {
	s, a := reservedMediaStore(t)
	ctx, cancel := context.WithCancel(mediaTestContext())
	cancel()
	s.SetBrowserRefresher(func(context.Context, Account) (Account, error) {
		t.Error("cancelled call reached browser")
		return Account{}, nil
	})
	before, _ := os.ReadFile(s.path)
	if _, err := s.PrepareMediaForTask(ctx, a.InFlight, a.ID, a.Revision); SafeError(err) != "adobe_media_prepare_cancelled" {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(s.path)
	if !bytes.Equal(before, after) {
		t.Fatal("cancelled call wrote credentials")
	}
}

func TestPrepareMediaTaskMarkerRemainsEncryptedAndPrivate(t *testing.T) {
	s, a := reservedMediaStore(t)
	s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) { return freshMediaAccount(a), nil })
	if _, err := s.PrepareMediaForTask(mediaTestContext(), a.InFlight, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.path)
	for _, secret := range []string{a.InFlight, "fresh-runtime-session", "media_prepared_tasks"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("preparation state leaked outside encrypted store")
		}
	}
	fresh, _ := s.Get(a.ID)
	if _, leaked := Public(fresh)["media_prepared_tasks"]; leaked {
		t.Fatal("private preparation marker exposed publicly")
	}
	if fresh.LastRefreshAt < time.Now().Add(-time.Minute).Unix() {
		t.Fatal("preparation did not record successful collection")
	}
}

func TestResolveUnsubmittedMediaTaskAuditsCrashedPreparation(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(map[bool]string{false: "before collection", true: "collected before crash"}[prepared], func(t *testing.T) {
			s, a := reservedMediaStore(t)
			key := a.InFlight
			if prepared {
				s.SetBrowserRefresher(func(ctx context.Context, a Account) (Account, error) { return freshMediaAccount(a), nil })
				var err error
				a, err = s.PrepareMediaForTask(mediaTestContext(), key, a.ID, a.Revision)
				if err != nil {
					t.Fatal(err)
				}
			}
			// The task coordinator has established a durable pre-submit route
			// after restart. Store resolution preserves the disabled preference.
			if err := s.SetEnabled([]string{a.ID}, false); err != nil {
				t.Fatal(err)
			}
			a.Enabled = false
			s = NewStore(s.path, s.secret)
			resolved, audit, err := s.ResolveUnsubmittedMediaTask(key, a.ID, a.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.InFlight != "" || resolved.Enabled || resolved.Revision != a.Revision || resolved.QuotaRevision != a.QuotaRevision || resolved.Token != a.Token || resolved.Cookie != a.Cookie || resolved.Status != a.Status || resolved.SettlementUntil != a.SettlementUntil || !reflect.DeepEqual(resolved.Quota, a.Quota) {
				t.Fatal("crash recovery changed fields other than reservation")
			}
			if audit.TaskKey != key || audit.AccountID != a.ID || audit.CredentialRevision != a.Revision || audit.ReviewedBy != "system" || audit.Method != "automatic_before_submit" || audit.Outcome != "not_submitted" || audit.QuotaStatus != "not_checked" || audit.TaskRecordState != "terminal" {
				t.Fatal("unsubmitted recovery audit missing")
			}
			d, err := s.load()
			if err != nil || d.Bindings[key] != a.ID || d.Resolutions[key] != audit {
				t.Fatal("audit and binding were not retained")
			}
			_, hasPreparation := d.PreparedMediaTasks[key]
			if hasPreparation != prepared {
				t.Fatal("recovery removed preparation marker")
			}
			if _, _, err := s.ResolveUnsubmittedMediaTask(key, a.ID, a.Revision); !errors.Is(err, ErrTaskResolved) {
				t.Fatal("duplicate recovery overwrote audit")
			}
			if err := s.SaveJob(key, Job{Kind: "image"}); !errors.Is(err, ErrTaskResolved) {
				t.Fatal("resolved pre-submit task accepted a submission")
			}
			if _, err := s.Reserve(key); err == nil {
				t.Fatal("resolved task was replayable")
			}
		})
	}
}

func TestResolveUnsubmittedMediaTaskRejectsUncertainOrChangedState(t *testing.T) {
	for _, mode := range []string{"revision", "binding", "inflight", "unknown", "invalid", "historical unknown", "job", "corrupt job", "maintenance", "quota query", "reconciliation"} {
		t.Run(mode, func(t *testing.T) {
			s, a := reservedMediaStore(t)
			switch mode {
			case "revision":
				a.Revision++
			case "job":
				if err := s.SaveJob(a.InFlight, Job{Kind: "image", Status: "unknown"}); err != nil {
					t.Fatal(err)
				}
			case "corrupt job":
				if err := os.MkdirAll(filepath.Dir(s.jobPath(a.InFlight)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s.jobPath(a.InFlight), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "maintenance":
				s.maintenance[a.ID] = true
			case "quota query":
				s.quotaQueries[a.ID] = true
			default:
				rewriteMediaTestState(t, s, func(d *diskState) {
					switch mode {
					case "binding":
						d.Bindings[a.InFlight] = "other-account"
					case "inflight":
						d.Accounts[0].InFlight = "other-task"
					case "unknown":
						d.Accounts[0].Status = "submission_unknown"
					case "invalid":
						d.Accounts[0].Status = "invalid"
					case "historical unknown":
						d.Accounts[0].LastError = "adobe_submission_unknown"
					case "reconciliation":
						d.Reconciliations = map[string]SubmissionReconciliation{a.InFlight: {}}
					}
				})
			}
			before, _ := os.ReadFile(s.path)
			if _, _, err := s.ResolveUnsubmittedMediaTask(a.InFlight, a.ID, a.Revision); err == nil {
				t.Fatal("uncertain reservation released as unsubmitted")
			}
			after, _ := os.ReadFile(s.path)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected recovery changed stored state")
			}
		})
	}
}
