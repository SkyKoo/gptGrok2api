package register

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
		"--user-agent", registrationUA,
		"--browser-family", "chrome",
		"--navigator-platform", "MacIntel",
		"--navigator-vendor", "Google Inc.",
		"--user-agent-data-platform", "macOS",
		"--request-idle-callback", "0",
		"--width", "1920",
		"--height", "1080",
		"--avail-width", "1920",
		"--avail-height", "1080",
		"--outer-width", "1920",
		"--outer-height", "1080",
		"--inner-width", "1920",
		"--inner-height", "993",
		"--cores", "8",
		"--js-heap-size-limit", "4294705152",
		"--language", "en-US",
		"--languages", "en-US,en",
		"--time-zone", "UTC",
		"--timezone-name", "Coordinated Universal Time",
		"--timezone-offset-minutes", "0",
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
