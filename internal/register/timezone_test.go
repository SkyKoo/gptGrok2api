package register

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mustTimezone(t *testing.T, zone string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestRegistrationTimezoneDatesAndDST(t *testing.T) {
	for _, tc := range []struct {
		zone, when, suffix string
		offset             int
	}{
		{"America/Phoenix", "2026-07-01T12:00:00Z", "05:00:00 GMT-0700 (MST)", 420},
		{"America/Los_Angeles", "2026-01-01T12:00:00Z", "04:00:00 GMT-0800 (PST)", 480},
		{"America/Los_Angeles", "2026-07-01T12:00:00Z", "05:00:00 GMT-0700 (PDT)", 420},
		{"Asia/Kolkata", "2026-07-01T12:00:00Z", "17:30:00 GMT+0530 (IST)", -330},
	} {
		t.Run(tc.zone+tc.when, func(t *testing.T) {
			at, _ := time.Parse(time.RFC3339, tc.when)
			loc := mustTimezone(t, tc.zone)
			profile := browserProfilePool[0]
			profile.setTimezone(loc, at)
			if profile.TimezoneIANA != tc.zone || profile.TimezoneOffsetMinutes != tc.offset {
				t.Fatal("wrong zone or JS offset", profile)
			}
			flow := &WebRegistrar{profile: browserProfilePool[0], timezone: loc}
			if date := flow.browserDate(at); !strings.HasSuffix(date, tc.suffix) {
				t.Fatal("wrong local date", date)
			}
		})
	}
}

func TestRestoreFingerprintWithEgressTimezone(t *testing.T) {
	want := browserProfilePool[4]
	want.setTimezone(mustTimezone(t, "America/Phoenix"), time.Now())
	fp := want.fingerprintMetadata()
	got, ok := BrowserProfileFromFingerprint(fp)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatal("could not restore non-UTC fingerprint")
	}
	for _, bad := range []string{"Local", "", "../etc/passwd", "Unknown/Zone"} {
		fp["timezone"] = bad
		if _, ok := BrowserProfileFromFingerprint(fp); ok {
			t.Fatal("accepted invalid timezone", bad)
		}
	}
	fp["timezone"] = "America/Phoenix"
	fp["user-agent"] = "different-browser"
	if _, ok := BrowserProfileFromFingerprint(fp); ok {
		t.Fatal("timezone change bypassed stable profile validation")
	}
}

func TestLoginTimezoneCancellationStopsAuthorization(t *testing.T) {
	for _, existing := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		flow, err := NewWebRegistrar("https://chat.example.test", "https://auth.example.test", "http://proxy.example.test:8080", browserProfilePool[0])
		if err != nil {
			t.Fatal(err)
		}
		flow.resolveTimezone = func(ctx context.Context, origin, proxy string) *time.Location {
			if proxy != "http://proxy.example.test:8080" {
				t.Error("wrong proxy")
			}
			cancel()
			return mustTimezone(t, "America/Los_Angeles")
		}
		login := flow.Register
		if existing {
			login = flow.LoginExisting
		}
		_, err = login(ctx, Mailbox{}, FreeConfig{}, nil, func(string) error { t.Error("authorization started after cancel"); return nil })
		flow.Close()
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatal("lost cancellation", err)
		}
	}
}

