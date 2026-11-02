package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGrokWebOfficialModesAndAvailability(t *testing.T) {
	raw := []byte(`{"modes":[{"id":"fast","title":"快速","availability":{"available":{}}},{"id":"expert","title":"专家","description":"深度思考 · Grok 4.7","availability":{"requiresUpgrade":{"minimumSubscriptionTier":"TIER_SUPERGROK_LITE"}}},{"id":"imagine","title":"Imagine","availability":{"available":{}}}],"checked_at":"2099-01-01T00:00:00Z"}`)
	c, err := ParseGrokWebCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Modes) != 2 || !c.CheckedAt.IsZero() {
		t.Fatal("accepted a media mode or untrusted freshness timestamp")
	}
	c.CheckedAt = time.Now()
	if !c.Supports("fast", time.Now()) || c.Supports("expert", time.Now()) || c.Supports("heavy", time.Now()) {
		t.Fatal("account permissions not respected")
	}
	if !strings.Contains(c.Modes[1].DisplayName(), "Grok 4.7") {
		t.Fatal("lost official version description")
	}
	c.CheckedAt = time.Now().Add(-7 * time.Hour)
	if c.Supports("fast", time.Now()) {
		t.Fatal("stale availability is still eligible")
	}
	c.LastAttemptAt = time.Now()
	c.LastError = "HTTP 403"
	if c.Due(time.Now().Add(time.Minute)) || !c.Due(time.Now().Add(16*time.Minute)) {
		t.Fatal("upstream refusal backoff broken")
	}
	for _, value := range []string{`null`, `false`, `{"requiresUpgrade":{}}`} {
		var availability map[string]json.RawMessage
		if value == `{"requiresUpgrade":{}}` {
			json.Unmarshal([]byte(value), &availability)
		} else {
			availability = map[string]json.RawMessage{"available": json.RawMessage(value)}
		}
		if (GrokWebMode{Availability: availability}).Available() {
			t.Fatalf("availability incorrectly accepted %s", value)
		}
	}
}
func TestGrokWebCatalogDoesNotInventVersionedModels(t *testing.T) {
	for _, m := range Catalog() {
		if m.OwnedBy != "xai" || m.Capability&Chat == 0 {
			continue
		}
		if !strings.HasPrefix(m.ID, "grok-web-") || strings.Contains(m.Name, "4.") {
			t.Fatalf("hardcoded Web version remains: %#v", m)
		}
	}
	for id, mode := range GrokWebAliases() {
		route, ok := ResolveChat(id)
		if !ok || !route.GrokWeb || route.Mode != mode || CanonicalID(id) != GrokWebID(mode) {
			t.Fatalf("legacy alias routed incorrectly: %s", id)
		}
	}
	for _, id := range []string{"grok-4.5", "grok-4.7", "grok-4.3-beta", "grok-web-imagine", "grok-web-build"} {
		if _, ok := ResolveChat(id); ok {
			t.Fatalf("unverified model was enabled: %s", id)
		}
	}
}
