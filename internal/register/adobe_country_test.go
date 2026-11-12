package register

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
					json.NewEncoder(w).Encode(map[string]any{"stage": "prepared", "registration_country": adobeCountrySelection{Country: country, Source: source}})
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
