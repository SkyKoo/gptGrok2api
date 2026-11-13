package register

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

func TestAdobeMediaHandoffPersistsBeforeClosingAndUsesDailyRoute(t *testing.T) {
	for _, scenario := range []string{"success", "media_failure", "journal_failure", "routing_failure", "close_failure", "invalid_pending", "otp"} {
		t.Run(scenario, func(t *testing.T) {
			var events []string
			var mu sync.Mutex
			add := func(event string) { mu.Lock(); events = append(events, event); mu.Unlock() }
			mediaLease := ""
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					if strings.HasSuffix(r.URL.Path, mediaLease) && mediaLease != "" {
						add("close_media")
						json.NewEncoder(w).Encode(map[string]any{"status": "released"})
					} else {
						add("close_registration")
						json.NewEncoder(w).Encode(map[string]any{"status": "closing"})
					}
					return
				}
				if r.Method == http.MethodGet {
					add("confirm_close")
					if scenario == "close_failure" {
						w.WriteHeader(http.StatusForbidden)
					} else {
						w.WriteHeader(http.StatusNotFound)
					}
					return
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if r.URL.Path == "/v1/browser/reservations" {
					mode := stringValue(body["adobe_mode"])
					add("reserve_" + mode)
					if body["profile_id"] != "profile-original" || body["email"] != "existing@example.test" {
						t.Error("handoff changed identity or profile")
					}
					if mode == "registration" && (body["proxy"] != "http://registration.invalid:8080" || body["adobe_defer_media"] != true) {
						t.Error("registration route or handoff contract missing")
					}
					if mode == "media" {
						mediaLease = stringValue(body["id"])
						if body["proxy"] != "socks5://daily.invalid:1080" || body["password"] != nil || body["adobe_legacy_media"] != nil || body["adobe_require_new"] != nil {
							t.Error("media lease changed route or allows a new signup/login")
						}
					}
					json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
					return
				}
				action := stringValue(body["action"])
				add(action)
				if action == "run" && scenario == "otp" {
					json.NewEncoder(w).Encode(map[string]any{"stage": "awaiting_verification", "creation_outcome": "accepted", "code": "verification_required"})
					return
				}
				if action == "run" || action == "verify" {
					pending := map[string]any{"stage": "media_pending", "creation_outcome": "accepted"}
					if scenario == "invalid_pending" {
						pending["code"] = "media_session_incomplete"
					}
					json.NewEncoder(w).Encode(pending)
					return
				}
				if action != "media" {
					t.Error("unexpected action")
				}
				if scenario == "media_failure" {
					json.NewEncoder(w).Encode(map[string]any{"stage": "media_pending", "creation_outcome": "accepted", "code": "media_session_incomplete"})
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"stage": "completed", "creation_outcome": "accepted", "account": map[string]any{
					"email": "existing@example.test", "account_id": "identity", "token": "fixture", "profile_id": "profile-original"}})
			}))
			defer worker.Close()
			flow := AdobeBrowserFlow{Client: worker.Client(), ResolveMediaProxy: func(_ context.Context, email, profile string) (string, error) {
				add("resolve_daily")
				if scenario == "routing_failure" {
					return "", errors.New("private proxy URL")
				}
				return "socks5://daily.invalid:1080", nil
			}}
			cfg := AdobeConfig{WorkerURL: worker.URL, BrowserProfileID: "profile-original", Proxy: "http://registration.invalid:8080",
				mediaProgress: func() error {
					add("persist_pending")
					if scenario == "journal_failure" {
						return fail("storage", "journal_write_failed")
					}
					return nil
				}}
			account, err := flow.Register(context.Background(), "task", Mailbox{Email: "existing@example.test"}, cfg,
				func(context.Context, time.Time) (string, error) { add("mail_code"); return "123456", nil }, func(string) error { return nil })
			flow.Close()
			want := []string{"reserve_registration", "run"}
			if scenario == "otp" {
				want = append(want, "mail_code", "verify")
			}
			if scenario != "invalid_pending" {
				want = append(want, "persist_pending")
			}
			want = append(want, "close_registration")
			if scenario != "invalid_pending" && scenario != "journal_failure" {
				want = append(want, "confirm_close")
				if scenario == "close_failure" {
					want = append(want, "close_registration")
				} else {
					want = append(want, "resolve_daily")
					if scenario != "routing_failure" {
						want = append(want, "reserve_media", "media", "close_media")
					}
				}
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("handoff ordering changed: got %v want %v", events, want)
			}
			if scenario == "success" || scenario == "otp" {
				if err != nil || account["account_id"] != "identity" {
					t.Fatalf("handoff failed: %v", err)
				}
			} else if err == nil {
				t.Fatal("failure became a completed registration")
			}
			if scenario == "routing_failure" && safeFailure(err) != "adobe: media_proxy_unavailable" {
				t.Fatal("routing error leaked or fell back")
			}
		})
	}
}

