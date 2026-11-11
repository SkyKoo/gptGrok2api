package adobe

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func selectionGeneration(now time.Time, available, cost *float64) GenerationEntitlements {
	return GenerationEntitlements{Status: "known", CheckedAt: now.UTC().Format(time.RFC3339Nano),
		Models: []ModelEntitlement{{Model: "gemini-3.1-flash-image", Authorized: true, PoolID: "images", Cost: cost}, {Model: "veo-3.1-lite", Authorized: true, PoolID: "videos", Cost: cost}},
		Pools:  []GenerationPool{{ID: "images", Available: available}, {ID: "videos", Available: func() *float64 { v := 10.0; return &v }()}},
	}
}

func TestModelAvailablePreservesUnknownAndSeparatePools(t *testing.T) {
	now := time.Now()
	zero, one, two := 0.0, 1.0, 2.0
	for _, tc := range []struct {
		name            string
		available, cost *float64
		change          func(*GenerationEntitlements)
		want            bool
	}{
		{name: "empty target pool", available: &zero, cost: &one, want: false},
		{name: "insufficient target pool", available: &one, cost: &two, want: false},
		{name: "available", available: &one, cost: &one, want: true},
		{name: "zero cost", available: &zero, cost: &zero, want: true},
		{name: "missing balance", cost: &one, want: true},
		{name: "missing cost and positive balance", available: &one, want: true},
		{name: "missing cost and empty pool", available: &zero, want: false},
		{name: "unauthorized", available: &one, cost: &one, change: func(e *GenerationEntitlements) { e.Models[0].Authorized = false }, want: false},
		{name: "unknown", available: &zero, cost: &one, change: func(e *GenerationEntitlements) { e.Status = "unknown" }, want: true},
		{name: "error", available: &zero, cost: &one, change: func(e *GenerationEntitlements) { e.Status = "error" }, want: true},
		{name: "stale", available: &zero, cost: &one, change: func(e *GenerationEntitlements) { e.Status = "stale" }, want: true},
		{name: "expired", available: &zero, cost: &one, change: func(e *GenerationEntitlements) { e.CheckedAt = now.Add(-6 * time.Minute).Format(time.RFC3339Nano) }, want: true},
		{name: "missing time", available: &zero, cost: &one, change: func(e *GenerationEntitlements) { e.CheckedAt = "" }, want: true},
		{name: "future time", available: &zero, cost: &one, change: func(e *GenerationEntitlements) { e.CheckedAt = now.Add(time.Hour).Format(time.RFC3339Nano) }, want: true},
		{name: "missing model", available: &zero, cost: &one, change: func(e *GenerationEntitlements) { e.Models = e.Models[1:] }, want: true},
		{name: "missing pool", available: &zero, cost: &one, change: func(e *GenerationEntitlements) { e.Pools = e.Pools[1:] }, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := selectionGeneration(now, tc.available, tc.cost)
			if tc.change != nil {
				tc.change(&e)
			}
			if got := e.ModelAvailable("gemini-3.1-flash-image", now); got != tc.want {
				t.Fatalf("available=%v want=%v", got, tc.want)
			}
			if !e.ModelAvailable("veo-3.1-lite", now) {
				t.Fatal("image allowance affected independent video pool")
			}
		})
	}
}

func selectionStore(t *testing.T) (*Store, Account, Account) {
	t.Helper()
	s, first := readyStore(t)
	second := fixtureAccount(t)
	second.AccountID = "second@AdobeID"
	claims, _ := json.Marshal(map[string]any{"user_id": second.AccountID, "exp": time.Now().Add(time.Hour).Unix()})
	second.Token = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	rows, err := s.Import([]Account{second})
	if err != nil {
		t.Fatal(err)
	}
	second = rows[0]
	if err = s.update(second.ID, second.Revision, func(a *Account) { a.LastUsedAt = time.Now().Unix() }); err != nil {
		t.Fatal(err)
	}
	return s, first, second
}

func TestReserveForModelSkipsKnownUnavailableBeforeBinding(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty image pool", true: "image not entitled"}[denied], func(t *testing.T) {
			s, first, second := selectionStore(t)
			zero, one := 0.0, 1.0
			generation := selectionGeneration(time.Now(), &zero, &one)
			if denied {
				generation.Models[0].Authorized = false
			}
			if err := s.UpdateGenerationPermissions(first.ID, first.Revision, generation); err != nil {
				t.Fatal(err)
			}
			got, err := s.ReserveForModel("image-task", "gemini-3.1-flash-image")
			if err != nil || got.ID != second.ID {
				t.Fatalf("selected=%s want=%s err=%v", got.ID, second.ID, err)
			}
			video, err := s.ReserveForModel("video-task", "veo-3.1-lite")
			if err != nil || video.ID != first.ID {
				t.Fatalf("video selected=%s want=%s err=%v", video.ID, first.ID, err)
			}
			if err = s.Release("image-task"); err != nil {
				t.Fatal(err)
			}
			restarted := NewStore(s.path, s.secret)
			if _, err = restarted.ReserveForModel("image-task", "veo-3.1-lite"); err == nil {
				t.Fatal("completed binding replayed with another model")
			}
		})
	}
}

func TestReserveForModelUnknownRemainsCandidateAndLegacyReserveIsCompatible(t *testing.T) {
	for _, mode := range []string{"stale model", "legacy reserve", "explicit account"} {
		t.Run(mode, func(t *testing.T) {
			s, first, _ := selectionStore(t)
			zero, one := 0.0, 1.0
			generation := selectionGeneration(time.Now(), &zero, &one)
			if mode == "stale model" {
				generation.Status = "stale"
			}
			if err := s.UpdateGenerationPermissions(first.ID, first.Revision, generation); err != nil {
				t.Fatal(err)
			}
			var got Account
			var err error
			switch mode {
			case "stale model":
				got, err = s.ReserveForModel("task", "gemini-3.1-flash-image")
			case "legacy reserve":
				got, err = s.Reserve("task")
			default:
				got, err = s.ReserveAccount("task", first.ID)
			}
			if err != nil || got.ID != first.ID {
				t.Fatalf("selected=%s want=%s err=%v", got.ID, first.ID, err)
			}
		})
	}
}

func TestReserveForModelAllUnavailableDoesNotBindAndReleaseStalesObservation(t *testing.T) {
	s, a := readyStore(t)
	zero, one := 0.0, 1.0
	generation := selectionGeneration(time.Now(), &zero, &one)
	if err := s.UpdateGenerationPermissions(a.ID, a.Revision, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveForModel("task", "gemini-3.1-flash-image"); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("empty pool selected: %v", err)
	}
	generation.Pools[0].Available = &one
	if err := s.UpdateGenerationPermissions(a.ID, a.Revision, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveForModel("task", "gemini-3.1-flash-image"); err != nil {
		t.Fatalf("unselected task was bound: %v", err)
	}
	if err := s.Release("task"); err != nil {
		t.Fatal(err)
	}
	a, err := s.Get(a.ID)
	if err != nil || a.Quota.Generation.Status != "stale" {
		t.Fatal("completed generation retained an authoritative old balance")
	}
}
