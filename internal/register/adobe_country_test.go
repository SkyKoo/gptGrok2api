package register

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdobeCountrySelectionPersistsBeforeCreationAndRejectsChangedSavedCountry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.enc.json")
	e := NewAdobeEngine(path, "country-fixture-key", nil, nil, nil)
	seedAdobeCompensation(t, e, RegistrationJob{ID: "task", AdobeProfile: &adobeRegistrationProfile{Country: "US", BirthYear: 1988, BirthMonth: 6, BirthDay: 2}})
	selection := adobeCountrySelection{Country: "JP", Source: "ipwho_is"}
	if err := e.recordRegistrationCountry("task", selection); err != nil {
		t.Fatal(err)
	}
	if err := e.recordRegistrationCountry("task", selection); err != nil {
		t.Fatal(err)
	}
	got, _ := e.getJob("task")
	if got.AdobeProfile.Country != "JP" || got.AdobeProfile.CountrySource != "ipwho_is" || got.AdobeProfile.BirthYear != 1988 || len(got.Logs) != 1 {
		t.Fatal("country was not saved independently and idempotently")
	}
	reloaded := NewAdobeEngine(path, "country-fixture-key", nil, nil, nil)
	saved, _ := reloaded.getJob("task")
	if !reflect.DeepEqual(got.AdobeProfile, saved.AdobeProfile) {
		t.Fatal("country changed after restart")
	}
	if err := e.recordRegistrationCountry("task", adobeCountrySelection{Country: "CA", Source: "ipwho_is"}); err == nil {
		t.Fatal("saved country changed")
	}
	for _, value := range []adobeCountrySelection{{Country: "ZZ", Source: "ipwho_is"}, {Country: "US", Source: "PRIVATE"}, {Country: "CA", Source: "default_us"}, {Country: "us", Source: "ipwho_is"}} {
		if err := e.recordRegistrationCountry("task", value); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
}

func TestAdobeCountryFallbackAndAcceptedResumeRemainExplicit(t *testing.T) {
	e := NewAdobeEngine(filepath.Join(t.TempDir(), "jobs.enc.json"), "fixture", nil, nil, nil)
	seedAdobeCompensation(t, e,
		RegistrationJob{ID: "fresh", AdobeProfile: &adobeRegistrationProfile{Country: "US"}},
		RegistrationJob{ID: "accepted", AdobeProfile: &adobeRegistrationProfile{Country: "GB"}, AdobeRegistration: &AdobeRegistrationState{SubmissionStarted: true, CreationOutcome: "accepted"}},
	)
	if err := e.recordRegistrationCountry("fresh", adobeCountrySelection{Country: "US", Source: "default_us"}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.getJob("fresh")
	encoded, _ := json.Marshal(got.Logs)
	if !strings.Contains(string(encoded), "定位失败") || got.AdobeProfile.CountrySource != "default_us" {
		t.Fatal("fallback masquerades as successful lookup")
	}
	if err := e.recordRegistrationCountry("accepted", adobeCountrySelection{Country: "US", Source: "ipwho_is"}); err == nil {
		t.Fatal("accepted country changed")
	}
	if err := e.recordRegistrationCountry("accepted", adobeCountrySelection{Country: "GB", Source: "preserved"}); err != nil {
		t.Fatal(err)
	}
}

func TestAdobeCountryPreparationMustBeValidAndDurableBeforeRun(t *testing.T) {
	for _, scenario := range []string{"success", "fallback", "invalid", "old_worker", "persist_failure"} {
		t.Run(scenario, func(t *testing.T) {
			persisted := false
			var commands []string
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					json.NewEncoder(w).Encode(map[string]string{"status": "closed"})
					return
				}
				if r.URL.Path == "/v1/browser/reservations" {
					json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
					return
				}
				var payload map[string]any
				json.NewDecoder(r.Body).Decode(&payload)
				action := stringValue(payload["action"])
				commands = append(commands, action)
				if action == "prepare" {
					country, source := "JP", "ipwho_is"
					if scenario == "fallback" {
						country, source = "US", "default_us"
					}
					if scenario == "invalid" {
						country = "ZZ"
					}
					if scenario == "old_worker" {
						w.WriteHeader(http.StatusUnprocessableEntity)
						return
					}
					selection := adobeCountrySelection{Country: country, Source: source}
					if scenario == "fallback" {
						exit := 22
						selection.Diagnostics = &adobeCountryDiagnostics{FailureKind: "proxy_tunnel", CurlExitCode: &exit, ConnectStatus: 502, ElapsedMS: 125}
					}
					json.NewEncoder(w).Encode(map[string]any{"stage": "prepared", "registration_country": selection})
					return
				}
				if !persisted {
					t.Error("registration ran before country persistence")
				}
				json.NewEncoder(w).Encode(map[string]any{"stage": "completed", "account": map[string]any{"email": "fixture@example.test", "account_id": "fixture", "token": "fixture-token"}})
			}))
			defer worker.Close()
			flow := AdobeBrowserFlow{Client: worker.Client()}
			cfg := AdobeConfig{WorkerURL: worker.URL, Profile: map[string]any{"country": "US"}, countryProgress: func(value adobeCountrySelection) error {
				if scenario == "persist_failure" {
					return fail("storage", "fixture_write_failed")
				}
				if !value.valid() {
					t.Fatal("unvalidated country persisted")
				}
				if scenario == "fallback" && (value.Diagnostics == nil || value.Diagnostics.FailureKind != "proxy_tunnel" || value.Diagnostics.ConnectStatus != 502) {
					t.Fatal("Worker transport diagnostics were lost before task persistence")
				}
				persisted = true
				return nil
			}}
			_, err := flow.Register(context.Background(), "task", Mailbox{Email: "fixture@example.test"}, cfg, nil, func(string) error { return nil })
			flow.Close()
			if scenario == "success" || scenario == "fallback" {
				if err != nil || !reflect.DeepEqual(commands, []string{"prepare", "run"}) {
					t.Fatalf("prepared flow failed: %v %v", err, commands)
				}
			} else if err == nil || !reflect.DeepEqual(commands, []string{"prepare"}) {
				t.Fatal("invalid or unpersisted selection reached creation")
			}
		})
	}
}

