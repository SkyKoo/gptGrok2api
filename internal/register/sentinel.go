package register

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// sentinelHeadersFromRunner runs the same SDK path Turb uses for the
// oauth_create_account flow. The runner returns the main token plus an
// optional _so payload produced by sessionObserverToken.
func (r *WebRegistrar) sentinelHeadersFromRunner(ctx context.Context, flow, requirements, sentinelSID, cookie string, challenge map[string]any) (map[string]string, error) {
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
	if err := json.NewEncoder(challengeFile).Encode(challenge); err != nil {
		return nil, err
	}
	if err := challengeFile.Close(); err != nil {
		return nil, err
	}

	runnerCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	args := []string{
		runner,
		"--challenge-file", challengePath,
		"--flow", flow,
		"--device-id", r.device,
		"--sentinel-sid", sentinelSID,
		"--challenge-proof", requirements,
		"--sdk", sdk,
		"--script-src", r.sentinel + "/sentinel/" + sentinelVersion + "/sdk.js",
		"--page-url", r.auth + "/about-you",
		"--user-agent", r.profile.UserAgent,
		"--browser-family", r.profile.BrowserFamily,
		"--navigator-platform", r.profile.NavigatorPlatform,
		"--navigator-vendor", r.profile.NavigatorVendor,
		"--user-agent-data-platform", r.profile.UserAgentDataPlatform,
		"--request-idle-callback", "0",
		"--width", strconv.Itoa(r.profile.ScreenWidth),
		"--height", strconv.Itoa(r.profile.ScreenHeight),
		"--avail-width", strconv.Itoa(r.profile.AvailWidth),
		"--avail-height", strconv.Itoa(r.profile.AvailHeight),
		"--outer-width", strconv.Itoa(r.profile.OuterWidth),
		"--outer-height", strconv.Itoa(r.profile.OuterHeight),
		"--inner-width", strconv.Itoa(r.profile.InnerWidth),
		"--inner-height", strconv.Itoa(r.profile.InnerHeight),
		"--cores", strconv.Itoa(r.profile.HardwareConcurrency),
		"--js-heap-size-limit", strconv.FormatInt(r.profile.JSHeapSizeLimit, 10),
		"--device-memory", strconv.Itoa(r.profile.DeviceMemory),
		"--device-pixel-ratio", strconv.Itoa(r.profile.DevicePixelRatio),
		"--chrome-major", r.profile.ChromeMajor,
		"--chrome-full-version", r.profile.ChromeFullVersion,
		"--sec-ch-ua", r.profile.SecCHUA,
		"--sec-ch-ua-full-version-list", r.profile.SecCHUAFullVersionList,
		"--sec-ch-ua-platform", r.profile.SecCHUAPlatform,
		"--sec-ch-ua-platform-version", r.profile.SecCHUAPlatformVersion,
		"--sec-ch-ua-arch", r.profile.SecCHUAArch,
		"--sec-ch-ua-bitness", r.profile.SecCHUABitness,
		"--sec-ch-ua-model", r.profile.SecCHUAModel,
		"--language", r.profile.NavigatorLanguage,
		"--languages", r.profile.navigatorLanguagesValue(),
		"--time-zone", r.profile.TimezoneIANA,
		"--timezone-name", r.profile.TimezoneName,
		"--timezone-offset-minutes", strconv.Itoa(r.profile.TimezoneOffsetMinutes),
		"--cookie", "oai-did=" + r.device + "; oai-sc=" + cookie,
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
	soRaw, ok := payload["_so"]
	if !ok || len(soRaw) == 0 || string(soRaw) == "null" {
		return nil, fmt.Errorf("sentinel runner returned no session observer token")
	}
	delete(payload, "_so")
	delete(payload, "so")
	mainToken, err := json.Marshal(payload)
	if err != nil {
		return nil, err
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
	var c string
	if err := json.Unmarshal(payload["c"], &c); err != nil || c == "" {
		return nil, fmt.Errorf("sentinel runner token has no challenge id")
	}
	soToken, err := json.Marshal(map[string]any{"so": soValue, "c": c, "id": r.device, "flow": flow})
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"openai-sentinel-token":    string(mainToken),
		"openai-sentinel-so-token": string(soToken),
	}, nil
}
