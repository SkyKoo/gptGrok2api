package register

import (
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
