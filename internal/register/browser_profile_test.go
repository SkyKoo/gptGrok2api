package register

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBrowserProfilePoolIsCoherent(t *testing.T) {
	if len(browserProfilePool) < 2 {
		t.Fatal("browser profile pool is too small")
	}
	for _, profile := range browserProfilePool {
		if !profile.valid() {
			t.Fatalf("invalid browser profile: %#v", profile)
		}
		if !strings.Contains(profile.UserAgent, "Chrome/"+profile.ChromeFullVersion) {
			t.Fatalf("user agent does not match chrome version: %#v", profile)
		}
		if !strings.Contains(profile.SecCHUA, `v="`+profile.ChromeMajor+`"`) {
			t.Fatalf("client hints do not match chrome version: %#v", profile)
		}
		if profile.NavigatorPlatform != "MacIntel" || profile.UserAgentDataPlatform != "macOS" || profile.SecCHUAPlatform != `"macOS"` {
			t.Fatalf("platform fields are inconsistent: %#v", profile)
		}
		if profile.AvailHeight >= profile.ScreenHeight || profile.InnerHeight >= profile.ScreenHeight {
			t.Fatalf("window dimensions are not browser-like: %#v", profile)
		}
	}
}

func TestNewWebRegistrarUsesRequestedBrowserProfile(t *testing.T) {
	want := browserProfileBase(1800, 1169, 10, 4294967296)
	flow, err := NewWebRegistrar("https://chat.example.test", "https://auth.example.test", "direct", want)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	got := flow.BrowserProfile()
	if got.Name != want.Name || got.UserAgent != want.UserAgent || got.ScreenWidth != want.ScreenWidth || got.HardwareConcurrency != want.HardwareConcurrency {
		t.Fatalf("requested profile was not retained: got=%#v want=%#v", got, want)
	}
}

func TestRestoreSavedBrowserProfile(t *testing.T) {
	for _, want := range browserProfilePool {
		raw, _ := json.Marshal(want)
		var stored map[string]any
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		got, ok := ParseBrowserProfile(stored)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatal("persisted profile changed")
		}
		got.NavigatorLanguages[0] = "changed"
		if want.NavigatorLanguages[0] == "changed" {
			t.Fatal("profile shares mutable state")
		}
		fp := want.fingerprintMetadata()
		got, ok = BrowserProfileFromFingerprint(fp)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatal("legacy summary did not restore the original template")
		}
		fp["user-agent"] = "inconsistent UA"
		if _, ok := BrowserProfileFromFingerprint(fp); ok {
			t.Fatal("accepted inconsistent saved summary")
		}
	}
	if _, ok := ParseBrowserProfile(map[string]any{"tls_profile": "unsupported"}); ok {
		t.Fatal("accepted invalid profile")
	}
	if _, ok := BrowserProfileFromFingerprint(map[string]any{"profile": "unknown"}); ok {
		t.Fatal("guessed an unknown profile")
	}
}

func TestSavedDeviceIDValidation(t *testing.T) {
	flow, err := NewWebRegistrar("https://chat.example.test", "https://auth.example.test", "direct")
	if err != nil {
		t.Fatal(err)
	}
	defer flow.Close()
	for _, device := range []string{"saved-device-123", "", "abcdef0123456789"} {
		if err := flow.SetDeviceID(device); err != nil {
			t.Fatal(err)
		}
	}
	for _, device := range []string{"bad\r\nheader", "cookie;value", strings.Repeat("a", 257)} {
		if err := flow.SetDeviceID(device); err == nil {
			t.Fatal("accepted invalid device ID")
		}
	}
}
