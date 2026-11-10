package register

import (
	"net/url"
	"os"
	"strings"
	"time"
)

type AdobeConfig struct {
	Total, Threads                                     int
	Proxy, WorkerURL, WorkerKey                        string
	Timeout, MailTimeout, PollInterval, RequestTimeout time.Duration
	MailboxSourceID                                    string
	HME                                                HMEConfig
	Profile                                            map[string]any
}

func ParseAdobeConfig(raw map[string]any) (AdobeConfig, error) {
	c := AdobeConfig{Timeout: 15 * time.Minute, RequestTimeout: 160 * time.Second}
	if registrationTarget(raw["target"]) != "adobe" {
		return c, fail("config", "adobe_target_required")
	}
	normalized := cloneMap(raw)
	normalized["target"] = "grok"
	normalized["grok"] = map[string]any{"provider": "browser"}
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
	country := firstNonEmpty(stringValue(a["country"]), "US")
	year, month := intValue(a["birth_year"]), intValue(a["birth_month"])
	if year == 0 {
		year = 1990
	}
	if month == 0 {
		month = 1
	}
	if len(country) != 2 || year < 1900 || year > time.Now().Year()-18 || month < 1 || month > 12 {
		return c, fail("config", "adobe_profile_invalid")
	}
	c.Profile = map[string]any{"country": strings.ToUpper(country), "birthYear": year, "birthMonth": month}
	return c, nil
}
