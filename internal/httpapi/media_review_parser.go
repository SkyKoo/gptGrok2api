package httpapi

import (
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

const maxGrokReviewResponseBytes = 16 << 10

// A complete, explicit verdict determines relevance. Formatting and the model's
// self-reported attachment counts are diagnostics, not content rejection rules.
func parseMediaVerification(raw string, frames, references int) videoVerification {
	result := videoVerification{Verdict: "uncertain", Reason: "invalid_review_response", Frames: frames, References: references}
	invalid := func(cause string) videoVerification { result.parseError = cause; return result }
	if len(raw) > maxGrokReviewResponseBytes {
		return invalid("response_too_long")
	}
	if frames < 1 || references < 0 {
		return invalid("missing_review_input")
	}
	objects, outside, cause := reviewJSONDocuments(raw)
	if cause != "" {
		return invalid(cause)
	}
	var chosen videoVerification
	for i, object := range objects {
		checked := parseReviewDocument(object, frames, references, 0)
		if checked.parseError != "" {
			return invalid(checked.parseError)
		}
		if i == 0 {
			chosen = checked
			continue
		}
		// A duplicated response can be harmless; never select one of conflicting
		// conclusions or explanations, including a quoted example and a final reply.
		if chosen.Verdict != checked.Verdict || chosen.Reason != checked.Reason || chosen.expected != checked.expected || chosen.observed != checked.observed || chosen.basis != checked.basis {
			return invalid("conflicting_review_objects")
		}
		chosen.parseNotes = append(chosen.parseNotes, checked.parseNotes...)
		chosen.parseNotes = append(chosen.parseNotes, "repeated_identical_review")
	}
	for _, match := range reviewProseVerdict.FindAllStringSubmatch(outside, -1) {
		if strings.ToLower(match[1]) != chosen.Verdict {
			return invalid("conflicting_prose_verdict")
		}
	}
	if strings.TrimSpace(outside) != "" {
		chosen.parseNotes = append(chosen.parseNotes, "json_envelope_removed")
	}
	chosen.parseNotes = uniqueReviewNotes(chosen.parseNotes)
	return chosen
}

var reviewProseVerdict = regexp.MustCompile(`(?i)\bverdict\b["'\s]*[:=]["'\s]*(matched|mismatch|uncertain)\b`)

// Scan balanced documents, respecting quoted braces and escapes. A truncated
// document fails as a whole: never salvage a nested object from broken JSON.
func reviewJSONDocuments(raw string) ([]string, string, string) {
	raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "\ufeff"))
	documents := []string{}
	var outside strings.Builder
	for i := 0; i < len(raw); {
		if raw[i] != '{' && !reviewJSONArrayStart(raw, i) {
			outside.WriteByte(raw[i])
			i++
			continue
		}
		start := i
		stack := []byte{}
		quoted, escaped := false, false
		for ; i < len(raw); i++ {
			c := raw[i]
			if quoted {
				if escaped {
					escaped = false
				} else if c == '\\' {
					escaped = true
				} else if c == '"' {
					quoted = false
				}
				continue
			}
			if c == '"' {
				quoted = true
				continue
			}
			if c == '{' || c == '[' {
				stack = append(stack, c)
			}
			if c == '}' || c == ']' {
				if len(stack) == 0 || c == '}' && stack[len(stack)-1] != '{' || c == ']' && stack[len(stack)-1] != '[' {
					return nil, "", "invalid_json_object"
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					i++
					documents = append(documents, raw[start:i])
					break
				}
			}
		}
		if len(stack) > 0 || quoted {
			return nil, "", "incomplete_json_object"
		}
	}
	if len(documents) == 0 {
		return nil, "", "invalid_json_object"
	}
	return documents, outside.String(), ""
}

// Treat an array of JSON objects as a complete envelope, but not prose labels
// such as [JSON]. Its closing bracket must be present before using its verdict.
func reviewJSONArrayStart(raw string, at int) bool {
	if raw[at] != '[' {
		return false
	}
	next := strings.TrimLeft(raw[at+1:], " \t\r\n")
	return len(next) > 0 && (next[0] == '{' || next[0] == '[')
}

