package adobe

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestMediaDiagnosticDropsUntrustedFieldsAndValues(t *testing.T) {
	raw := json.RawMessage(`{"state":"pending","readiness_reason":"vendors_pending","value_kind":"null","vendors_started":3,"vendors_complete":2,"url":"https://private.invalid/?token=PRIVATE","email":"PRIVATE@example.test","token":"PRIVATE","proof":{"instance":"PRIVATE"},"vendor_response":"PRIVATE"}`)
	diagnostic := SanitizeMediaPreparationDiagnostic(raw)
	if diagnostic == nil || diagnostic.State != "pending" || diagnostic.ReadinessReason != "vendors_pending" || diagnostic.ValueKind != "null" || diagnostic.VendorsStarted == nil || *diagnostic.VendorsStarted != 3 || diagnostic.VendorsComplete == nil || *diagnostic.VendorsComplete != 2 {
		t.Fatal("valid fixed diagnostic fields were dropped")
	}
	safe, err := json.Marshal(diagnostic)
	if err != nil || strings.Contains(string(safe), "PRIVATE") || strings.Contains(string(safe), "https:") || strings.Contains(string(safe), "email") || strings.Contains(string(safe), "proof") || strings.Contains(string(safe), "token") {
		t.Fatal("private worker detail escaped projection")
	}
	for _, value := range []any{
		json.RawMessage(`{"state":"https://private.invalid","readiness_reason":"PRIVATE@example.test","value_kind":"PRIVATE_TOKEN","vendors_started":-1,"vendors_complete":9000}`),
		json.RawMessage(`"PRIVATE"`), json.RawMessage(`{"state":`),
		&MediaPreparationDiagnostic{State: "pending\nPRIVATE", ReadinessReason: "vendors_pending:PRIVATE", ValueKind: "private"},
	} {
		if SanitizeMediaPreparationDiagnostic(value) != nil {
			t.Fatal("untrusted enum, malformed object or unbounded count was accepted")
		}
	}
}

func TestMediaDiagnosticCountBoundsAndConsistency(t *testing.T) {
	for _, value := range []any{0, 3, float64(0), float64(3)} {
		diagnostic := SanitizeMediaPreparationDiagnostic(map[string]any{"vendors_started": value, "vendors_complete": value})
		if diagnostic == nil || diagnostic.VendorsStarted == nil || diagnostic.VendorsComplete == nil {
			t.Fatal("valid boundary count lost")
		}
	}
	for _, value := range []any{-1, 4, float64(1.5), math.NaN(), math.Inf(1), "3", true, nil} {
		if SanitizeMediaPreparationDiagnostic(map[string]any{"vendors_started": value, "vendors_complete": value}) != nil {
			t.Fatal("invalid count accepted")
		}
	}
	if SanitizeMediaPreparationDiagnostic(map[string]any{"vendors_started": 1, "vendors_complete": 2}) != nil {
		t.Fatal("completed count exceeded started count")
	}
}

func TestMediaDiagnosticErrorSuffixPreservesLegacyMessages(t *testing.T) {
	for _, diagnostic := range []*MediaPreparationDiagnostic{nil, {}, {State: "PRIVATE"}} {
		err := &Error{Stage: "relogin", Code: "media_session_incomplete", MediaDiagnostic: diagnostic}
		if SafeError(err) != "adobe_relogin_media_session_incomplete" {
			t.Fatal("absent diagnostic changed legacy error")
		}
	}
	for _, reason := range []string{"editor_not_ready", "observer_unavailable", "vendors_pending", "instance_ambiguous", "context_changed", "identity_pending", "identity_changed", "runtime_changed", "verification_timeout",
		"authentication_missing", "arp_observer_missing", "editor_route_changed", "page_hidden", "observation_cancelled", "evaluation_failed", "navigation_budget_exhausted"} {
		err := &Error{Stage: "relogin", Code: "media_session_incomplete", MediaDiagnostic: &MediaPreparationDiagnostic{State: "pending", ReadinessReason: reason}}
		if SafeError(err) != "adobe_relogin_media_session_incomplete ["+reason+"]" {
			t.Fatal("fixed reason was not retained")
		}
	}
	err := &Error{Stage: "relogin", Code: "media_session_incomplete", Status: 502, MediaDiagnostic: &MediaPreparationDiagnostic{State: "changed"}}
	if SafeError(err) != "adobe_relogin_media_session_incomplete (HTTP 502) [changed]" {
		t.Fatal("safe fallback state or status was lost")
	}
	err.Code = "result_unknown"
	if SafeError(err) != "adobe_relogin_result_unknown (HTTP 502)" {
		t.Fatal("unrelated error formatting changed")
	}
}

func TestMediaFailurePersistsOnlySanitizedDiagnostic(t *testing.T) {
	s, a := idleMediaStore(t)
	badCount := 9000
	s.SetBrowserRefresher(func(context.Context, Account) (Account, error) {
		return Account{}, &Error{Stage: "relogin", Code: "media_session_incomplete", MediaDiagnostic: &MediaPreparationDiagnostic{State: "pending", ReadinessReason: "instance_ambiguous", ValueKind: "PRIVATE_TOKEN", VendorsStarted: &badCount}}
	})
	before := time.Now().Unix()
	if _, err := s.WarmMediaSession(mediaTestContext(), a.ID, a.Revision); SafeError(err) != "adobe_relogin_media_session_incomplete [instance_ambiguous]" {
		t.Fatalf("safe reason lost: %v", err)
	}
	restarted := NewStore(s.path, s.secret)
	disk, err := restarted.load()
	if err != nil {
		t.Fatal(err)
	}
	failure := disk.MediaFailures[a.ID]
	if failure.Revision != a.Revision || failure.ObservedAt < before || failure.RetryAfter <= failure.ObservedAt || failure.Diagnostic == nil || failure.Diagnostic.ReadinessReason != "instance_ambiguous" || failure.Diagnostic.ValueKind != "" || failure.Diagnostic.VendorsStarted != nil {
		t.Fatal("bounded diagnostic did not survive encrypted persistence")
	}
	raw, _ := json.Marshal(failure)
	if strings.Contains(string(raw), "PRIVATE_TOKEN") || strings.Contains(string(raw), a.Token) || strings.Contains(string(raw), a.Email) || strings.Contains(string(raw), a.Cookie) {
		t.Fatal("credentials persisted in failure diagnostic")
	}
}
