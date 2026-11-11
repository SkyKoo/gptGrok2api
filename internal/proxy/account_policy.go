package proxy

import (
	"encoding/json"
	"strings"
)

const (
	AccountProxyInherit  = "inherit"
	AccountProxyExplicit = "explicit"
)

// This exact reason identifies the former bulk migration, not an arbitrary
// administrator choosing direct routing for an individual account.
const legacyGrokDirectReason = "用户明确选择日常调用直连；注册继续使用原动态家宽，2026-11-02"

// IsLegacyAutomaticAccountRoute recognizes routes previously copied from a
// registration task or assigned by the old Grok import policy. Unknown policies,
// malformed metadata and any other manual binding history remain overrides.
func IsLegacyAutomaticAccountRoute(fields map[string]any) bool {
	if strings.TrimSpace(stringValue(fields["proxy_policy"])) != "" {
		return false
	}
	binding := ParseBinding(fields["proxy_binding"])
	if binding == nil {
		return false
	}
	history, valid := accountBindingHistory(fields["proxy_binding_history"])
	if !valid {
		return false
	}
	for _, raw := range history {
		entry, ok := raw.(map[string]any)
		reason, stringReason := entry["reason"].(string)
		if !ok || !stringReason || reason != legacyGrokDirectReason {
			return false
		}
	}
	if binding.Purpose == "registration" {
		return len(history) == 0
	}
	if binding.Platform != "grok" || binding.Purpose != "account" || binding.Reference != "direct" {
		return false
	}
	return len(history) > 0 || strings.TrimSpace(stringValue(fields["registration_job_id"])) != "" || stringValue(fields["source_type"]) == "grok_protocol"
}

// AccountInheritsDefault reports the daily account policy. It does not describe
// registration tasks: AcquireTask continues to honor their saved bindings.
func AccountInheritsDefault(fields map[string]any) bool {
	switch strings.TrimSpace(stringValue(fields["proxy_policy"])) {
	case AccountProxyInherit:
		return true
	case "":
	default:
		return false
	}
	if IsLegacyAutomaticAccountRoute(fields) {
		return true
	}
	if fields["proxy_binding"] != nil {
		return false
	}
	for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
		if strings.TrimSpace(stringValue(fields[key])) != "" {
			return false
		}
	}
	return true
}

// AccountRoutingFields returns a read-only routing view without changing stored
// account metadata. Inheritance removes the old route and its policy marker so
// an internally selected default can subsequently be resolved as an explicit
// request route without being discarded again.
func AccountRoutingFields(fields map[string]any) map[string]any {
	if fields == nil || !AccountInheritsDefault(fields) {
		return fields
	}
	effective := make(map[string]any, len(fields))
	for key, value := range fields {
		switch key {
		case "proxy", "proxy_url", "proxyUrl", "proxy_binding", "proxy_policy":
			continue
		}
		effective[key] = value
	}
	return effective
}

func accountBindingHistory(value any) ([]any, bool) {
	if value == nil {
		return nil, true
	}
	// JSON-backed stores usually supply []any; marshaling also handles typed
	// in-memory fixtures without accepting an object as an empty history.
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var history []any
	if json.Unmarshal(raw, &history) != nil {
		return nil, false
	}
	return history, true
}
