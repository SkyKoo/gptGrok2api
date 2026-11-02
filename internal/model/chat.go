package model

import "strings"

type ChatRoute struct {
	Provider       string
	Mode           string
	PoolCandidates []string
	GrokWeb        bool
	Console        bool
	OpenAI         bool
	Image          bool
}

func ResolveChat(id string) (ChatRoute, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ChatRoute{}, false
	}
	// Keep the legacy chat-completions compatibility path for image clients,
	// while the model remains hidden from the normal chat catalog.
	if id == "gpt-image-2" {
		return ChatRoute{OpenAI: true, Image: true, PoolCandidates: []string{"basic", "super", "heavy"}}, true
	}
	if strings.HasPrefix(id, "grok-") && (strings.HasSuffix(id, "-console") || strings.Contains(id, "-console-")) {
		return ChatRoute{Mode: "console", PoolCandidates: []string{"basic"}, Console: true}, true
	}
	switch id {
	case "grok-4.3-low", "grok-4.3-medium", "grok-4.3-high",
		"grok-4.20-multi-agent-low", "grok-4.20-multi-agent-medium",
		"grok-4.20-multi-agent-high", "grok-4.20-multi-agent-xhigh":
		// These console variants are exposed by the catalog without a
		// "-console" suffix. Keep them on the same native console path as
		// the suffixed variants instead of silently rejecting them.
		return ChatRoute{Mode: "console", PoolCandidates: []string{"basic"}, Console: true}, true
	}
	if mode, ok := ResolveGrokWebMode(id); ok {
		return ChatRoute{Mode: mode, PoolCandidates: []string{"basic", "super", "heavy"}, GrokWeb: true}, true
	}
	return ChatRoute{}, false
}