func TestResolvedRegistrationTimezoneSurvivesFailureAndCompensation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	var requests, factories atomic.Int32
	zones := []string{"America/Phoenix", "Asia/Tokyo"}
	stable := browserProfilePool[4]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(requests.Add(1)) - 1
		raw, err := os.ReadFile(path)
		var jobs []RegistrationJob
		if err != nil || json.Unmarshal(raw, &jobs) != nil || len(jobs) != i+1 || jobs[len(jobs)-1].BrowserProfile == nil {
			t.Error("missing durable profile before auth")
		} else if i >= len(zones) || jobs[len(jobs)-1].BrowserProfile.TimezoneIANA != zones[i] || jobs[len(jobs)-1].BrowserProfile.Name != stable.Name {
			t.Error("durable profile did not match actual attempt")
		}
		http.Error(w, "temporary outage", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	mail := &fakeMail{}
	factory := func(cfg FreeConfig) (MailSource, RegistrationFlow, error) {
		i := int(factories.Add(1)) - 1
		profile := stable
		if i > 0 {
			if cfg.BrowserProfile == nil || cfg.BrowserProfile.TimezoneIANA != zones[i-1] {
				t.Error("retry lost previous environment")
			} else {
				profile = *cfg.BrowserProfile
			}
		}
		flow, err := NewWebRegistrar(server.URL, server.URL, "direct", profile)
		if err != nil {
			return nil, nil, err
		}
		flow.resolveTimezone = func(context.Context, string, string) *time.Location { return mustTimezone(t, zones[i]) }
		return mail, flow, nil
	}
	engine := NewFreeEngine(path, factory, nil, nil)
	id, err := engine.Start(freeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	waitEngine(t, engine)
	job := mustCompensationJob(t, engine, id)
	if job.Status != "registration_pending" || job.BrowserProfile == nil || job.BrowserProfile.TimezoneIANA != zones[0] {
		t.Fatal("failed attempt lost timezone")
	}
	// Reload the journal, then compensate through the same production entry point.
	engine = NewFreeEngine(path, factory, nil, nil)
	if err := engine.RetryRegistration(freeTestConfig(), id); err != nil {
		t.Fatal(err)
	}
	waitEngine(t, engine)
	job = mustCompensationJob(t, engine, id)
	if job.BrowserProfile == nil || job.BrowserProfile.TimezoneIANA != zones[1] || requests.Load() != 2 || mail.calls.Load() != 1 || mail.prepared.Load() != 1 {
		t.Fatal("compensation did not realign egress or duplicated mailbox")
	}
	want := stable
	want.setTimezone(mustTimezone(t, zones[1]), time.Now())
	if !reflect.DeepEqual(*job.BrowserProfile, want) {
		t.Fatal("stable browser fields changed")
	}
}

// Execute the real Node VM runner with a local probe SDK (no upstream SDK or
// network). This checks the Go arguments, Date and Intl together, including DST.
func TestSentinelRunnerUsesResolvedTimezone(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the Sentinel VM test")
	}
	runner, err := filepath.Abs("sentinel-runner.js")
	if err != nil {
		t.Fatal(err)
	}
	sdk := filepath.Join(t.TempDir(), "probe.js")
	code := `var SentinelSDK = {
 token: async () => JSON.stringify({p:"probe",c:"challenge",observed:{zone:Intl.DateTimeFormat().resolvedOptions().timeZone, date:new Date().toString(), offset:new Date().getTimezoneOffset(), winter:new Date("2026-01-01T12:00:00Z").getTimezoneOffset(), summer:new Date("2026-07-01T12:00:00Z").getTimezoneOffset()}}),
 sessionObserverToken: async () => "probe-so"
 };`
	if err := os.WriteFile(sdk, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CFM_SENTINEL_NODE", node)
	t.Setenv("CFM_SENTINEL_RUNNER", runner)
	t.Setenv("CFM_SENTINEL_SDK", sdk)
	for _, zone := range []string{"America/Phoenix", "America/Los_Angeles", "Asia/Kolkata"} {
		t.Run(zone, func(t *testing.T) {
			flow := &WebRegistrar{chat: "https://chat.example.test", auth: "https://auth.example.test", sentinel: sentinelOrigin, profile: browserProfilePool[0], timezone: mustTimezone(t, zone), device: "test-device"}
			headers, err := flow.sentinelHeadersFromRunner(context.Background(), "oauth_create_account", "proof", "sid", "cookie", map[string]any{"token": "challenge"})
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Observed struct {
					Zone, Date             string
					Offset, Winter, Summer int
				}
			}
			if err := json.Unmarshal([]byte(headers["openai-sentinel-token"]), &payload); err != nil {
				t.Fatal(err)
			}
			got := payload.Observed
			profile := flow.BrowserProfile()
			// ICU may canonicalize Kolkata to its Calcutta alias.
			if got.Zone != zone && !(zone == "Asia/Kolkata" && got.Zone == "Asia/Calcutta") {
				t.Fatal("wrong Intl timezone", got.Zone)
			}
			if got.Offset != profile.TimezoneOffsetMinutes || !strings.HasSuffix(got.Date, "("+profile.TimezoneName+")") {
				t.Fatal("Date/Intl/profile diverged", got)
			}
			for month, offset := range map[time.Month]int{time.January: got.Winter, time.July: got.Summer} {
				_, seconds := time.Date(2026, month, 1, 12, 0, 0, 0, time.UTC).In(flow.timezone).Zone()
				if offset != -seconds/60 {
					t.Fatal("VM Date ignored DST", month, offset)
				}
			}
		})
	}
}
