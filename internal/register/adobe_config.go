package register

import (
	"crypto/rand"
	"io"
	"math/big"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserqueue"
)

type AdobeConfig struct {
	Total, Threads                                     int
	Proxy, WorkerURL, WorkerKey                        string
	Timeout, MailTimeout, PollInterval, RequestTimeout time.Duration
	MailboxSourceID                                    string
	HME                                                HMEConfig
	Profile                                            map[string]any
	// Recovery is derived from the encrypted task journal, never user settings.
	BrowserProfileID, RecoveryMode string
	diagnosticProgress             func(adobeRegistrationDiagnostics) error
	creationProgress               func(adobeCreationObservation, bool) error
	startupProgress                func(browserqueue.StartupEvent) error
	countryProgress                func(adobeCountrySelection) error
	mediaProgress                  func() error
	mediaOnly, legacyMedia         bool
}

func ParseAdobeConfig(raw map[string]any) (AdobeConfig, error) {
	c := AdobeConfig{Timeout: 15 * time.Minute, RequestTimeout: 160 * time.Second}
	if registrationTarget(raw["target"]) != "adobe" {
		return c, fail("config", "adobe_target_required")
	}
	normalized := cloneMap(raw)
	normalized["target"] = "grok"
	normalized["grok"] = map[string]any{"provider": "browser"}
	mail := cloneMap(object(normalized["mail"]))
	delete(mail, "wait_timeout")
	delete(mail, "wait_interval")
	normalized["mail"] = mail
	common, err := ParseGrokConfig(normalized)
	if err != nil {
		return c, err
	}
	c.Total, c.Threads, c.Proxy = common.Total, common.Threads, registrationProxyForTarget(raw, "adobe", stringValue(raw["proxy"]))
	c.HME, c.MailboxSourceID, c.MailTimeout, c.PollInterval = common.HME, common.MailboxSourceID, common.MailTimeout, common.PollInterval
	a := object(raw["adobe"])
	if seconds := intValue(a["request_timeout"]); seconds != 0 {
		if seconds < 30 || seconds > 180 {
			return c, fail("config", "adobe_timeout_must_be_30_to_180_seconds")
		}
		c.RequestTimeout = time.Duration(seconds) * time.Second
	}
	c.WorkerURL = strings.TrimRight(firstNonEmpty(stringValue(a["api_base"]), os.Getenv("CFM_BROWSER_WORKER_URL"), os.Getenv("CFM_GROK_BROWSER_URL"), "http://cfm-browser-worker:8899"), "/")
	c.WorkerKey = firstNonEmpty(stringValue(a["api_key"]), os.Getenv("CFM_BROWSER_WORKER_TOKEN"), os.Getenv("GROK_BROWSER_TOKEN"))
	u, err := url.Parse(c.WorkerURL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return c, fail("config", "adobe_worker_url_invalid")
	}
	return c, nil
}

// Adobe registration identity is generated once per task, never from shared
// settings. The encrypted task journal retains it across result recovery.
type adobeRegistrationProfile struct {
	Country       string `json:"country"`
	CountrySource string `json:"countrySource,omitempty"`
	BirthYear     int    `json:"birthYear"`
	BirthMonth    int    `json:"birthMonth"`
	BirthDay      int    `json:"birthDay"`
}

func (p adobeRegistrationProfile) fields() map[string]any {
	fields := map[string]any{"country": p.Country, "birthYear": p.BirthYear, "birthMonth": p.BirthMonth, "birthDay": p.BirthDay}
	if p.CountrySource != "" {
		fields["countrySource"] = p.CountrySource
	}
	return fields
}

// Clamp a leap-day anniversary to the last calendar day of the target month.
func adobeBirthdayYear(date time.Time, year int) time.Time {
	day := min(date.Day(), time.Date(year, date.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day())
	return time.Date(year, date.Month(), day, 0, 0, 0, 0, time.UTC)
}

func adobeBirthdayBounds(now time.Time) (time.Time, time.Time) {
	// 51st birthday must still be in the future, and the 25th birthday must
	// already have occurred. Whole UTC dates avoid DST-dependent day lengths.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return adobeBirthdayYear(today, today.Year()-51).AddDate(0, 0, 1), adobeBirthdayYear(today, today.Year()-25)
}

func newAdobeRegistrationProfile(now time.Time, random io.Reader) (*adobeRegistrationProfile, error) {
	first, last := adobeBirthdayBounds(now)
	days := int64(last.Sub(first)/(24*time.Hour)) + 1
	offset, err := rand.Int(random, big.NewInt(days))
	if err != nil {
		return nil, fail("task", "random_failed")
	}
	birth := first.AddDate(0, 0, int(offset.Int64()))
	return &adobeRegistrationProfile{Country: "US", BirthYear: birth.Year(), BirthMonth: int(birth.Month()), BirthDay: birth.Day()}, nil
}

func normalizeAdobeSettings(value map[string]any) {
	adobe := cloneMap(object(value["adobe"]))
	for _, key := range []string{"country", "birth_year", "birth_month", "birth_day"} {
		delete(adobe, key)
	}
	value["adobe"] = adobe
}
