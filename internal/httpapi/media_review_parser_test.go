package httpapi

import (
	"strings"
	"testing"
)

func TestMediaReviewAcceptsHarmlessFormatting(t *testing.T) {
	for _, tc := range []struct {
		name, raw, note string
	}{
		{"prose and fence", "Here is the review:\n```json\n{\"verdict\":\"matched\"}\n```\nThe subject is related.", "json_envelope_removed"},
		{"prose label", `[JSON] {"verdict":"matched"}`, "json_envelope_removed"},
		{"BOM case and camel keys", "\ufeff {\" Verdict \":\" MATCHED \",\"Reason\":\" Consistent \",\"framesReviewed\":1,\"referencesReviewed\":2}", "normalized_field_names"},
		{"string counts", `{"verdict":"matched","frames_reviewed":"1","references_reviewed":"2"}`, "string_count_normalized"},
		{"wrong count", `{"verdict":"matched","frames_reviewed":6,"references_reviewed":999}`, "frames_reviewed_uses_actual_count"},
		{"missing counts", `{"verdict":"matched"}`, "references_reviewed_uses_actual_count"},
		{"null count", `{"verdict":"matched","frames_reviewed":null}`, "frames_reviewed_uses_actual_count"},
		{"invalid count shape", `{"verdict":"matched","frames_reviewed":{"count":1}}`, "frames_reviewed_uses_actual_count"},
		{"metadata", `{"verdict":"matched","confidence":0.99,"data":{"elapsed":20,"result":"ok"}}`, "additional_fields_ignored"},
		{"trailing commas", `{"verdict":"matched","extra":[1,2,],}`, "trailing_commas_removed"},
		{"same duplicate verdict", `{"verdict":"matched","VERDICT":" MATCHED "}`, "identical_duplicate_field_ignored"},
		{"same duplicate reason", `{"verdict":"matched","reason":"consistent","Reason":" Consistent "}`, "identical_duplicate_field_ignored"},
		{"duplicate count", `{"verdict":"matched","frames_reviewed":1,"framesReviewed":8}`, "duplicate_count_ignored"},
		{"identical repeated objects", `{"verdict":"matched"} {"VERDICT":" MATCHED "}`, "repeated_identical_review"},
		{"wrapped result", `{"result":{"verdict":"matched"},"meta":{"id":1}}`, "single_review_wrapper_removed"},
		{"wrapped review array", `{"review":[{"verdict":"matched"}]}`, "single_review_wrapper_removed"},
		{"array envelope", `[{"verdict":"matched"}]`, "single_review_wrapper_removed"},
		{"wrapped data", `{"data":{"review":{"verdict":"matched"}}}`, "single_review_wrapper_removed"},
		{"matching nested verdict", `{"verdict":"matched","data":{"verdict":"matched"}}`, "additional_fields_ignored"},
		{"missing reason", `{"verdict":"matched"}`, "reason_normalized_from_verdict"},
		{"prose reason", `{"verdict":"matched","reason":"The main subject is related."}`, "reason_normalized_from_verdict"},
		{"nontext auxiliary description", `{"verdict":"matched","expected":["cat"],"observed":null,"basis":{"text":"cat"}}`, "non_text_expected_ignored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMediaVerification(tc.raw, 1, 2)
			if got.Verdict != "matched" || got.Reason != "consistent" || got.parseError != "" || got.Frames != 1 || got.References != 2 {
				t.Fatalf("harmless formatting rejected: %+v", got)
			}
			if !reviewHasNote(got.parseNotes, tc.note) {
				t.Fatalf("missing normalization diagnostic %q: %v", tc.note, got.parseNotes)
			}
		})
	}
}

func reviewHasNote(notes []string, expected string) bool {
	for _, note := range notes {
		if note == expected {
			return true
		}
	}
	return false
}

func TestMediaReviewPreservesQuotedEvidence(t *testing.T) {
	raw := `{"verdict":"matched","expected":"A sign reading \",}\" beside [braces]","observed":"a cat with {curly} marks","basis":"text: ,} and ,] and \\ path",}`
	got := parseMediaVerification(raw, 1, 0)
	if got.Verdict != "matched" || got.expected != `A sign reading ",}" beside [braces]` || got.observed != "a cat with {curly} marks" || got.basis != `text: ,} and ,] and \ path` {
		t.Fatalf("evidence text changed during normalization: %+v", got)
	}
}

