package register

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func adobeTestAge(now, birthday time.Time) int {
	years := now.Year() - birthday.Year()
	if now.Month() < birthday.Month() || (now.Month() == birthday.Month() && now.Day() < birthday.Day()) {
		years--
	}
	return years
}

func TestAdobeBirthdayRangeIncludesOnlyAges25Through50(t *testing.T) {
	for _, date := range []string{"2026-11-11", "2024-02-29", "2025-02-28", "2025-03-01", "2026-01-01", "2026-12-31"} {
		t.Run(date, func(t *testing.T) {
			now, _ := time.Parse("2006-01-02", date)
			first, last := adobeBirthdayBounds(now)
			if adobeTestAge(now, first) != 50 || adobeTestAge(now, last) != 25 {
				t.Fatalf("wrong inclusive range: %s through %s", first, last)
			}
			if adobeTestAge(now, first.AddDate(0, 0, -1)) != 51 || adobeTestAge(now, last.AddDate(0, 0, 1)) != 24 {
				t.Fatal("range excludes eligible boundary dates or includes ineligible ones")
			}
			for birthday := first; !birthday.After(last); birthday = birthday.AddDate(0, 0, 1) {
				if age := adobeTestAge(now, birthday); age < 25 || age > 50 {
					t.Fatalf("invalid age %d for %s", age, birthday)
				}
			}
			for i := 0; i < 32; i++ {
				profile, err := newAdobeRegistrationProfile(now, rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				birth := time.Date(profile.BirthYear, time.Month(profile.BirthMonth), profile.BirthDay, 0, 0, 0, 0, time.UTC)
				if profile.Country != "US" || birth.Day() != profile.BirthDay || birth.Month() != time.Month(profile.BirthMonth) || birth.Before(first) || birth.After(last) {
					t.Fatal("generated invalid profile")
				}
			}
		})
	}
	if _, err := newAdobeRegistrationProfile(time.Now(), bytes.NewReader(nil)); err == nil {
		t.Fatal("randomness failure ignored")
	}
}

func TestAdobeConfigurationIgnoresRemovedIdentityAndMailTiming(t *testing.T) {
	raw := adobeTestConfig()
	raw["adobe"] = map[string]any{"country": "INVALID", "birth_year": 2100, "birth_month": 99, "birth_day": 99}
	mail := object(raw["mail"])
	mail["wait_timeout"], mail["wait_interval"] = 9999, 9999
	raw["mail"] = mail
	cfg, err := ParseAdobeConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != nil {
		t.Fatal("batch config generated or retained a shared identity")
	}
	if cfg.MailTimeout != defaultMailWaitTimeout || cfg.PollInterval != defaultMailPollInterval {
		t.Fatal("Adobe uses custom mail timing")
	}
	if intValue(object(raw["mail"])["wait_timeout"]) != 9999 {
		t.Fatal("parse mutated another platform's shared settings")
	}

	configPath := filepath.Join(t.TempDir(), "register.json")
	if err := writeJSON0600(configPath, raw); err != nil {
		t.Fatal(err)
	}
	store := New(configPath, filepath.Join(t.TempDir(), "accounts.json"))
	assertRemoved := func(value map[string]any) {
		t.Helper()
		for _, key := range []string{"country", "birth_year", "birth_month", "birth_day"} {
			if _, ok := object(value["adobe"])[key]; ok {
				t.Fatalf("retained removed setting %s", key)
			}
		}
	}
	assertRemoved(store.Get())
	saved, err := store.UpdateForTarget("adobe", map[string]any{"adobe": raw["adobe"]})
	if err != nil {
		t.Fatal(err)
	}
	assertRemoved(saved)
	assertRemoved(New(configPath, "").Get())
	encoded, err := os.ReadFile(configPath)
	if err != nil || bytes.Contains(encoded, []byte("birth_year")) {
		t.Fatal("legacy identity settings remained on disk")
	}
}

func TestAdobeRequestTimeoutDefaultsAndBounds(t *testing.T) {
	cfg, err := ParseAdobeConfig(adobeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestTimeout != 320*time.Second {
		t.Fatalf("default Adobe browser-call timeout = %s, want 320s", cfg.RequestTimeout)
	}
	for _, tc := range []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{"minimum", 30, 30 * time.Second},
		{"maximum", 600, 600 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := adobeTestConfig()
			raw["adobe"] = map[string]any{"request_timeout": tc.seconds}
			got, err := ParseAdobeConfig(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.RequestTimeout != tc.want {
				t.Fatalf("request timeout = %s, want %s", got.RequestTimeout, tc.want)
			}
		})
	}
	for _, seconds := range []int{29, 601} {
		raw := adobeTestConfig()
		raw["adobe"] = map[string]any{"request_timeout": seconds}
		if _, err := ParseAdobeConfig(raw); err == nil || !strings.Contains(err.Error(), "adobe_timeout_must_be_30_to_600_seconds") {
			t.Fatalf("request_timeout=%d was not rejected with the bounded error: %v", seconds, err)
		}
	}
}

func TestAdobeTaskProfilesArePrivateIndependentAndStableAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	cfg := adobeTestConfig()
	cfg["total"] = 3
	seen := []map[string]any{}
	engine := NewAdobeEngine(path, "profile-key", func(c AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		seen = append(seen, cloneMap(c.Profile))
		c.Profile["country"] = "MUTATED"
		return nil, nil, fail("fixture", "stop_before_mailbox")
	}, nil, nil)
	_, err := engine.Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	waitAdobe(t, engine)
	if len(seen) != 3 {
		t.Fatalf("expected independent profile in each factory, got %d", len(seen))
	}
	original := []RegistrationJob{}
	engine.mu.Lock()
	for _, job := range engine.jobs {
		original = append(original, cloneJob(job))
	}
	engine.mu.Unlock()
	for i, j := range original {
		if j.AdobeProfile == nil || j.AdobeProfile.Country != "US" {
			t.Fatal("mutable batch config changed saved profile")
		}
		if !reflect.DeepEqual(cloneMap(j.AdobeProfile.fields()), seen[i]) {
			t.Fatal("flow did not receive its persisted profile")
		}
	}
	restored := NewAdobeEngine(path, "profile-key", nil, nil, nil)
	for _, j := range original {
		got, e := restored.getJob(j.ID)
		if e != nil || !reflect.DeepEqual(got.AdobeProfile, j.AdobeProfile) {
			t.Fatal("restart replaced the saved identity")
		}
		got.AdobeProfile.Country = "MUTATED"
		check, _ := restored.getJob(j.ID)
		if check.AdobeProfile.Country != "US" {
			t.Fatal("getJob returned shared profile state")
		}
	}
	for _, raw := range restored.Snapshot()["jobs"].([]any) {
		if _, ok := raw.(map[string]any)["adobe_profile"]; ok {
			t.Fatal("public task snapshot exposed birthday")
		}
	}
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
