package adobe

import (
	"encoding/json"
	"errors"
	"math"
)

// MediaPreparationDiagnostic contains only bounded runtime observations. It
// never carries a URL, SDK instance/proof, vendor payload, token or cookie scope.
type MediaPreparationDiagnostic struct {
	State           string                 `json:"state,omitempty"`
	ReadinessReason string                 `json:"readiness_reason,omitempty"`
	ValueKind       string                 `json:"value_kind,omitempty"`
	VendorsStarted  *int                   `json:"vendors_started,omitempty"`
	VendorsComplete *int                   `json:"vendors_complete,omitempty"`
	Page            *MediaPageDiagnostic   `json:"media_page,omitempty"`
	Timing          *MediaTimingDiagnostic `json:"media_timing,omitempty"`
}

// MediaPageDiagnostic is a bounded description of the Firefly page shape. It
// deliberately contains only counts and fixed categories; no URL, DOM text,
// SDK object, token or cookie is ever retained.
type MediaPageDiagnostic struct {
	Route                string   `json:"route,omitempty"`
	MatchingHostCount    *int     `json:"matching_host_count,omitempty"`
	AuthenticationCount  *int     `json:"authentication_count,omitempty"`
	ARPObserverCount     *int     `json:"arp_observer_count,omitempty"`
	ScanComplete         *bool    `json:"scan_complete,omitempty"`
	PerformanceAvailable *bool    `json:"performance_available,omitempty"`
	MarkCount            *int     `json:"mark_count,omitempty"`
	MarkCategories       []string `json:"mark_categories,omitempty"`
}