func TestMediaReviewRejectsAmbiguousOrIncompleteConclusions(t *testing.T) {
	for _, tc := range []struct{ name, raw, cause string }{
		{"duplicate verdict", `{"verdict":"matched","Verdict":"mismatch"}`, "conflicting_duplicate_field"},
		{"conflicting objects", `{"verdict":"matched"} {"verdict":"mismatch"}`, "conflicting_review_objects"},
		{"reverse conflicts", `{"verdict":"mismatch"} {"verdict":"matched"}`, "conflicting_review_objects"},
		{"truncated second object", `{"verdict":"matched"} {"verdict":"mismatch"`, "incomplete_json_object"},
		{"truncated outer object", `{"data":{"verdict":"matched"}`, "incomplete_json_object"},
		{"truncated array", `[{"verdict":"matched"}`, "incomplete_json_object"},
		{"conflicting prose", `verdict: mismatch; {"verdict":"matched"}`, "conflicting_prose_verdict"},
		{"conflicting trailing prose", `{"verdict":"matched"} Verdict = "uncertain"`, "conflicting_prose_verdict"},
		{"contradictory reason", `{"verdict":"matched","reason":"unrelated_scene"}`, "inconsistent_verdict"},
		{"contradictory normalized reason", `{"verdict":"matched","reason":"Unrelated Scene"}`, "inconsistent_verdict"},
		{"detail-only rejection", `{"verdict":"mismatch","reason":"action_mismatch"}`, "invalid_reason"},
		{"no verdict", `{"reason":"consistent"}`, "missing_verdict"},
		{"unknown verdict", `{"verdict":"approved"}`, "invalid_verdict"},
		{"boolean verdict", `{"verdict":true}`, "invalid_verdict_type"},
		{"null verdict", `{"verdict":null}`, "invalid_verdict_type"},
		{"refusal", "I cannot inspect these images.", "invalid_json_object"},
		{"ambiguous wrappers", `{"result":{"verdict":"matched"},"data":{"verdict":"mismatch"}}`, "ambiguous_review_wrapper"},
		{"multiple array decisions", `[{"verdict":"matched"},{"verdict":"mismatch"}]`, "ambiguous_review_array"},
		{"conflicting nested decision", `{"verdict":"matched","data":{"verdict":"mismatch"}}`, "conflicting_review_objects"},
		{"nested duplicate conflict", `{"verdict":"matched","data":{"verdict":"matched","verdict":"mismatch"}}`, "conflicting_duplicate_field"},
		{"nested invalid verdict", `{"verdict":"matched","data":{"verdict":"approve"}}`, "invalid_verdict"},
		{"deeper conflict", `{"verdict":"matched","data":{"verdict":"matched","result":{"verdict":"mismatch"}}}`, "conflicting_review_objects"},
		{"too large", strings.Repeat(" ", maxGrokReviewResponseBytes) + `{"verdict":"matched"}`, "response_too_long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMediaVerification(tc.raw, 1, 0)
			if got.Verdict != "uncertain" || got.Reason != "invalid_review_response" || got.parseError != tc.cause || got.confirmed {
				t.Fatalf("ambiguous reply used as a content decision: %+v", got)
			}
		})
	}
}

func TestMediaReviewKeepsExplicitNonMatchVerdicts(t *testing.T) {
	for _, verdict := range []string{"mismatch", "uncertain"} {
		got := parseMediaVerification("```json\n{\"verdict\":\""+verdict+"\"}\n```", 1, 0)
		if got.Verdict != verdict || got.parseError != "" || got.confirmed {
			t.Fatalf("tolerance changed the verdict or confirmed a single rejection: %+v", got)
		}
	}
	for _, raw := range []string{`{"verdict":"matched"}`, `{"verdict":"mismatch"}`} {
		if got := parseMediaVerification(raw, 0, 0); got.Verdict != "uncertain" || got.parseError != "missing_review_input" {
			t.Fatalf("accepted missing actual attachments: %+v", got)
		}
	}
}