func TestAdobeCountryDiagnosticsAreDurableBoundedAndCopied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.enc.json")
	e := NewAdobeEngine(path, "fixture", nil, nil, nil)
	seedAdobeCompensation(t, e, RegistrationJob{ID: "task", AdobeProfile: &adobeRegistrationProfile{Country: "US"}})
	exit := 22
	selection := adobeCountrySelection{Country: "US", Source: "default_us", Diagnostics: &adobeCountryDiagnostics{
		FailureKind: "proxy_tunnel", CurlExitCode: &exit, ConnectStatus: 502, ElapsedMS: 125}}
	if err := e.recordRegistrationCountry("task", selection); err != nil {
		t.Fatal(err)
	}
	selection.Diagnostics.ConnectStatus = 407
	exit = 0
	got, _ := e.getJob("task")
	if got.AdobeProfile.CountryDiagnostics.ConnectStatus != 502 || *got.AdobeProfile.CountryDiagnostics.CurlExitCode != 22 {
		t.Fatal("caller mutated stored diagnostics")
	}
	raw, _ := json.Marshal(got.Logs)
	for _, text := range []string{"实际出口国家未确认", "代理隧道失败", "CONNECT=502", "curl=22", "耗时125ms"} {
		if !strings.Contains(string(raw), text) {
			t.Fatalf("missing safe diagnostic %s", text)
		}
	}
	reloaded := NewAdobeEngine(path, "fixture", nil, nil, nil)
	saved, _ := reloaded.getJob("task")
	if !reflect.DeepEqual(saved.AdobeProfile, got.AdobeProfile) {
		t.Fatal("country diagnostics lost across restart")
	}
	fields := got.AdobeProfile.fields()
	fields["countryDiagnostics"].(*adobeCountryDiagnostics).ConnectStatus = 407
	got.AdobeProfile.CountryDiagnostics.ConnectStatus = 407
	again, _ := e.getJob("task")
	if again.AdobeProfile.CountryDiagnostics.ConnectStatus != 502 {
		t.Fatal("public copy mutated private journal")
	}
	if err := e.recordRegistrationCountry("task", adobeCountrySelection{Country: "US", Source: "default_us"}); err != nil {
		t.Fatal(err)
	}
	again, _ = e.getJob("task")
	if len(again.Logs) != 1 || again.AdobeProfile.CountryDiagnostics.ConnectStatus != 502 {
		t.Fatal("resume replaced original location evidence")
	}
}

