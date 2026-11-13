package adobe

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"math"
	"net/http"
	"net/url"
	"regexp"
)

// GenerationQuota is the selected free pool, which may be shared by models.
// The upstream pool identifier is private and must not be exposed as a credential.
type GenerationQuota struct {
	ReferenceID   string   `json:"-"`
	ResourceType  string   `json:"resource_type"`
	Provisioned   *float64 `json:"provisioned,omitempty"`
	Consumed      *float64 `json:"consumed,omitempty"`
	RefreshPeriod string   `json:"refresh_period,omitempty"`
}
type GenerationPermission struct {
	FeatureID    string           `json:"feature_id"`
	Authorized   bool             `json:"authorized"`
	Free         bool             `json:"free"`
	ReasonCode   string           `json:"reason_code,omitempty"`
	ResourceType string           `json:"resource_type,omitempty"`
	Cost         *float64         `json:"cost,omitempty"`
	Quota        *GenerationQuota `json:"quota,omitempty"`
}

func referenceMetadata(m Model, references []ReferenceImage) []any {
	out := make([]any, 0, len(references))
	for i, r := range references {
		cfg, _, _ := image.DecodeConfig(bytes.NewReader(r.Data))
		entry := map[string]any{"usage": m.ReferenceUsage, "metadata": map[string]any{"mimeType": http.DetectContentType(r.Data), "blobSize": len(r.Data), "size": map[string]int{"width": cfg.Width, "height": cfg.Height}}}
		if m.ReferenceUsage == "frame" {
			entry["order"] = i + 1
		}
		out = append(out, entry)
	}
	return out
}
func (c *Client) PreflightImage(ctx context.Context, a Account, r ImageRequest) (GenerationPermission, error) {
	m, v, err := ValidateImageRequest(r)
	if err != nil {
		return GenerationPermission{}, err
	}
	return c.preflight(ctx, a, m, imagePayload(m, v, r, nil), r.References)
}
func (c *Client) PreflightVideo(ctx context.Context, a Account, r VideoRequest) (GenerationPermission, error) {
	m, v, err := ValidateVideoRequest(r)
	if err != nil {
		return GenerationPermission{}, err
	}
	inputs, err := fitVideoReferences(r.References, r.Width, r.Height)
	if err != nil {
		return GenerationPermission{}, err
	}
	if err = validateReferences(m, inputs); err != nil {
		return GenerationPermission{}, err
	}
	return c.preflight(ctx, a, m, videoPayload(m, v, r, nil), inputs)
}
func (c *Client) preflight(ctx context.Context, a Account, m Model, payload map[string]any, references []ReferenceImage) (GenerationPermission, error) {
	if id, _, err := TokenIdentity(a.Token); err != nil || id != a.AccountID {
		return GenerationPermission{}, ErrIdentity
	}
	delete(payload, "referenceBlobs")
	if _, present := payload["seeds"]; present {
		payload["seeds"] = []int{1}
	}
	if len(references) > 0 {
		payload["referenceBlobsMetadata"] = referenceMetadata(m, references)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return GenerationPermission{}, &Error{Stage: "preflight", Code: "invalid_request"}
	}
	endpoint := "https://firefly-3p.ff.adobe.io/v2/models/generation-preflight?api=" + url.QueryEscape("/v2/3p-"+m.Kind+"s/generate-async")
	data, _, err := c.request(ctx, "preflight", http.MethodPost, endpoint, a, raw, map[string]string{"x-request-id": requestID()})
	if err != nil {
		return GenerationPermission{}, err
	}
	diagnostic := preflightDiagnostic(data, m.FeatureID)
	failure := func(code string) error {
		diagnostic.ErrorCode = code
		return &Error{Stage: "preflight", Code: code, PreflightDiagnostic: diagnostic}
	}
	permission := GenerationPermission{FeatureID: str(data["featureId"])}
	if permission.FeatureID == "" || permission.FeatureID != m.FeatureID {
		return permission, failure("invalid_response")
	}
	auth := object(data["authorizationResult"])
	result := str(auth["result"])
	if result == "" {
		return permission, failure("invalid_response")
	}
	if reason := str(auth["reasonCode"]); regexp.MustCompile(`^[a-z][a-z0-9_]{0,79}$`).MatchString(reason) {
		permission.ReasonCode = reason
	}
	permission.Authorized = result == "authorized"
	selected := object(data["selectedResource"])
	resource := str(selected["resourceType"])
	switch resource {
	case "firefly_fair_use", "firefly_limited_taste", "firefly_complimentary_generations":
		permission.ResourceType = resource
		permission.Free = permission.Authorized
	}
	permission.Cost = finiteNumber(selected["computedCost"])
	// Reject a paid or unavailable selection before inspecting a quota whose
	// resource type was intentionally not admitted into the free permission.
	if !permission.Free {
		return permission, failure("free_entitlement_unavailable")
	}
	ref := str(selected["effectiveQuotaReferenceId"])
	quotas, _ := data["effectiveQuotas"].([]any)
	for _, raw := range quotas {
		q := object(raw)
		if ref == "" || str(q["effectiveQuotaReferenceId"]) != ref {
			continue
		}
		if str(q["resourceType"]) != permission.ResourceType {
			return permission, failure("invalid_quota")
		}
		permission.Quota = &GenerationQuota{ReferenceID: ref, ResourceType: permission.ResourceType, Provisioned: finiteNumber(q["provisionedQuota"]), Consumed: finiteNumber(q["consumedQuota"])}
		period := str(q["refreshPeriod"])
		if period == "P1D" || period == "P1M" {
			permission.Quota.RefreshPeriod = period
		}
		break
	}
	if q := permission.Quota; q != nil && q.Provisioned != nil && q.Consumed != nil && permission.Cost != nil && *q.Provisioned-*q.Consumed < *permission.Cost {
		permission.Free = false
		return permission, failure("free_quota_exhausted")
	}
	return permission, nil
}
func finiteNumber(value any) *float64 {
	n := number(value)
	if n == nil || math.IsNaN(*n) || math.IsInf(*n, 0) || *n < 0 {
		return nil
	}
	return n
}
