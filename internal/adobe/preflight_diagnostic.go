package adobe

import "errors"

// PreflightDiagnostic describes the exact request's authorization response.
// It contains no prompt, input bytes, upstream pool IDs or arbitrary strings.
type PreflightDiagnostic struct {
	Authorization string `json:"authorization"`
	ResourceType  string `json:"resource_type"`
	ReasonCode    string `json:"reason_code"`
	FeatureMatch  bool   `json:"feature_match"`
	ErrorCode     string `json:"error_code"`
}

func preflightDiagnostic(data map[string]any, feature string) *PreflightDiagnostic {
	auth := object(data["authorizationResult"])
	return sanitizePreflightDiagnostic(&PreflightDiagnostic{
		Authorization: str(auth["result"]),
		ResourceType:  str(object(data["selectedResource"])["resourceType"]),
		ReasonCode:    str(auth["reasonCode"]),
		FeatureMatch:  str(data["featureId"]) == feature,
	})
}

func sanitizePreflightDiagnostic(d *PreflightDiagnostic) *PreflightDiagnostic {
	if d == nil {
		return nil
	}
	copy := *d
	category := func(value string, allowed ...string) string {
		if value == "" {
			return "missing"
		}
		for _, item := range allowed {
			if value == item {
				return value
			}
		}
		return "unknown"
	}
	copy.Authorization = category(copy.Authorization, "authorized", "unauthorized_forbidden", "missing")
	copy.ResourceType = category(copy.ResourceType, "firefly_fair_use", "firefly_limited_taste", "firefly_complimentary_generations", "generative_credits", "missing")
	copy.ReasonCode = category(copy.ReasonCode, "quota_available", "quota_exhausted", "user_not_entitled", "quota_not_available", "missing")
	copy.ErrorCode = category(copy.ErrorCode, "invalid_response", "invalid_quota", "free_entitlement_unavailable", "free_quota_exhausted", "missing")
	return &copy
}

func PreflightDiagnosticFromError(err error) *PreflightDiagnostic {
	var upstream *Error
	if errors.As(err, &upstream) && upstream.Stage == "preflight" {
		return sanitizePreflightDiagnostic(upstream.PreflightDiagnostic)
	}
	return nil
}