func TestAdobeCountryDiagnosticsRejectUntrustedCategoriesAndNumbers(t *testing.T) {
	exit := 22
	good := adobeCountryDiagnostics{FailureKind: "proxy_tunnel", CurlExitCode: &exit, ConnectStatus: 502, ElapsedMS: 125}
	for _, change := range []func(*adobeCountryDiagnostics){
		func(d *adobeCountryDiagnostics) { d.FailureKind = "PRIVATE" },
		func(d *adobeCountryDiagnostics) { d.ElapsedMS = -1 },
		func(d *adobeCountryDiagnostics) { d.ElapsedMS = 60001 },
		func(d *adobeCountryDiagnostics) { d.HTTPStatus = 999 },
		func(d *adobeCountryDiagnostics) { d.ConnectStatus = -1 },
		func(d *adobeCountryDiagnostics) { v := 256; d.CurlExitCode = &v },
		func(d *adobeCountryDiagnostics) { d.FailureKind = "none" },
	} {
		d := good
		change(&d)
		if d.valid("default_us") {
			t.Fatal("invalid transport metadata accepted")
		}
		if !(adobeCountrySelection{Country: "US", Source: "default_us", Diagnostics: &d}).valid() {
			t.Fatal("optional transport metadata invalidated a valid country")
		}
	}
	if good.valid("ipwho_is") {
		t.Fatal("failed lookup masqueraded as confirmed country")
	}
}

func malformedAdobeCountryDiagnostics() map[string]string {
	return map[string]string{
		"array": `["PRIVATE"]`, "scalar": `"PRIVATE"`, "null": `null`,
		"wrong_field_type": `{"failure_kind":"timeout","http_status":"PRIVATE","connect_status":502,"elapsed_ms":15}`,
		"fraction":         `{"failure_kind":"timeout","http_status":0,"connect_status":502.5,"elapsed_ms":15}`,
		"null_field":       `{"failure_kind":"timeout","http_status":null,"connect_status":502,"elapsed_ms":15}`,
		"invalid_range":    `{"failure_kind":"timeout","http_status":0,"connect_status":502,"elapsed_ms":-1}`,
		"unknown_field":    `{"failure_kind":"timeout","http_status":0,"connect_status":502,"elapsed_ms":15,"raw":"PRIVATE"}`,
		"oversized":        `{"raw":"` + strings.Repeat("x", 1024) + `"}`,
	}
}

func TestAdobeCountryMalformedDiagnosticPreservesPersistedIdentity(t *testing.T) {
	for name, raw := range malformedAdobeCountryDiagnostics() {
		t.Run(name, func(t *testing.T) {
			var job RegistrationJob
			if err := json.Unmarshal([]byte(`{"id":"accepted","adobe_profile":{"country":"US","countrySource":"default_us","countryDiagnostics":`+raw+`,"birthYear":1988},"adobe_registration":{"submission_started":true,"creation_outcome":"accepted"}}`), &job); err != nil {
				t.Fatalf("optional diagnostic prevented loading task: %v", err)
			}
			if job.AdobeProfile == nil || job.AdobeProfile.Country != "US" || job.AdobeProfile.CountrySource != "default_us" || job.AdobeProfile.BirthYear != 1988 || job.AdobeProfile.CountryDiagnostics != nil ||
				job.AdobeRegistration == nil || !job.AdobeRegistration.SubmissionStarted || adobeCreationOutcome(job) != "accepted" {
				t.Fatalf("optional diagnostic changed saved identity or creation result: %+v", job)
			}
		})
	}
}