// MediaTimingDiagnostic records only bounded phase names and elapsed times.
// The values are observations for diagnosis, not a session credential.
type MediaTimingDiagnostic struct {
	CandidateChanges      *int   `json:"candidate_changes,omitempty"`
	SourceChanges         *int   `json:"source_changes,omitempty"`
	AuthenticationChanges *int   `json:"authentication_changes,omitempty"`
	Emissions             *int   `json:"emissions,omitempty"`
	ValueChanges          *int   `json:"value_changes,omitempty"`
	ProofChanges          *int   `json:"proof_changes,omitempty"`
	FirstSourceMS         *int64 `json:"first_source_ms,omitempty"`
	FirstNonemptyMS       *int64 `json:"first_nonempty_ms,omitempty"`
	VendorsReadyMS        *int64 `json:"vendors_ready_ms,omitempty"`
	LastCandidateMS       *int64 `json:"last_candidate_ms,omitempty"`
	Phase                 string `json:"phase,omitempty"`
	PhaseElapsedMS        *int64 `json:"phase_elapsed_ms,omitempty"`
	NavigationElapsedMS   *int64 `json:"navigation_elapsed_ms,omitempty"`
	ObservationElapsedMS  *int64 `json:"observation_elapsed_ms,omitempty"`
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
		if input.Page != nil {
			fields["media_page"] = input.Page
		}
		if input.Timing != nil {
			fields["media_timing"] = input.Timing
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
		case "performance_unavailable", "instance_missing", "instance_ambiguous", "vendor_unrecognized", "vendors_pending", "token_pending", "token_settling", "editor_not_ready", "observer_unavailable", "context_changed", "identity_pending", "identity_changed", "runtime_changed", "verification_timeout",
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
	count := func(value any, maximum int) *int {
		number := -1.0
		switch v := value.(type) {
		case float64:
			number = v
		case int:
			number = float64(v)
		}
		if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > float64(maximum) || math.Trunc(number) != number {
			return nil
		}
		integer := int(number)
		return &integer
	}
	diagnostic.VendorsStarted = count(fields["vendors_started"], 3)
	diagnostic.VendorsComplete = count(fields["vendors_complete"], 3)
	if diagnostic.VendorsStarted != nil && diagnostic.VendorsComplete != nil && *diagnostic.VendorsComplete > *diagnostic.VendorsStarted {
		diagnostic.VendorsStarted, diagnostic.VendorsComplete = nil, nil
	}
	if page := sanitizeMediaPage(fields["media_page"], count); page != nil {
		diagnostic.Page = page
	}
	if timing := sanitizeMediaTiming(fields["media_timing"], count); timing != nil {
		diagnostic.Timing = timing
	}
	if diagnostic.State == "" && diagnostic.ReadinessReason == "" && diagnostic.ValueKind == "" && diagnostic.VendorsStarted == nil && diagnostic.VendorsComplete == nil && diagnostic.Page == nil && diagnostic.Timing == nil {
		return nil
	}
	return diagnostic
}

func sanitizeMediaPage(value any, count func(any, int) *int) *MediaPageDiagnostic {
	fields, ok := diagnosticFields(value)
	if !ok {
		return nil
	}
	page := &MediaPageDiagnostic{}
	if route, ok := fields["route"].(string); ok {
		switch route {
		case "unavailable", "editor", "other":
			page.Route = route
		}
	}
	page.MatchingHostCount = count(fields["matching_host_count"], 32)
	page.AuthenticationCount = count(fields["authentication_count"], 32)
	page.ARPObserverCount = count(fields["arp_observer_count"], 32)
	if page.AuthenticationCount != nil && page.MatchingHostCount != nil && *page.AuthenticationCount > *page.MatchingHostCount {
		page.AuthenticationCount = nil
	}
	if page.ARPObserverCount != nil && page.AuthenticationCount != nil && *page.ARPObserverCount > *page.AuthenticationCount {
		page.ARPObserverCount = nil
	}
	if scan, ok := fields["scan_complete"].(bool); ok {
		page.ScanComplete = &scan
	}
	if available, ok := fields["performance_available"].(bool); ok {
		page.PerformanceAvailable = &available
	}
	page.MarkCount = count(fields["mark_count"], 1024)
	if categories, ok := fields["mark_categories"].([]any); ok {
		for _, item := range categories {
			if category, ok := item.(string); ok && mediaMarkCategory(category) {
				page.MarkCategories = appendUniqueBounded(page.MarkCategories, category, 16)
			}
		}
	} else if categories, ok := fields["mark_categories"].([]string); ok {
		for _, category := range categories {
			if mediaMarkCategory(category) {
				page.MarkCategories = appendUniqueBounded(page.MarkCategories, category, 16)
			}
		}
	}
	if page.Route == "" && page.MatchingHostCount == nil && page.AuthenticationCount == nil && page.ARPObserverCount == nil && page.ScanComplete == nil && page.PerformanceAvailable == nil && page.MarkCount == nil && len(page.MarkCategories) == 0 {
		return nil
	}
	return page
}

func sanitizeMediaTiming(value any, count func(any, int) *int) *MediaTimingDiagnostic {
	fields, ok := diagnosticFields(value)
	if !ok {
		return nil
	}
	timing := &MediaTimingDiagnostic{}
	timing.CandidateChanges = count(fields["candidate_changes"], 65535)
	timing.SourceChanges = count(fields["source_changes"], 65535)
	timing.AuthenticationChanges = count(fields["authentication_changes"], 65535)
	timing.Emissions = count(fields["emissions"], 65535)
	timing.ValueChanges = count(fields["value_changes"], 65535)
	timing.ProofChanges = count(fields["proof_changes"], 65535)
	timing.FirstSourceMS = duration(fields["first_source_ms"], 60000)
	timing.FirstNonemptyMS = duration(fields["first_nonempty_ms"], 60000)
	timing.VendorsReadyMS = duration(fields["vendors_ready_ms"], 60000)
	timing.LastCandidateMS = duration(fields["last_candidate_ms"], 60000)
	if phase, ok := fields["phase"].(string); ok {
		switch phase {
		case "navigation", "observation", "verification", "cleanup":
			timing.Phase = phase
		}
	}
	timing.PhaseElapsedMS = duration(fields["phase_elapsed_ms"], 60000)
	timing.NavigationElapsedMS = duration(fields["navigation_elapsed_ms"], 60000)
	timing.ObservationElapsedMS = duration(fields["observation_elapsed_ms"], 60000)
	if timing.CandidateChanges == nil && timing.SourceChanges == nil && timing.AuthenticationChanges == nil && timing.Emissions == nil && timing.ValueChanges == nil && timing.ProofChanges == nil && timing.FirstSourceMS == nil && timing.FirstNonemptyMS == nil && timing.VendorsReadyMS == nil && timing.LastCandidateMS == nil && timing.Phase == "" && timing.PhaseElapsedMS == nil && timing.NavigationElapsedMS == nil && timing.ObservationElapsedMS == nil {
		return nil
	}
	return timing
}

func diagnosticFields(value any) (map[string]any, bool) {
	switch input := value.(type) {
	case map[string]any:
		return input, true
	case *MediaPageDiagnostic:
		if input == nil {
			return nil, false
		}
		return mediaPageFields(input), true
	case *MediaTimingDiagnostic:
		if input == nil {
			return nil, false
		}
		return mediaTimingFields(input), true
	default:
		return nil, false
	}
}

func mediaPageFields(page *MediaPageDiagnostic) map[string]any {
	fields := map[string]any{"route": page.Route}
	if page.MatchingHostCount != nil {
		fields["matching_host_count"] = *page.MatchingHostCount
	}
	if page.AuthenticationCount != nil {
		fields["authentication_count"] = *page.AuthenticationCount
	}
	if page.ARPObserverCount != nil {
		fields["arp_observer_count"] = *page.ARPObserverCount
	}
	if page.ScanComplete != nil {
		fields["scan_complete"] = *page.ScanComplete
	}
	if page.PerformanceAvailable != nil {
		fields["performance_available"] = *page.PerformanceAvailable
	}
	if page.MarkCount != nil {
		fields["mark_count"] = *page.MarkCount
	}
	if len(page.MarkCategories) > 0 {
		fields["mark_categories"] = page.MarkCategories
	}
	return fields
}

func mediaTimingFields(timing *MediaTimingDiagnostic) map[string]any {
	fields := map[string]any{"phase": timing.Phase}
	for key, value := range map[string]*int{"candidate_changes": timing.CandidateChanges, "source_changes": timing.SourceChanges, "authentication_changes": timing.AuthenticationChanges, "emissions": timing.Emissions, "value_changes": timing.ValueChanges, "proof_changes": timing.ProofChanges} {
		if value != nil {
			fields[key] = *value
		}
	}
	for key, value := range map[string]*int64{"first_source_ms": timing.FirstSourceMS, "first_nonempty_ms": timing.FirstNonemptyMS, "vendors_ready_ms": timing.VendorsReadyMS, "last_candidate_ms": timing.LastCandidateMS, "phase_elapsed_ms": timing.PhaseElapsedMS, "navigation_elapsed_ms": timing.NavigationElapsedMS, "observation_elapsed_ms": timing.ObservationElapsedMS} {
		if value != nil {
			fields[key] = *value
		}
	}
	return fields
}

func duration(value any, maximum int64) *int64 {
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case int:
		number = float64(v)
	case int64:
		number = float64(v)
	default:
		return nil
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > float64(maximum) || math.Trunc(number) != number {
		return nil
	}
	result := int64(number)
	return &result
}

func mediaMarkCategory(value string) bool {
	switch value {
	case "init_start", "vendor_start", "vendor_duration", "unknown":
		return true
	default:
		return false
	}
}

func appendUniqueBounded(values []string, value string, maximum int) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	if len(values) >= maximum {
		return values
	}
	return append(values, value)
}

func MediaDiagnosticFromError(err error) *MediaPreparationDiagnostic {
	var failure *Error
	if errors.As(err, &failure) {
		return SanitizeMediaPreparationDiagnostic(failure.MediaDiagnostic)
	}
	return nil
}
