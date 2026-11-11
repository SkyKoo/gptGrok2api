package adobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GenerationEntitlements keeps shared generation pools separate from credits.
// Pool identifiers are local hashes, never upstream account/resource identifiers.
type GenerationEntitlements struct {
	Status    string             `json:"status"`
	CheckedAt string             `json:"checked_at"`
	Error     string             `json:"error,omitempty"`
	Models    []ModelEntitlement `json:"models"`
	Pools     []GenerationPool   `json:"pools"`
}
type ModelEntitlement struct {
	Model        string   `json:"model"`
	Kind         string   `json:"kind"`
	Authorized   bool     `json:"authorized"`
	ReasonCode   string   `json:"reason_code,omitempty"`
	ResourceType string   `json:"resource_type,omitempty"`
	PoolID       string   `json:"pool_id,omitempty"`
	Cost         *float64 `json:"cost,omitempty"`
}
type GenerationPool struct {
	ID            string   `json:"id"`
	ResourceType  string   `json:"resource_type"`
	Total         *float64 `json:"total,omitempty"`
	Used          *float64 `json:"used,omitempty"`
	Available     *float64 `json:"available,omitempty"`
	RefreshPeriod string   `json:"refresh_period,omitempty"`
	Kinds         []string `json:"kinds"`
	Models        []string `json:"models"`
}

func freeResource(resource string) bool {
	return resource == "firefly_fair_use" || resource == "firefly_limited_taste" || resource == "firefly_complimentary_generations"
}
func poolHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:12])
}
func appendUnique(items []string, value string) []string {
	for _, old := range items {
		if old == value {
			return items
		}
	}
	return append(items, value)
}

// GenerationPermissions reads all admitted model defaults without generating or
// reserving quota. Individual submissions still require their exact preflight.
func (c *Client) GenerationPermissions(ctx context.Context, a Account) (GenerationEntitlements, error) {
	out := GenerationEntitlements{Status: "unknown", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Models: []ModelEntitlement{}, Pools: []GenerationPool{}}
	if id, _, err := TokenIdentity(a.Token); err != nil || id != a.AccountID {
		return out, ErrIdentity
	}
	models := FreeModels()
	features := make([]any, 0, len(models))
	byFeature := map[string]Model{}
	for _, m := range models {
		params := map[string]any{}
		if m.Kind == "image" {
			if m.Resolution != "" {
				params["imageResolution"] = m.Resolution
			}
			if m.ModelID == "gpt-image" {
				params["imageResolution"] = "1K"
				params["quality"] = "medium"
			}
		} else {
			if m.Duration > 0 {
				params["videoDuration"] = strconv.Itoa(m.Duration) + ".0"
			}
			if m.Resolution != "" {
				params["videoResolution"] = m.Resolution
			}
			for _, v := range m.Variants {
				if v.Audio || m.DefaultAudio {
					params["generateAudio"] = m.DefaultAudio
					break
				}
			}
		}
		features = append(features, map[string]any{"featureId": m.FeatureID, "occurrences": 1, "params": params})
		byFeature[m.FeatureID] = m
	}
	raw, _ := json.Marshal(map[string]any{"features": features})
	data, _, err := c.request(ctx, "entitlements", http.MethodPost, "https://bks.adobe.io/v1/feature-auth-profile?include-feature-cost=true&include-effective-quota-balance=true", a, raw, map[string]string{"x-request-id": requestID()})
	if err != nil {
		out.Status = "error"
		out.Error = SafeError(err)
		return out, err
	}
	profiles, ok := data["featuresAuthorization"].([]any)
	if !ok {
		return out, &Error{Stage: "entitlements", Code: "invalid_response"}
	}
	quotas, ok := data["effectiveQuotas"].([]any)
	if !ok {
		return out, &Error{Stage: "entitlements", Code: "invalid_response"}
	}
	pools := map[string]int{}
	for _, value := range quotas {
		q := object(value)
		ref := str(q["effectiveQuotaReferenceId"])
		resource := str(q["resourceType"])
		if ref == "" || !freeResource(resource) {
			continue
		}
		if _, exists := pools[ref]; exists {
			return out, &Error{Stage: "entitlements", Code: "invalid_response"}
		}
		p := GenerationPool{ID: poolHash(ref), ResourceType: resource, Total: finiteNumber(q["provisionedQuota"]), Used: finiteNumber(q["consumedQuota"]), Kinds: []string{}, Models: []string{}}
		if p.Total != nil && p.Used != nil {
			available := max(0, *p.Total-*p.Used)
			p.Available = &available
		}
		if period := str(q["refreshPeriod"]); period == "P1D" || period == "P1M" {
			p.RefreshPeriod = period
		}
		pools[ref] = len(out.Pools)
		out.Pools = append(out.Pools, p)
	}
	seen := map[string]bool{}
	for _, value := range profiles {
		p := object(value)
		feature := str(p["featureId"])
		m, known := byFeature[feature]
		if !known || seen[feature] {
			return out, &Error{Stage: "entitlements", Code: "invalid_response"}
		}
		seen[feature] = true
		auth := object(p["authorizationResult"])
		result := str(auth["result"])
		if result != "authorized" && !strings.HasPrefix(result, "unauthorized_") {
			return out, &Error{Stage: "entitlements", Code: "invalid_response"}
		}
		resource := object(p["selectedResource"])
		typ := str(resource["resourceType"])
		if result == "authorized" && typ == "" {
			return out, &Error{Stage: "entitlements", Code: "invalid_response"}
		}
		item := ModelEntitlement{Model: m.ID, Kind: m.Kind, Cost: finiteNumber(resource["computedCost"])}
		switch str(auth["reasonCode"]) {
		case "quota_available", "quota_exhausted", "user_not_entitled", "quota_not_available":
			item.ReasonCode = str(auth["reasonCode"])
		}
		if freeResource(typ) {
			item.ResourceType = typ
			item.Authorized = str(auth["result"]) == "authorized"
		}
		if index, exists := pools[str(resource["effectiveQuotaReferenceId"])]; exists && out.Pools[index].ResourceType == item.ResourceType {
			pool := &out.Pools[index]
			item.PoolID = pool.ID
			pool.Kinds = appendUnique(pool.Kinds, m.Kind)
			pool.Models = appendUnique(pool.Models, m.ID)
		}
		out.Models = append(out.Models, item)
	}
	if len(seen) != len(byFeature) {
		return out, &Error{Stage: "entitlements", Code: "invalid_response"}
	}
	out.Status = "known"
	return out, nil
}

