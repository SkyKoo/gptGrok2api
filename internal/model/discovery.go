package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const CatalogTTL = 6 * time.Hour

type WebModel struct {
	Slug          string   `json:"slug"`
	Title         string   `json:"title"`
	ReasoningType string   `json:"reasoning_type,omitempty"`
	EnabledTools  []string `json:"enabled_tools,omitempty"`
	WorkMode      bool     `json:"is_work_mode_model,omitempty"`
}
type WebCategory struct {
	ID       string   `json:"category"`
	Default  string   `json:"default_model"`
	Name     string   `json:"human_category_name,omitempty"`
	Models   []string `json:"supported_models,omitempty"`
	Features []string `json:"supported_features,omitempty"`
}
type WebCatalog struct {
	Models     []WebModel    `json:"models"`
	Categories []WebCategory `json:"categories,omitempty"`
}
type ModelBinding struct {
	Hash         string               `json:"catalog_hash,omitempty"`
	ObservedAt   time.Time            `json:"observed_at,omitempty"`
	LastAttempt  time.Time            `json:"last_attempt_at,omitempty"`
	LastError    string               `json:"last_error,omitempty"`
	NeedsRefresh bool                 `json:"needs_refresh,omitempty"`
	DefaultModel string               `json:"default_model,omitempty"`
	Verified     map[string]time.Time `json:"verified,omitempty"`
	Rejected     map[string]time.Time `json:"rejected_until,omitempty"`
}
type registryState struct {
	Version  int                     `json:"version"`
	Catalogs map[string]WebCatalog   `json:"catalogs"`
	Accounts map[string]ModelBinding `json:"accounts"`
}
type Registry struct {
	mu        sync.RWMutex
	path      string
	state     registryState
	loadError error
}

func NewRegistry(path string) *Registry {
	r := &Registry{path: path, state: registryState{Version: 1, Catalogs: map[string]WebCatalog{}, Accounts: map[string]ModelBinding{}}}
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return r
	}
	if e == nil {
		e = json.Unmarshal(b, &r.state)
	}
	if e == nil && (r.state.Version != 1 || r.state.Catalogs == nil || r.state.Accounts == nil) {
		e = errors.New("unsupported model cache schema")
	}
	if e != nil {
		r.loadError = e
		r.state = registryState{Version: 1, Catalogs: map[string]WebCatalog{}, Accounts: map[string]ModelBinding{}}
	}
	return r
}
func ParseWebCatalog(raw map[string]any) (WebCatalog, error) {
	b, e := json.Marshal(raw)
	if e != nil {
		return WebCatalog{}, e
	}
	var c WebCatalog
	if e = json.Unmarshal(b, &c); e != nil {
		return c, e
	}
	if len(c.Models) == 0 {
		return c, errors.New("upstream returned no models; retained previous catalog")
	}
	seen := map[string]bool{}
	models := []WebModel{}
	for _, m := range c.Models {
		m.Slug = strings.TrimSpace(m.Slug)
		if m.Slug == "" || len(m.Slug) > 160 || strings.ContainsAny(m.Slug, "/\\\n\r\t ") || seen[m.Slug] {
			continue
		}
		seen[m.Slug] = true
		m.Title = strings.TrimSpace(m.Title)
		sort.Strings(m.EnabledTools)
		models = append(models, m)
	}
	if len(models) == 0 {
		return c, errors.New("upstream returned no valid model identifiers")
	}
	c.Models = models
	sort.Slice(c.Models, func(i, j int) bool { return c.Models[i].Slug < c.Models[j].Slug })
	for i := range c.Categories {
		sort.Strings(c.Categories[i].Models)
		sort.Strings(c.Categories[i].Features)
	}
	sort.Slice(c.Categories, func(i, j int) bool { return c.Categories[i].ID < c.Categories[j].ID })
	return c, nil
}
func (m WebModel) ChatEnabled() bool {
	return !m.WorkMode && m.Slug != "research" && (m.Slug == "auto" || strings.HasPrefix(m.Slug, "gpt-")) && !strings.Contains(m.Slug, "image") && !strings.HasSuffix(m.Slug, "-wm")
}