func TestAdobeCountryMalformedDiagnosticCannotChangeAcceptedRecovery(t *testing.T) {
	for name, raw := range malformedAdobeCountryDiagnostics() {
		t.Run(name, func(t *testing.T) {
			var resumes atomic.Int32
			country := `"registration_country":{"country":"US","source":"default_us","diagnostics":` + raw + `}`
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					_, _ = w.Write([]byte(`{"status":"released"}`))
					return
				}
				var request map[string]any
				_ = json.NewDecoder(r.Body).Decode(&request)
				if r.URL.Path == "/v1/browser/reservations" {
					if request["adobe_mode"] != "resume" || request["profile_id"] != "original-adobe-profile" {
						t.Error("diagnostic changed the original recovery identity")
					}
					_, _ = w.Write([]byte(`{"status":"ready"}`))
					return
				}
				if request["action"] == "prepare" {
					_, _ = w.Write([]byte(`{"stage":"prepared",` + country + `}`))
					return
				}
				if request["action"] != "resume" {
					t.Error("diagnostic triggered another creation attempt")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				resumes.Add(1)
				_, _ = w.Write([]byte(`{"stage":"needs_manual","code":"recovery_login_form_unavailable","creation_outcome":"accepted","submission_started":true,` + country + `}`))
			}))
			defer worker.Close()
			config := adobeTestConfig()
			config["adobe"] = map[string]any{"api_base": worker.URL}
			path := filepath.Join(t.TempDir(), "adobe.enc.json")
			engine := NewAdobeEngine(path, "test-key", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
				return &adobeCompensationMail{}, &AdobeBrowserFlow{Client: worker.Client()}, nil
			}, nil, nil)
			now := time.Now().UTC()
			job := adobeCompensationFixture(t, now)
			job.AdobeRegistration.MediaPending = true
			job.AdobeProfile.CountrySource = "default_us"
			seedAdobeCompensation(t, engine, job)
			if started, err := engine.RunCompensation(config, now); err != nil || !started {
				t.Fatalf("recovery did not start: %v", err)
			}
			waitAdobe(t, engine)
			child := latestAdobe(t, engine, job.ID)
			if resumes.Load() != 1 || child.Error != "adobe: recovery_login_form_unavailable" || adobeCreationOutcome(child) != "accepted" || child.AdobeProfile.CountryDiagnostics != nil {
				t.Fatalf("optional diagnostic changed recovery result: %+v", child)
			}
			reloaded := NewAdobeEngine(path, "test-key", nil, nil, nil)
			restored, err := reloaded.getJob(child.ID)
			if err != nil || !restored.AdobeRegistration.SubmissionStarted || adobeCreationOutcome(restored) != "accepted" || restored.AdobeProfile.Country != "US" || restored.AdobeProfile.CountrySource != "default_us" {
				t.Fatalf("accepted identity did not survive restart: %+v %v", restored, err)
			}
		})
	}
}

func TestAdobeCountryAcceptedCompensationPreservesOriginalLookupEvidence(t *testing.T) {
	now := time.Now()
	parent := adobeCompensationFixture(t, now)
	parent.AdobeRegistration.MediaPending = true
	parent.AdobeProfile.CountrySource = "default_us"
	parent.AdobeProfile.CountryDiagnostics = &adobeCountryDiagnostics{FailureKind: "timeout", ElapsedMS: 4500}
	mail := &adobeCompensationMail{}
	var seen *adobeCountryDiagnostics
	engine := NewAdobeEngine(filepath.Join(t.TempDir(), "jobs.enc.json"), "test", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		return mail, adobeCompensationFlow{run: func(_ context.Context, cfg AdobeConfig, _ Progress) (map[string]any, error) {
			raw, _ := json.Marshal(cfg.Profile["countryDiagnostics"])
			json.Unmarshal(raw, &seen)
			if cfg.RecoveryMode != "resume_session" || cfg.Profile["country"] != "US" || cfg.Profile["countrySource"] != "default_us" {
				t.Error("accepted registration identity changed")
			}
			return nil, fail("adobe", "recovery_login_form_unavailable")
		}}, nil
	}, nil, nil)
	seedAdobeCompensation(t, engine, parent)
	if started, err := engine.RunCompensation(adobeTestConfig(), now); !started || err != nil {
		t.Fatalf("compensation did not start: %v", err)
	}
	waitAdobe(t, engine)
	if seen == nil || !reflect.DeepEqual(seen, parent.AdobeProfile.CountryDiagnostics) {
		t.Fatal("saved transport evidence was lost in resume request")
	}
}