func parseReviewDocument(raw string, frames, references, depth int) videoVerification {
	result := videoVerification{Verdict: "uncertain", Reason: "invalid_review_response", Frames: frames, References: references}
	invalid := func(cause string) videoVerification { result.parseError = cause; return result }
	if depth > 3 {
		return invalid("review_wrapper_too_deep")
	}
	cleaned, changed := stripReviewTrailingCommas(raw)
	if changed {
		result.parseNotes = append(result.parseNotes, "trailing_commas_removed")
	}
	if strings.HasPrefix(strings.TrimSpace(cleaned), "[") {
		var items []json.RawMessage
		if json.Unmarshal([]byte(cleaned), &items) != nil {
			return invalid("invalid_json_object")
		}
		if len(items) != 1 {
			return invalid("ambiguous_review_array")
		}
		checked := parseReviewDocument(string(items[0]), frames, references, depth+1)
		checked.parseNotes = append(checked.parseNotes, result.parseNotes...)
		checked.parseNotes = append(checked.parseNotes, "single_review_wrapper_removed")
		return checked
	}
	fields, notes, cause := decodeReviewFields(cleaned)
	result.parseNotes = append(result.parseNotes, notes...)
	if cause != "" {
		return invalid(cause)
	}
	if _, ok := fields["verdict"]; !ok {
		var nested json.RawMessage
		for _, key := range []string{"result", "review", "data"} {
			if value, ok := fields[key]; ok {
				if nested != nil {
					return invalid("ambiguous_review_wrapper")
				}
				nested = value
			}
		}
		if nested == nil {
			return invalid("missing_verdict")
		}
		checked := parseReviewDocument(string(nested), frames, references, depth+1)
		checked.parseNotes = append(checked.parseNotes, result.parseNotes...)
		checked.parseNotes = append(checked.parseNotes, "single_review_wrapper_removed")
		return checked
	}
	verdict, ok := reviewString(fields["verdict"])
	if !ok {
		return invalid("invalid_verdict_type")
	}
	normalized := strings.ToLower(strings.TrimSpace(verdict))
	if normalized != verdict {
		result.parseNotes = append(result.parseNotes, "normalized_verdict")
	}
	switch normalized {
	case "matched", "mismatch", "uncertain":
	default:
		return invalid("invalid_verdict")
	}
	for _, key := range []string{"result", "review", "data"} {
		if value, ok := fields[key]; ok {
			// Nondecision metadata is harmless. A nested explicit decision must
			// agree, even if it is invalid or wrapped several levels deep.
			if hasReviewDecision(value) {
				nested := parseReviewDocument(string(value), frames, references, depth+1)
				if nested.parseError != "" {
					return invalid(nested.parseError)
				}
				if nested.Verdict != normalized {
					return invalid("conflicting_review_objects")
				}
			}
			result.parseNotes = append(result.parseNotes, "additional_fields_ignored")
		}
	}
	reason, _ := reviewString(fields["reason"])
	canonicalReason := normalizedReviewReason(reason)
	switch canonicalReason {
	case "consistent":
		if normalized != "matched" {
			return invalid("inconsistent_verdict")
		}
	case "unrelated_subject", "unrelated_scene", "placeholder", "unrelated_content":
		if normalized != "mismatch" {
			return invalid("inconsistent_verdict")
		}
	case "insufficient_evidence":
		if normalized != "uncertain" {
			return invalid("inconsistent_verdict")
		}
	default:
		// A detail-only or otherwise unsupported rejection is not a formatting issue.
		if normalized == "mismatch" && strings.TrimSpace(reason) != "" {
			return invalid("invalid_reason")
		}
		canonicalReason = map[string]string{"matched": "consistent", "mismatch": "unrelated_content", "uncertain": "insufficient_evidence"}[normalized]
		result.parseNotes = append(result.parseNotes, "reason_normalized_from_verdict")
	}
	if canonicalReason != reason {
		result.parseNotes = append(result.parseNotes, "normalized_reason")
	}
	result.Verdict, result.Reason = normalized, canonicalReason
	for _, field := range []struct {
		key   string
		dest  *string
		limit int
	}{{"expected", &result.expected, 400}, {"observed", &result.observed, 400}, {"basis", &result.basis, 600}} {
		value, ok := reviewString(fields[field.key])
		if !ok && fields[field.key] != nil {
			result.parseNotes = append(result.parseNotes, "non_text_"+field.key+"_ignored")
		}
		*field.dest = truncateRunes(strings.TrimSpace(value), field.limit)
	}
	for _, field := range []struct {
		key    string
		actual int
	}{{"frames_reviewed", frames}, {"references_reviewed", references}} {
		// These numbers cannot establish what the model looked at. The attachment
		// manifest is authoritative; retain discrepancies for troubleshooting only.
		reported, ok := reviewCount(fields[field.key])
		if _, quoted := reviewString(fields[field.key]); quoted {
			result.parseNotes = append(result.parseNotes, "string_count_normalized")
		}
		if !ok || reported != float64(field.actual) {
			result.parseNotes = append(result.parseNotes, field.key+"_uses_actual_count")
		}
	}
	result.parseNotes = uniqueReviewNotes(result.parseNotes)
	return result
}