func (s *Store) UpdateGenerationPermissions(id string, revision uint64, value GenerationEntitlements) error {
	return s.update(id, revision, func(a *Account) { a.Quota.Generation = &value })
}

// Unknown counters never mean zero; exact request preflight remains authoritative.
func (e GenerationEntitlements) AnyAvailable() bool {
	for _, m := range e.Models {
		if !m.Authorized {
			continue
		}
		if m.PoolID == "" {
			return true
		}
		found := false
		for _, p := range e.Pools {
			if p.ID != m.PoolID {
				continue
			}
			found = true
			if p.Available == nil {
				return true
			}
			if m.Cost != nil && *p.Available >= *m.Cost {
				return true
			}
			if m.Cost == nil && *p.Available > 0 {
				return true
			}
		}
		if !found {
			return true
		}
	}
	return false
}

// Exhausted reports only explicit zero balances, not missing entitlement data.
func (e GenerationEntitlements) Exhausted() bool {
	if e.Status != "known" || len(e.Pools) == 0 || e.AnyAvailable() {
		return false
	}
	for _, pool := range e.Pools {
		if pool.Available == nil || *pool.Available > 0 {
			return false
		}
	}
	return true
}

// ModelAvailable excludes only a freshly observed unavailable default for this
// model. Missing or stale evidence still requires the exact request preflight.
func (e GenerationEntitlements) ModelAvailable(model string, now time.Time) bool {
	checked, err := time.Parse(time.RFC3339Nano, e.CheckedAt)
	if e.Status != "known" || err != nil || checked.After(now) || now.Sub(checked) > 5*time.Minute {
		return true
	}
	for _, m := range e.Models {
		if m.Model != model {
			continue
		}
		if !m.Authorized {
			return false
		}
		for _, p := range e.Pools {
			if m.PoolID == "" || p.ID != m.PoolID || p.Available == nil {
				continue
			}
			if m.Cost != nil {
				return *p.Available >= *m.Cost
			}
			return *p.Available > 0
		}
		return true
	}
	return true
}
