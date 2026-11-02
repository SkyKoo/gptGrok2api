package model

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const GrokWebCatalogTTL = 6 * time.Hour

// Web exposes modes, not a promise to keep serving a fixed model version.
// Version descriptions are read from /rest/modes rather than inferred from IDs.
type GrokWebMode struct {
	ID           string                     `json:"id"`
	Title        string                     `json:"title"`
	Description  string                     `json:"description"`
	Availability map[string]json.RawMessage `json:"availability"`
}
type GrokWebCatalog struct {
	Modes         []GrokWebMode `json:"modes"`
	DefaultModeID string        `json:"defaultModeId"`
	CheckedAt     time.Time     `json:"checked_at"`
	LastAttemptAt time.Time     `json:"last_attempt_at"`
	LastError     string        `json:"last_error,omitempty"`
}

func GrokWebID(mode string) string { return "grok-web-" + mode }
func IsGrokWebMode(mode string) bool {
	switch mode {
	case "auto", "fast", "expert", "heavy":
		return true
	}
	return false
}
func GrokWebAliases() map[string]string {
	return map[string]string{
		"grok-4.20-0309-non-reasoning": "fast", "grok-4.20-fast": "fast", "grok-4.3-fast": "fast",
		"grok-4.20-0309-non-reasoning-super": "fast", "grok-4.20-0309-non-reasoning-heavy": "fast",
		"grok-4.20-0309": "auto", "grok-4.20-auto": "auto", "grok-4.20-0309-super": "auto",
		"grok-4.20-0309-heavy": "heavy", "grok-4.20-heavy": "heavy", "grok-4.20-multi-agent-0309": "heavy",
		"grok-4.20-0309-reasoning": "expert", "grok-4.20-expert": "expert",
		"grok-4.20-0309-reasoning-super": "expert", "grok-4.20-0309-reasoning-heavy": "expert",
	}
}
func ResolveGrokWebMode(id string) (string, bool) {
	if mode, ok := GrokWebAliases()[id]; ok {
		return mode, true
	}
	mode := strings.TrimPrefix(id, "grok-web-")
	return mode, strings.HasPrefix(id, "grok-web-") && IsGrokWebMode(mode)
}
func ParseGrokWebCatalog(raw []byte) (GrokWebCatalog, error) {
	var c GrokWebCatalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, errors.New("invalid Grok Web model directory")
	}
	seen := map[string]bool{}
	modes := []GrokWebMode{}
	for _, m := range c.Modes {
		if !IsGrokWebMode(m.ID) || seen[m.ID] {
			continue
		}
		if len(m.Title) > 200 || len(m.Description) > 1000 {
			return GrokWebCatalog{}, errors.New("invalid Grok Web mode description")
		}
		seen[m.ID] = true
		modes = append(modes, m)
	}
	if len(modes) == 0 {
		return GrokWebCatalog{}, errors.New("Grok Web directory contains no supported chat modes")
	}
	c.Modes = modes
	// Timestamp fields from the upstream must never control cache freshness.
	c.CheckedAt = time.Time{}
	c.LastAttemptAt = time.Time{}
	c.LastError = ""
	return c, nil
}
func DecodeGrokWebCatalog(value any) GrokWebCatalog {
	raw, _ := json.Marshal(value)
	var c GrokWebCatalog
	_ = json.Unmarshal(raw, &c)
	return c
}
func (c GrokWebCatalog) Fresh(now time.Time) bool {
	return !c.CheckedAt.IsZero() && !c.CheckedAt.After(now.Add(time.Minute)) && now.Sub(c.CheckedAt) < GrokWebCatalogTTL
}
func (c GrokWebCatalog) Due(now time.Time) bool {
	if c.Fresh(now) {
		return false
	}
	delay := time.Minute
	if c.LastError != "" {
		delay = 15 * time.Minute
	}
	return c.LastAttemptAt.IsZero() || now.Sub(c.LastAttemptAt) >= delay
}
func (m GrokWebMode) Available() bool {
	// The official API represents available as an empty object, not true.
	available, ok := m.Availability["available"]
	if !ok || string(available) == "null" || string(available) == "false" {
		return false
	}
	for _, key := range []string{"requiresUpgrade", "unavailable", "comingSoon"} {
		if v, ok := m.Availability[key]; ok && string(v) != "null" {
			return false
		}
	}
	return string(available) == "true" || strings.HasPrefix(strings.TrimSpace(string(available)), "{")
}
func (c GrokWebCatalog) Supports(mode string, now time.Time) bool {
	if !c.Fresh(now) {
		return false
	}
	for _, m := range c.Modes {
		if m.ID == mode {
			return m.Available()
		}
	}
	return false
}
func (m GrokWebMode) DisplayName() string {
	title := strings.TrimSpace(m.Title)
	if title == "" {
		title = m.ID
	}
	name := "Grok Web · " + title
	if d := strings.TrimSpace(m.Description); d != "" {
		name += "（" + d + "）"
	}
	return name
}