func TestAdobeMediaRecoveryBypassesBrokenRegistrationBindingAndPersistsAcrossRestart(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkpoint", true: "legacy"}[legacy], func(t *testing.T) {
			now := time.Now()
			job := adobeCompensationFixture(t, now)
			job.AdobeRegistration.Proxy = "http://registration.invalid:8080"
			job.AdobeRegistration.MediaPending = !legacy
			job.Error = "adobe: media_session_incomplete"
			job.AutoRetry.Attempts = 1
			path := filepath.Join(t.TempDir(), "journal.enc")
			var reservations, commands int
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					json.NewEncoder(w).Encode(map[string]any{"status": "released"})
					return
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if r.URL.Path == "/v1/browser/reservations" {
					reservations++
					if body["adobe_mode"] != "media" || body["profile_id"] != job.ID || body["proxy"] != "http://daily.invalid:8080" || (body["adobe_legacy_media"] == true) != legacy {
						t.Error("recovery lost journal handoff or selected the registration route")
					}
					json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
					return
				}
				commands++
				if body["action"] != "media" || body["code"] != "" {
					t.Error("media recovery attempted registration or OTP")
				}
				json.NewEncoder(w).Encode(map[string]any{"stage": "media_pending", "creation_outcome": "accepted", "code": "media_session_incomplete"})
			}))
			defer worker.Close()
			e := NewAdobeEngine(path, "key", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				t.Error("recovery reopened registration/HME factory")
				return nil, nil, errors.New("unexpected")
			}, nil, nil)
			routing := routingFixture()
			lease, err := routing.Manager.AcquireTask(context.Background(), "group:shared", "adobe", "registration", nil, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			job.ProxyBinding = &lease.Binding
			job.AdobeRegistration.Proxy = lease.URL
			lease.Release()
			routing.Manager.ConfigureTaskGroups(nil) // Original node is no longer available.
			e.SetProxyRouting(routing)
			e.SetMediaFlowFactory(func(AdobeConfig) AdobeRegistrationFlow {
				return &AdobeBrowserFlow{Client: worker.Client(), ResolveMediaProxy: func(context.Context, string, string) (string, error) { return "http://daily.invalid:8080", nil }}
			})
			seedAdobeCompensation(t, e, job)
			cfg := adobeTestConfig()
			cfg["adobe"] = map[string]any{"api_base": worker.URL}
			if started, err := e.RunCompensation(cfg, now); err != nil || !started {
				t.Fatalf("recovery did not start: %v", err)
			}
			waitAdobe(t, e)
			child := latestAdobe(t, e, job.ID)
			if reservations != 1 || commands != 1 || child.Error != "adobe: media_session_incomplete" || child.AutoRetry.Attempts != 2 || !child.AdobeRegistration.MediaPending || child.AdobeRegistration.LegacyMedia != legacy || adobeRegistrationRecoveryMode(child) != "resume_session" || len(child.Account) != 0 {
				t.Fatalf("wrong durable media recovery result: %+v", child)
			}
			reloaded := NewAdobeEngine(path, "key", nil, nil, nil)
			saved, _ := reloaded.getJob(child.ID)
			if !adobeMediaPreparationPending(saved) || saved.AdobeRegistration.LegacyMedia != legacy || saved.AutoRetry.Attempts != 2 {
				t.Fatal("restart lost checkpoint, exact legacy evidence or retry budget")
			}
		})
	}
}

