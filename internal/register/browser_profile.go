package register

import (
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserprofile"
	"github.com/bogdanfinn/tls-client/profiles"
)

// BrowserProfile keeps the persisted registration schema while sharing the
// same browser definition with authenticated ChatGPT requests.
type BrowserProfile browserprofile.Profile

func NewBrowserProfile() BrowserProfile { return BrowserProfile(browserprofile.New()) }
func ParseBrowserProfile(v any) (BrowserProfile, bool) {
	p, ok := browserprofile.Parse(v)
	return BrowserProfile(p), ok
}
func BrowserProfileFromFingerprint(v any) (BrowserProfile, bool) {
	p, ok := browserprofile.FromFingerprint(v)
	return BrowserProfile(p), ok
}
func (p BrowserProfile) valid() bool { return browserprofile.Profile(p).Valid() }
func (p BrowserProfile) tlsClientProfile() profiles.ClientProfile {
	return browserprofile.Profile(p).TLSClientProfile()
}
func (p BrowserProfile) navigatorLanguagesValue() string {
	return browserprofile.Profile(p).NavigatorLanguagesValue()
}
func (p BrowserProfile) fingerprintMetadata() map[string]any {
	return browserprofile.Profile(p).FingerprintMetadata()
}
func (p *BrowserProfile) setTimezone(loc *time.Location, at time.Time) {
	(*browserprofile.Profile)(p).SetTimezone(loc, at)
}

func maxInt(a, b int) int { return max(a, b) }
