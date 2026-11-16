package oauth

import "testing"

func TestEffectiveConsentStateDoesNotTreatMissingMetadataAsUnsubmitted(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		submitted   bool
		want        string
	}{
		{"legacy_false", "", false, "unknown"},
		{"legacy_true", "", true, "attempted"},
		{"explicit_preclick", "not_attempted", false, "not_attempted"},
		{"explicit_click", "attempted", false, "attempted"},
		{"contradictory", "not_attempted", true, "attempted"},
		{"invalid", "SECRET", false, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := BrowserDeviceResult{ConsentState: tc.state, ConsentSubmitted: tc.submitted}
			if got := r.EffectiveConsentState(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