// The two explicit compatibility entries have working request traces. Web's
// discovery endpoint is not exhaustive (notably for the image conversation slug).
func CompatibilityModels() []Spec {
	return []Spec{{ID: "auto", Name: "Auto", OwnedBy: "openai", Capability: Chat, Enabled: true}, {ID: "gpt-5-3", Name: "GPT-5.3 (verified compatibility)", OwnedBy: "openai", Capability: Chat, Enabled: true}}
}
func IsCompatibilityModel(slug string) bool { return slug == "auto" || slug == "gpt-5-3" }
func (r *Registry) update(fn func(*registryState)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loadError != nil {
		return fmt.Errorf("model cache requires recovery: %w", r.loadError)
	}
	b, _ := json.Marshal(r.state)
	var next registryState
	_ = json.Unmarshal(b, &next)
	fn(&next)
	// Keep catalogs referenced by current account bindings; no unbounded snapshots.
	used := map[string]bool{}
	for _, a := range next.Accounts {
		used[a.Hash] = true
	}
	for h := range next.Catalogs {
		if !used[h] {
			delete(next.Catalogs, h)
		}
	}
	b, e := json.MarshalIndent(next, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(r.path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(r.path), ".models-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e == nil {
		e = os.Rename(name, r.path)
	}
	if e != nil {
		return e
	}
	r.state = next
	return nil
}
func (r *Registry) Record(id, defaultModel string, raw map[string]any, fetchError error) error {
	var c WebCatalog
	err := fetchError
	if err == nil {
		c, err = ParseWebCatalog(raw)
	}
	now := time.Now().UTC()
	saveErr := r.update(func(s *registryState) {
		a := s.Accounts[id]
		a.LastAttempt = now
		if err != nil {
			a.LastError = "upstream model discovery failed; previous catalog retained"
		} else {
			b, _ := json.Marshal(c)
			hash := sha256.Sum256(b)
			a.Hash = hex.EncodeToString(hash[:])
			s.Catalogs[a.Hash] = c
			a.ObservedAt = now
			a.DefaultModel = defaultModel
			a.LastError = ""
			a.NeedsRefresh = false
		}
		s.Accounts[id] = a
	})
	if saveErr != nil {
		return saveErr
	}
	return err
}
func (r *Registry) Due(id string, force bool) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a := r.state.Accounts[id]
	if r.loadError != nil || time.Since(a.LastAttempt) < 2*time.Minute {
		return false
	}
	return force || a.NeedsRefresh || a.Hash == "" || time.Since(a.ObservedAt) > CatalogTTL
}
func (r *Registry) Supports(id, slug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a := r.state.Accounts[id]
	if until := a.Rejected[slug]; until.After(time.Now()) {
		return false
	}
	if IsCompatibilityModel(slug) {
		return true
	}
	c := r.state.Catalogs[a.Hash]
	for _, m := range c.Models {
		if m.Slug == slug {
			return m.ChatEnabled()
		}
	}
	return false
}
func (r *Registry) RecordOutcome(id, slug string, success, rejected bool) error {
	if !success && !rejected {
		return nil
	}
	return r.update(func(s *registryState) {
		a := s.Accounts[id]
		if a.Verified == nil {
			a.Verified = map[string]time.Time{}
		}
		if a.Rejected == nil {
			a.Rejected = map[string]time.Time{}
		}
		if success {
			a.Verified[slug] = time.Now().UTC()
			delete(a.Rejected, slug)
		} else {
			a.Rejected[slug] = time.Now().Add(2 * time.Minute)
			a.NeedsRefresh = true
		}
		s.Accounts[id] = a
	})
}
func (r *Registry) Specs(active []string) []Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	found := map[string]Spec{}
	for _, m := range CompatibilityModels() {
		found[m.ID] = m
	}
	for _, id := range active {
		a := r.state.Accounts[id]
		for _, m := range r.state.Catalogs[a.Hash].Models {
			if m.ChatEnabled() {
				found[m.Slug] = Spec{ID: m.Slug, Name: m.Title, OwnedBy: "openai", Created: a.ObservedAt.Unix(), Capability: Chat, Enabled: true}
			}
		}
	}
	out := []Spec{}
	for _, s := range found {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (r *Registry) Summary(active []string) map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	models := map[string]map[string]any{}
	stale, ready, failed := 0, 0, 0
	for _, id := range active {
		a := r.state.Accounts[id]
		if a.Hash != "" {
			ready++
			seen[a.Hash] = true
			if time.Since(a.ObservedAt) > CatalogTTL {
				stale++
			}
		}
		if a.LastError != "" {
			failed++
		}
		for _, m := range r.state.Catalogs[a.Hash].Models {
			v := models[m.Slug]
			if v == nil {
				v = map[string]any{"id": m.Slug, "name": m.Title, "reasoning_type": m.ReasoningType, "enabled_tools": m.EnabledTools, "enabled": m.ChatEnabled(), "discovered": true, "verified": false, "temporarily_unavailable": true}
				models[m.Slug] = v
			}
			if !a.Rejected[m.Slug].After(time.Now()) {
				v["temporarily_unavailable"] = false
			}
			if !a.Verified[m.Slug].IsZero() {
				v["verified"] = true
			}
		}
	}
	list := []map[string]any{}
	for _, v := range models {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i]["id"].(string) < list[j]["id"].(string) })
	return map[string]any{"accounts_ready": ready, "accounts_pending": len(active) - ready, "accounts_stale": stale, "accounts_refresh_failed": failed, "distinct_catalogs": len(seen), "ttl_seconds": int(CatalogTTL.Seconds()), "load_error": r.loadError != nil, "discovered_models": list, "compatibility_models": []string{"auto", "gpt-5-3"}}
}