// Only the documented wrapper names can contain a second decision; arbitrary
// extra metadata is not interpreted as another review.
func hasReviewDecision(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var contains func(any) bool
	contains = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			for key, child := range x {
				switch canonicalReviewKey(key) {
				case "verdict":
					return true
				case "result", "review", "data":
					if contains(child) {
						return true
					}
				}
			}
		case []any:
			for _, child := range x {
				if contains(child) {
					return true
				}
			}
		}
		return false
	}
	return contains(value)
}

func decodeReviewFields(raw string) (map[string]json.RawMessage, []string, string) {
	d := json.NewDecoder(strings.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, nil, "invalid_json_object"
	}
	fields := map[string]json.RawMessage{}
	notes := []string{}
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, nil, "invalid_json_object"
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, nil, "invalid_json_object"
		}
		canonical := canonicalReviewKey(key)
		if canonical == "" {
			notes = append(notes, "additional_fields_ignored")
			continue
		}
		if canonical != key {
			notes = append(notes, "normalized_field_names")
		}
		if previous, exists := fields[canonical]; exists {
			if canonical == "frames_reviewed" || canonical == "references_reviewed" {
				notes = append(notes, "duplicate_count_ignored")
				continue
			}
			if !sameReviewField(canonical, previous, value) {
				return nil, nil, "conflicting_duplicate_field"
			}
			notes = append(notes, "identical_duplicate_field_ignored")
			continue
		}
		fields[canonical] = value
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return nil, nil, "invalid_json_object"
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, nil, "trailing_content"
	}
	return fields, notes, ""
}

func canonicalReviewKey(key string) string {
	normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(key)))
	switch normalized {
	case "verdict", "reason", "expected", "observed", "basis", "result", "review", "data":
		return normalized
	case "framesreviewed":
		return "frames_reviewed"
	case "referencesreviewed":
		return "references_reviewed"
	default:
		return ""
	}
}
func reviewString(value json.RawMessage) (string, bool) {
	if len(value) == 0 || string(value) == "null" {
		return "", false
	}
	var text string
	err := json.Unmarshal(value, &text)
	return text, err == nil
}
func reviewCount(value json.RawMessage) (float64, bool) {
	text := strings.TrimSpace(string(value))
	if decoded, ok := reviewString(value); ok {
		text = strings.TrimSpace(decoded)
	}
	number, err := strconv.ParseFloat(text, 64)
	return number, err == nil
}
func sameReviewField(key string, a, b json.RawMessage) bool {
	av, aok := reviewString(a)
	bv, bok := reviewString(b)
	if aok && bok {
		av, bv = strings.TrimSpace(av), strings.TrimSpace(bv)
		if key == "verdict" {
			av, bv = strings.ToLower(av), strings.ToLower(bv)
		} else if key == "reason" {
			av, bv = normalizedReviewReason(av), normalizedReviewReason(bv)
		} else {
			av, bv = strings.Join(strings.Fields(av), " "), strings.Join(strings.Fields(bv), " ")
		}
		return av == bv
	}
	var avalue, bvalue any
	return json.Unmarshal(a, &avalue) == nil && json.Unmarshal(b, &bvalue) == nil && reflect.DeepEqual(avalue, bvalue)
}
func uniqueReviewNotes(notes []string) []string {
	unique := []string{}
	seen := map[string]bool{}
	for _, note := range notes {
		if !seen[note] {
			unique = append(unique, note)
			seen[note] = true
		}
	}
	return unique
}

// Remove commas only immediately before a closing delimiter and outside
// strings. Never complete missing delimiters, change values or guess a verdict.
func stripReviewTrailingCommas(raw string) (string, bool) {
	var out strings.Builder
	quoted, escaped, changed := false, false, false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if quoted {
			out.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
		}
		if c == ',' {
			j := i + 1
			for j < len(raw) && (raw[j] == ' ' || raw[j] == '\n' || raw[j] == '\r' || raw[j] == '\t') {
				j++
			}
			if j < len(raw) && (raw[j] == '}' || raw[j] == ']') {
				changed = true
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.String(), changed
}

func normalizedReviewReason(reason string) string {
	return strings.NewReplacer(" ", "_", "-", "_").Replace(strings.ToLower(strings.TrimSpace(reason)))
}
