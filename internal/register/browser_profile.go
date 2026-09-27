package register

import (
	crand "crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/bogdanfinn/tls-client/profiles"
)

// BrowserProfile is the protocol-mode browser environment used by one
// registration task. It is a coherent parameter set shared by the HTTP
// headers, tls-client and the Sentinel VM runner. It is not a real browser
// fingerprint or a browser profile on disk.
type BrowserProfile struct {
	Name                   string   `json:"name"`
	TLSProfile             string   `json:"tls_profile"`
	BrowserFamily          string   `json:"browser_family"`
	BrowserOS              string   `json:"browser_os"`
	NavigatorPlatform      string   `json:"navigator_platform"`
	NavigatorVendor        string   `json:"navigator_vendor"`
	UserAgentDataPlatform  string   `json:"user_agent_data_platform"`
	ChromeMajor            string   `json:"chrome_major"`
	ChromeFullVersion      string   `json:"chrome_full_version"`
	UserAgent              string   `json:"user_agent"`
	SecCHUA                string   `json:"sec_ch_ua"`
	SecCHUAFullVersionList string   `json:"sec_ch_ua_full_version_list"`
	SecCHUAPlatform        string   `json:"sec_ch_ua_platform"`
	SecCHUAPlatformVersion string   `json:"sec_ch_ua_platform_version"`
	SecCHUAMobile          string   `json:"sec_ch_ua_mobile"`
	SecCHUAArch            string   `json:"sec_ch_ua_arch"`
	SecCHUABitness         string   `json:"sec_ch_ua_bitness"`
	SecCHUAModel           string   `json:"sec_ch_ua_model"`
	AcceptLanguage         string   `json:"accept_language"`
	NavigatorLanguage      string   `json:"navigator_language"`
	NavigatorLanguages     []string `json:"navigator_languages"`
	TimezoneIANA           string   `json:"timezone_iana"`
	TimezoneName           string   `json:"timezone_name"`
	TimezoneOffsetMinutes  int      `json:"timezone_offset_minutes"`
	ScreenWidth            int      `json:"screen_width"`
	ScreenHeight           int      `json:"screen_height"`
	AvailWidth             int      `json:"avail_width"`
	AvailHeight            int      `json:"avail_height"`
	OuterWidth             int      `json:"outer_width"`
	OuterHeight            int      `json:"outer_height"`
	InnerWidth             int      `json:"inner_width"`
	InnerHeight            int      `json:"inner_height"`
	HardwareConcurrency    int      `json:"hardware_concurrency"`
	DeviceMemory           int      `json:"device_memory"`
	JSHeapSizeLimit        int64    `json:"js_heap_size_limit"`
	DevicePixelRatio       int      `json:"device_pixel_ratio"`
}

// browserProfileBase contains the fields that Turb's protocol profile keeps
// stable across its candidate desktop environments. tls-client v1.9.2 has a
// Chrome 131 profile, so the HTTP and JavaScript versions intentionally stay
// on Chrome 131 until a matching newer TLS profile is available locally.
func browserProfileBase(width, height, cores int, heap int64) BrowserProfile {
	const chromeMajor = "131"
	const chromeFullVersion = "131.0.0.0"
	return BrowserProfile{
		Name:                   fmt.Sprintf("mac-chrome%s-%dx%d-%dc", chromeMajor, width, height, cores),
		TLSProfile:             "chrome_131",
		BrowserFamily:          "chrome",
		BrowserOS:              "macOS",
		NavigatorPlatform:      "MacIntel",
		NavigatorVendor:        "Google Inc.",
		UserAgentDataPlatform:  "macOS",
		ChromeMajor:            chromeMajor,
		ChromeFullVersion:      chromeFullVersion,
		UserAgent:              fmt.Sprintf("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/537.36", chromeFullVersion),
		SecCHUA:                fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not_A Brand";v="24"`, chromeMajor, chromeMajor),
		SecCHUAFullVersionList: fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not_A Brand";v="24.0.0.0"`, chromeFullVersion, chromeFullVersion),
		SecCHUAPlatform:        `"macOS"`,
		SecCHUAPlatformVersion: `"15.7.0"`,
		SecCHUAMobile:          "?0",
		SecCHUAArch:            `"arm"`,
		SecCHUABitness:         `"64"`,
		SecCHUAModel:           `""`,
		AcceptLanguage:         "en-US,en;q=0.9",
		NavigatorLanguage:      "en-US",
		NavigatorLanguages:     []string{"en-US", "en"},
		TimezoneIANA:           "UTC",
		TimezoneName:           "Coordinated Universal Time",
		TimezoneOffsetMinutes:  0,
		ScreenWidth:            width,
		ScreenHeight:           height,
		AvailWidth:             width,
		AvailHeight:            maxInt(0, height-25),
		OuterWidth:             width,
		OuterHeight:            height,
		InnerWidth:             width,
		InnerHeight:            maxInt(0, height-87),
		HardwareConcurrency:    cores,
		DeviceMemory:           8,
		JSHeapSizeLimit:        heap,
		DevicePixelRatio:       2,
	}
}

