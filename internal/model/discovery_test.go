package model

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func catalogFixture(ids ...string) map[string]any {
	rows := []any{}
	for _, id := range ids {
		rows = append(rows, map[string]any{"slug": id, "title": "Web " + id, "reasoning_type": "none"})
	}
	return map[string]any{"models": rows, "categories": []any{map[string]any{"category": "auto_no_tools", "default_model": "auto", "supported_features": []string{"audio"}}}}
}
func TestDiscoveryDeduplicatesPersistsAndScopesAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	r := NewRegistry(path)
	if e := r.Record("a", "auto", catalogFixture("gpt-6", "research", "gpt-6-wm"), nil); e != nil {
		t.Fatal(e)
	}
	if e := r.Record("b", "auto", catalogFixture("gpt-6-wm", "research", "gpt-6"), nil); e != nil {
		t.Fatal(e)
	}
	if e := r.Record("c", "auto", catalogFixture("gpt-6-mini"), nil); e != nil {
		t.Fatal(e)
	}
	if len(r.state.Catalogs) != 2 {
		t.Fatal("equivalent catalogs not shared")
	}
	reloaded := NewRegistry(path)
	if !reloaded.Supports("a", "gpt-6") || reloaded.Supports("c", "gpt-6") || reloaded.Supports("a", "research") || reloaded.Supports("a", "gpt-6-wm") {
		t.Fatal("account/adapter capability filter failed")
	}
	if !reloaded.Supports("a", "auto") || !reloaded.Supports("c", "gpt-5-3") {
		t.Fatal("compatibility route lost")
	}
	before := reloaded.state.Accounts["a"].Hash
	if e := reloaded.Record("a", "auto", nil, errors.New("private upstream body")); e == nil {
		t.Fatal("expected error")
	}
	if !reloaded.Supports("a", "gpt-6") || reloaded.state.Accounts["a"].Hash != before {
		t.Fatal("transient failure removed valid catalog")
	}
	if e := reloaded.Record("a", "auto", catalogFixture(), nil); e == nil {
		t.Fatal("empty catalog accepted")
	}
	public, _ := json.Marshal(reloaded.Summary([]string{"a", "b", "c"}))
	if string(public) == "" {
		t.Fatal("empty summary")
	}
}
func TestCorruptDiscoveryCacheNeverOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	bad := []byte("{broken")
	_ = os.WriteFile(path, bad, 0600)
	r := NewRegistry(path)
	if e := r.Record("a", "auto", catalogFixture("gpt-6"), nil); e == nil {
		t.Fatal("corruption silently overwritten")
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(bad) {
		t.Fatal("lost original cache")
	}
}
func TestConcurrentDiscoveryAndRejection(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "models.json"))
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "c"} {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := r.Record(id, "auto", catalogFixture("gpt-6"), nil); e != nil {
				t.Error(e)
			}
			_ = r.RecordOutcome(id, "gpt-6", true, false)
			_ = r.Specs([]string{"a", "b", "c"})
		}()
	}
	wg.Wait()
	if e := r.RecordOutcome("a", "gpt-6", false, true); e != nil {
		t.Fatal(e)
	}
	if r.Supports("a", "gpt-6") || !r.Supports("b", "gpt-6") {
		t.Fatal("rejection affected other accounts")
	}
}
