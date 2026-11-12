package adobe

import (
	"encoding/json"
	"errors"
	"math"
)

// MediaPreparationDiagnostic contains only bounded runtime observations. It
// never carries a URL, SDK instance/proof, vendor payload, token or cookie scope.
type MediaPreparationDiagnostic struct {
	State           string `json:"state,omitempty"`
	ReadinessReason string `json:"readiness_reason,omitempty"`
	ValueKind       string `json:"value_kind,omitempty"`
	VendorsStarted  *int   `json:"vendors_started,omitempty"`
	VendorsComplete *int   `json:"vendors_complete,omitempty"`
}

// SanitizeMediaPreparationDiagnostic is the sole projection for Worker input,
// typed errors, persistence and monitor output. Unknown fields and values drop.
func SanitizeMediaPreparationDiagnostic(value any) *MediaPreparationDiagnostic {
	var fields map[string]any
	switch input := value.(type) {
	case json.RawMessage:
		if len(input) > 8192 || json.Unmarshal(input, &fields) != nil {
			return nil
		}
	case map[string]any:
		fields = input
	case *MediaPreparationDiagnostic:
		if input == nil {
			return nil
		}
		fields = map[string]any{"state": input.State, "readiness_reason": input.ReadinessReason, "value_kind": input.ValueKind}
		if input.VendorsStarted != nil {
			fields["vendors_started"] = *input.VendorsStarted
		}
		if input.VendorsComplete != nil {
			fields["vendors_complete"] = *input.VendorsComplete
		}
	default:
		return nil
	}
	diagnostic := &MediaPreparationDiagnostic{}
	if state, ok := fields["state"].(string); ok {
		switch state {
		case "unavailable", "pending", "invalid", "invalid_contract", "changed", "observation_timeout", "ready", "editor_not_ready", "observer_unavailable", "context_changed":
			diagnostic.State = state
		}
	}
	if reason, ok := fields["readiness_reason"].(string); ok {
		switch reason {
		case "performance_unavailable", "instance_missing", "instance_ambiguous", "vendor_unrecognized", "vendors_pending", "token_settling", "editor_not_ready", "observer_unavailable", "context_changed", "identity_pending", "identity_changed", "runtime_changed", "verification_timeout",
			"authentication_missing", "arp_observer_missing", "editor_route_changed", "page_hidden", "observation_cancelled", "evaluation_failed", "navigation_budget_exhausted":
			diagnostic.ReadinessReason = reason
		}
	}
	if kind, ok := fields["value_kind"].(string); ok {
		switch kind {
		case "unobserved", "null", "undefined", "empty_string":
			diagnostic.ValueKind = kind
		}
	}
	count := func(value any) *int {
		number := -1.0
		switch v := value.(type) {
		case float64:
			number = v
		case int:
			number = float64(v)
		}
		if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > 3 || math.Trunc(number) != number {
			return nil
		}
		integer := int(number)
		return &integer
	}
	diagnostic.VendorsStarted = count(fields["vendors_started"])
	diagnostic.VendorsComplete = count(fields["vendors_complete"])
	if diagnostic.VendorsStarted != nil && diagnostic.VendorsComplete != nil && *diagnostic.VendorsComplete > *diagnostic.VendorsStarted {
		diagnostic.VendorsStarted, diagnostic.VendorsComplete = nil, nil
	}
	if diagnostic.State == "" && diagnostic.ReadinessReason == "" && diagnostic.ValueKind == "" && diagnostic.VendorsStarted == nil && diagnostic.VendorsComplete == nil {
		return nil
	}
	return diagnostic
}

func MediaDiagnosticFromError(err error) *MediaPreparationDiagnostic {
	var failure *Error
	if errors.As(err, &failure) {
		return SanitizeMediaPreparationDiagnostic(failure.MediaDiagnostic)
	}
	return nil
}