func TestAdobeMediaRecoveryRequiresAcceptedExactInternalEvidence(t *testing.T) {
	for _, outcome := range []string{"accepted", "unknown", "rejected", "not_submitted"} {
		for _, reason := range []string{"adobe: media_session_incomplete", "adobe: submission_result_unknown", "media_session_incomplete", "adobe: media_session_incomplete suffix"} {
			job := adobeCompensationFixture(t, time.Now())
			job.AdobeRegistration.CreationOutcome = outcome
			job.Error = reason
			if adobeMediaPreparationPending(job) != (outcome == "accepted" && reason == "adobe: media_session_incomplete") {
				t.Fatal("inexact or uncertain evidence admitted media handoff")
			}
		}
	}
}

func TestAdobeMediaPreparationTransientFailuresKeepBoundedMediaRecovery(t *testing.T) {
	now := time.Now().UTC()
	for _, code := range []string{"media_prepare_timeout", "media_prepare_failed"} {
		t.Run(code, func(t *testing.T) {
			for _, outcome := range []string{"accepted", "unknown", "rejected", "not_submitted"} {
				for _, pending := range []bool{false, true} {
					job := adobeCompensationFixture(t, now)
					job.Error = "adobe: " + code
					job.AdobeRegistration.CreationOutcome = outcome
					job.AdobeRegistration.MediaPending = pending
					want := outcome == "accepted" && pending
					if adobeAutomaticFailure(job) != want {
						t.Fatalf("media failure recovery bypassed durable identity: outcome=%s pending=%v", outcome, pending)
					}
					planAdobeRegistrationCompensation(&job, fail("adobe", code), now)
					if (job.AutoRetry.State == "waiting") != want {
						t.Fatalf("incorrect media failure plan: outcome=%s pending=%v state=%s", outcome, pending, job.AutoRetry.State)
					}
				}
			}
			for attempt := 0; attempt <= len(compensationDelays); attempt++ {
				job := adobeCompensationFixture(t, now)
				job.Error = "adobe: " + code
				job.AdobeRegistration.MediaPending = true
				job.AutoRetry.Attempts = attempt
				planAdobeRegistrationCompensation(&job, fail("adobe", code), now)
				if job.AutoRetry.Attempts != attempt || !adobeMediaPreparationPending(job) || adobeRegistrationRecoveryMode(job) != "resume_session" {
					t.Fatal("media failure changed budget or became a new registration")
				}
				if attempt == len(compensationDelays) {
					if job.AutoRetry.State != "exhausted" || job.AutoRetry.NextAttemptAt != "" {
						t.Fatal("media failure reset exhausted budget")
					}
				} else if job.AutoRetry.State != "waiting" || job.AutoRetry.NextAttemptAt != now.Add(compensationDelays[attempt]).Format(time.RFC3339Nano) {
					t.Fatal("media failure bypassed existing retry delay")
				}
			}
		})
	}
	for _, code := range []string{"media_identity_mismatch", "media_email_unverified", "media_login_required", "media_checkpoint_invalid", "media_page_cleanup_failed", "media_state_write_failed"} {
		job := adobeCompensationFixture(t, now)
		job.Error = "adobe: " + code
		job.AdobeRegistration.MediaPending = true
		planAdobeRegistrationCompensation(&job, fail("adobe", code), now)
		if job.AutoRetry.State != "manual" || job.AutoRetry.Reason != "failure_requires_review" {
			t.Fatalf("non-transient media failure became automatic: %s", code)
		}
	}
}

