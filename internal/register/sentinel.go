package register

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/browserprofile"
)

// SentinelRunnerOptions describes the browser/session context used to execute
// the vendored Sentinel runner. The challenge and cookies must belong to the
// same upstream session as the request that will carry the returned headers.
type SentinelRunnerOptions struct {
	Flow            string
	Requirements    string
	SentinelSID     string
	Cookie          string
	DeviceID        string
	SentinelOrigin  string
	SentinelVersion string
	ScriptSrc       string
	PageURL         string
	CookieHeader    string
	Profile         browserprofile.Profile
	Challenge       map[string]any
}

// SentinelHeadersFromRunner runs the vendored Turb-compatible runner. The
// runner loads sdk.js itself and returns the Sentinel headers for one request.
// It intentionally accepts a plain browser profile so OAuth and registration
// flows can share exactly the same JS implementation without sharing sessions.
func SentinelHeadersFromRunner(ctx context.Context, options SentinelRunnerOptions) (map[string]string, error) {
	runner := os.Getenv("CFM_SENTINEL_RUNNER")
	if runner == "" {
		runner = "/app/sentinel/sentinel-runner.js"
	}
	sdk := os.Getenv("CFM_SENTINEL_SDK")
	if sdk == "" {
		sdk = "/app/sentinel/sdk.js"
	}
	node := os.Getenv("CFM_SENTINEL_NODE")
	if node == "" {
		node = "node"
	}
	if _, err := os.Stat(runner); err != nil {
		return nil, fmt.Errorf("sentinel runner unavailable: %w", err)
	}
	if _, err := os.Stat(sdk); err != nil {
		return nil, fmt.Errorf("sentinel sdk unavailable: %w", err)
	}
	challengeFile, err := os.CreateTemp("", "cfm-sentinel-*.json")
	if err != nil {
		return nil, err
	}
	challengePath := challengeFile.Name()
	defer os.Remove(challengePath)
	defer challengeFile.Close()
	_ = challengeFile.Chmod(0600)
	if err := json.NewEncoder(challengeFile).Encode(options.Challenge); err != nil {
		return nil, err
	}
	if err := challengeFile.Close(); err != nil {
		return nil, err
	}

	origin := options.SentinelOrigin
	if origin == "" {
		origin = sentinelOrigin
	}
	version := options.SentinelVersion
	if version == "" {
		version = sentinelVersion
	}
	pageURL := options.PageURL
	if pageURL == "" {
		pageURL = "https://auth.openai.com/about-you"
	}
	scriptSrc := options.ScriptSrc
	if scriptSrc == "" {
		scriptSrc = origin + "/sentinel/" + version + "/sdk.js"
	}
	cookieHeader := options.CookieHeader
	if cookieHeader == "" {
		cookieHeader = "oai-did=" + options.DeviceID + "; oai-sc=" + options.Cookie
	}
	profile := options.Profile
	runnerCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	args := []string{
		runner,
		"--challenge-file", challengePath,
		"--flow", options.Flow,
		"--device-id", options.DeviceID,
		"--sentinel-sid", options.SentinelSID,
		"--challenge-proof", options.Requirements,
		"--sdk", sdk,
		"--script-src", scriptSrc,
		"--page-url", pageURL,
		"--user-agent", profile.UserAgent,
		"--browser-family", profile.BrowserFamily,
		"--navigator-platform", profile.NavigatorPlatform,
		"--navigator-vendor", profile.NavigatorVendor,
		"--user-agent-data-platform", profile.UserAgentDataPlatform,
		"--request-idle-callback", "0",
		"--width", strconv.Itoa(profile.ScreenWidth),
		"--height", strconv.Itoa(profile.ScreenHeight),
		"--avail-width", strconv.Itoa(profile.AvailWidth),
		"--avail-height", strconv.Itoa(profile.AvailHeight),
		"--outer-width", strconv.Itoa(profile.OuterWidth),
		"--outer-height", strconv.Itoa(profile.OuterHeight),
		"--inner-width", strconv.Itoa(profile.InnerWidth),
		"--inner-height", strconv.Itoa(profile.InnerHeight),
		"--cores", strconv.Itoa(profile.HardwareConcurrency),
		"--js-heap-size-limit", strconv.FormatInt(profile.JSHeapSizeLimit, 10),
		"--device-memory", strconv.Itoa(profile.DeviceMemory),
		"--device-pixel-ratio", strconv.Itoa(profile.DevicePixelRatio),
		"--chrome-major", profile.ChromeMajor,
		"--chrome-full-version", profile.ChromeFullVersion,
		"--sec-ch-ua", profile.SecCHUA,
		"--sec-ch-ua-full-version-list", profile.SecCHUAFullVersionList,
		"--sec-ch-ua-platform", profile.SecCHUAPlatform,
		"--sec-ch-ua-platform-version", profile.SecCHUAPlatformVersion,
		"--sec-ch-ua-arch", profile.SecCHUAArch,
		"--sec-ch-ua-bitness", profile.SecCHUABitness,
		"--sec-ch-ua-model", profile.SecCHUAModel,
		"--language", profile.NavigatorLanguage,
		"--languages", profile.NavigatorLanguagesValue(),
		"--time-zone", profile.TimezoneIANA,
		"--timezone-name", profile.TimezoneName,
		"--timezone-offset-minutes", strconv.Itoa(profile.TimezoneOffsetMinutes),
		"--cookie", cookieHeader,
	}
	out, err := exec.CommandContext(runnerCtx, node, args...).Output()
	if err != nil {
		if runnerCtx.Err() != nil {
			return nil, fmt.Errorf("sentinel runner timeout")
		}
		return nil, fmt.Errorf("sentinel runner failed")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("sentinel runner returned invalid token")
	}
	if len(payload["p"]) == 0 || len(payload["c"]) == 0 {
		return nil, fmt.Errorf("sentinel runner token is incomplete")
	}
	soRaw, hasSO := payload["_so"]
	delete(payload, "_so")
	delete(payload, "so")
	mainToken, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"openai-sentinel-token": string(mainToken)}
	if !hasSO || len(soRaw) == 0 || string(soRaw) == "null" {
		return headers, nil
	}
	var soValue any
	if err := json.Unmarshal(soRaw, &soValue); err != nil {
		soValue = string(soRaw)
	}
	if nested, ok := soValue.(map[string]any); ok {
		if inner, exists := nested["so"]; exists {
			soValue = inner
		}
	}
	var challengeID string
	if err := json.Unmarshal(payload["c"], &challengeID); err != nil || challengeID == "" {
		return nil, fmt.Errorf("sentinel runner token has no challenge id")
	}
	soToken, err := json.Marshal(map[string]any{"so": soValue, "c": challengeID, "id": options.DeviceID, "flow": options.Flow})
	if err != nil {
		return nil, err
	}
	headers["openai-sentinel-so-token"] = string(soToken)
	return headers, nil
}

// sentinelHeadersFromRunner runs the same SDK path Turb uses for the
// oauth_create_account flow. The runner returns the main token plus an
// optional _so payload produced by sessionObserverToken. The main header is
// still valid when the upstream challenge does not require session observer.
func (r *WebRegistrar) sentinelHeadersFromRunner(ctx context.Context, flow, requirements, sentinelSID, cookie string, challenge map[string]any) (map[string]string, error) {
	return SentinelHeadersFromRunner(ctx, SentinelRunnerOptions{
		Flow: flow, Requirements: requirements, SentinelSID: sentinelSID,
		Cookie: cookie, DeviceID: r.device, SentinelOrigin: r.sentinel,
		SentinelVersion: sentinelVersion, ScriptSrc: r.sentinel + "/sentinel/" + sentinelVersion + "/sdk.js", PageURL: r.auth + "/about-you",
		Profile: browserprofile.Profile(r.BrowserProfile()), Challenge: challenge,
	})
}