// browserProfilePool follows Turb's protocol-mode approach: randomize a
// small set of plausible desktop environments, while keeping each selected
// profile internally consistent for the lifetime of one registration task.
var browserProfilePool = []BrowserProfile{
	browserProfileBase(1680, 1050, 6, 4395630592),
	browserProfileBase(1440, 900, 8, 4294967296),
	browserProfileBase(1512, 982, 8, 4294967296),
	browserProfileBase(1680, 1050, 8, 4294967296),
	browserProfileBase(1728, 1117, 10, 4294967296),
	browserProfileBase(1800, 1169, 10, 4294967296),
	browserProfileBase(2056, 1329, 12, 4294967296),
}

// NewBrowserProfile selects a profile for a new protocol session. The
// selected profile is a value, so callers can safely persist and reuse it.
func NewBrowserProfile() BrowserProfile {
	if len(browserProfilePool) == 0 {
		return browserProfileBase(1680, 1050, 8, 4294967296)
	}
	index, err := crand.Int(crand.Reader, big.NewInt(int64(len(browserProfilePool))))
	if err != nil {
		index = big.NewInt(0)
	}
	profile := browserProfilePool[int(index.Int64())]
	profile.NavigatorLanguages = append([]string(nil), profile.NavigatorLanguages...)
	return profile
}

func (p BrowserProfile) valid() bool {
	return p.TLSProfile == "chrome_131" &&
		p.BrowserFamily == "chrome" &&
		p.BrowserOS == "macOS" &&
		p.UserAgent != "" && p.SecCHUA != "" &&
		p.NavigatorPlatform != "" && p.UserAgentDataPlatform != "" &&
		p.ScreenWidth > 0 && p.ScreenHeight > 0 &&
		p.HardwareConcurrency > 0 && p.DeviceMemory > 0 &&
		p.JSHeapSizeLimit > 0 && p.DevicePixelRatio > 0
}

func (p BrowserProfile) tlsClientProfile() profiles.ClientProfile {
	// Keep the mapping explicit. If another tls-client profile is added later,
	// it must be paired with matching UA/client-hint/Sentinel versions.
	return profiles.Chrome_131
}

func (p BrowserProfile) navigatorLanguagesValue() string {
	if len(p.NavigatorLanguages) == 0 {
		return p.NavigatorLanguage
	}
	return strings.Join(p.NavigatorLanguages, ",")
}

func (p BrowserProfile) fingerprintMetadata() map[string]any {
	return map[string]any{
		"profile":                  p.Name,
		"user-agent":               p.UserAgent,
		"impersonate":              p.TLSProfile,
		"browser-family":           p.BrowserFamily,
		"browser-os":               p.BrowserOS,
		"navigator-platform":       p.NavigatorPlatform,
		"user-agent-data-platform": p.UserAgentDataPlatform,
		"sec-ch-ua":                p.SecCHUA,
		"sec-ch-ua-platform":       p.SecCHUAPlatform,
		"accept-language":          p.AcceptLanguage,
		"timezone":                 p.TimezoneIANA,
		"screen":                   fmt.Sprintf("%dx%d", p.ScreenWidth, p.ScreenHeight),
		"hardware-concurrency":     p.HardwareConcurrency,
		"device-memory":            p.DeviceMemory,
		"device-pixel-ratio":       p.DevicePixelRatio,
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