func TestAdobeMediaLegacyInspectionDoesNotMutatePausedJournalOrRetryBudget(t *testing.T) {
	job := adobeCompensationFixture(t, time.Now())
	job.Error = "adobe: media_session_incomplete"
	job.AutoRetry.State, job.AutoRetry.Attempts = "exhausted", len(compensationDelays)
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	e := NewAdobeEngine(path, "key", nil, nil, nil)
	seedAdobeCompensation(t, e, job)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	e = NewAdobeEngine(path, "key", nil, nil, nil)
	e.SetMediaFlowFactory(func(AdobeConfig) AdobeRegistrationFlow { t.Fatal("paused journal launched a browser"); return nil })
	_ = e.Snapshot()
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("startup or read-only snapshot rewrote legacy journal")
	}
	saved, _ := e.getJob(job.ID)
	if saved.AdobeRegistration.MediaPending || saved.AdobeRegistration.LegacyMedia || saved.AutoRetry.State != "exhausted" || saved.AutoRetry.Attempts != len(compensationDelays) {
		t.Fatal("inspection changed terminal budget or staged legacy migration")
	}
}

func TestAdobeFreshMediaCheckpointIsDurableBeforeSecondLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	var profile string
	mediaReservations := 0
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			json.NewEncoder(w).Encode(map[string]any{"status": "released"})
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/v1/browser/reservations" {
			if body["adobe_mode"] == "registration" {
				profile = stringValue(body["profile_id"])
			}
			if body["adobe_mode"] == "media" {
				mediaReservations++
				raw, err := os.ReadFile(path)
				var envelope struct {
					Data []byte `json:"data"`
				}
				if err != nil || json.Unmarshal(raw, &envelope) != nil {
					t.Error("pending journal missing")
					w.WriteHeader(500)
					return
				}
				plain, err := adobe.Open("key", envelope.Data)
				var jobs []RegistrationJob
				if err != nil || json.Unmarshal(plain, &jobs) != nil || len(jobs) != 1 ||
					jobs[0].AdobeRegistration == nil || !jobs[0].AdobeRegistration.MediaPending ||
					jobs[0].AdobeRegistration.LegacyMedia || adobeCreationOutcome(jobs[0]) != "accepted" || len(jobs[0].Account) != 0 {
					t.Error("second lease preceded durable identity handoff or prematurely imported partial credentials")
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
			return
		}
		switch body["action"] {
		case "prepare":
			json.NewEncoder(w).Encode(map[string]any{"stage": "prepared", "registration_country": map[string]any{"country": "US", "source": "preserved"}})
		case "run":
			json.NewEncoder(w).Encode(map[string]any{"stage": "media_pending", "creation_outcome": "accepted"})
		case "media":
			json.NewEncoder(w).Encode(map[string]any{"stage": "completed", "creation_outcome": "accepted", "account": map[string]any{"email": "nod@example.test", "account_id": "identity", "token": "PRIVATE_FIXTURE", "profile_id": profile}})
		default:
			t.Error("unexpected action")
			w.WriteHeader(400)
		}
	}))
	defer worker.Close()
	imports, verifies := 0, 0
	e := NewAdobeEngine(path, "key", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		return adobeTestMail{}, &AdobeBrowserFlow{Client: worker.Client(), ResolveMediaProxy: func(context.Context, string, string) (string, error) { return "http://daily.invalid:8080", nil }}, nil
	}, func(_ context.Context, _ string, fields map[string]any) error {
		imports++
		if fields["token"] != "PRIVATE_FIXTURE" || fields["proxy_policy"] != "inherit" {
			t.Error("full account import changed")
		}
		return nil
	}, func(context.Context, string, map[string]any) error { verifies++; return nil })
	cfg := adobeTestConfig()
	cfg["adobe"] = map[string]any{"api_base": worker.URL}
	id, err := e.Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	waitAdobe(t, e)
	job, _ := e.getJob(id)
	if mediaReservations != 1 || imports != 1 || verifies != 1 || job.Status != "completed" || job.AdobeRegistration.MediaPending || len(job.Account) != 0 {
		t.Fatalf("fresh media handoff did not finish once: %+v", job)
	}
}
