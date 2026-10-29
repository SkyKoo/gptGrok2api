package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

var ErrBindingUnavailable = errors.New("proxy binding unavailable; select a node explicitly before retrying")

const blockedProxyURL = "blocked://proxy-binding-unavailable"

// Binding is account/task metadata, separate from the browser profile. Group
// and default-pool bindings contain references, never proxy credentials.
type Binding struct {
	BlockedUntil time.Time `json:"blocked_until,omitempty"`
	Reference    string    `json:"reference"`
	GroupID      string    `json:"group_id,omitempty"`
	NodeID       string    `json:"node_id,omitempty"`
	EndpointHash string    `json:"endpoint_hash,omitempty"`
	Platform     string    `json:"platform"`
	Purpose      string    `json:"purpose"`
	BoundAt      string    `json:"bound_at"`
}

type taskNode struct {
	url             string
	enabled         bool
	limit, inFlight int
	blocked         map[string]time.Time
}

type TaskLease struct {
	URL     string
	Binding Binding
	manager *Manager
	node    *taskNode
	once    sync.Once
}

func (l *TaskLease) Release() {
	if l == nil || l.manager == nil || l.node == nil {
		return
	}
	l.once.Do(func() {
		l.manager.mu.Lock()
		defer l.manager.mu.Unlock()
		l.node.inFlight--
		l.manager.signalTaskLocked()
	})
}

func proxyHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}
func NodeReference(group, node string) string {
	return "node:" + url.PathEscape(group) + "/" + url.PathEscape(node)
}
func (b Binding) Label() string {
	if b.GroupID != "" {
		return b.GroupID + "/" + b.NodeID
	}
	if b.Reference == "direct" {
		return "direct"
	}
	h := b.EndpointHash
	if len(h) > 12 {
		h = h[:12]
	}
	return "proxy:" + h
}
func ParseBinding(value any) *Binding {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var b Binding
	if json.Unmarshal(raw, &b) != nil || b.Reference == "" {
		return nil
	}
	return &b
}

// ConfigureTaskGroups shares node configuration with existing proxy groups,
// but keeps task capacity/health separate from image-request heuristics.
// Live leases survive refreshes; disabled/deleted references cannot fall back.
func (m *Manager) ConfigureTaskGroups(groups []GroupConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.taskNodes == nil {
		m.taskNodes = map[string]*taskNode{}
	}
	for _, node := range m.taskNodes {
		node.enabled = false
	}
	next := map[string][]string{}
	for _, group := range groups {
		if !group.Enabled || group.ID == "" {
			continue
		}
		for _, cfg := range group.Nodes {
			raw := normalizeURL(cfg.URL)
			if !cfg.Enabled || cfg.ID == "" || !taskProxyCompatible(raw) {
				continue
			}
			ref := NodeReference(group.ID, cfg.ID)
			n := m.taskNodes[ref]
			if n == nil || n.url != raw {
				n = &taskNode{url: raw, blocked: map[string]time.Time{}}
				m.taskNodes[ref] = n
			}
			n.enabled = probeAllowsRuntimeValidation(cfg.LastStatus, cfg.LastError)
			n.limit = cfg.TaskConcurrencyLimit
			if n.limit < 1 {
				n.limit = 1
			}
			if n.limit > 100 {
				n.limit = 100
			}
			next[group.ID] = append(next[group.ID], ref)
		}
	}
	m.taskGroups = next
	if m.taskCursors == nil {
		m.taskCursors = map[string]int{}
	}
	m.signalTaskLocked()
}
func taskProxyCompatible(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "socks5" || u.Scheme == "socks5h"
}
func (m *Manager) signalTaskLocked() {
	if m.taskWake != nil {
		close(m.taskWake)
	}
	m.taskWake = make(chan struct{})
}

// AcquireTask selects once. A saved binding takes precedence over current
// settings, including when a failed task is explicitly resumed after restart.
func (m *Manager) AcquireTask(ctx context.Context, selection, platform, purpose string, saved *Binding) (*TaskLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := Binding{Platform: platform, Purpose: purpose, BoundAt: time.Now().UTC().Format(time.RFC3339Nano)}
	selection = strings.TrimSpace(selection)
	if selection == "" && saved == nil && m != nil {
		m.mu.Lock()
		selection = m.defaultSelection
		m.mu.Unlock()
		// Ordinary default URLs still resolve through the configured pool/router.
		if selection != "direct" && !strings.HasPrefix(selection, "group:") {
			selection = ""
		}
	}
	if saved != nil {
		if time.Now().Before(saved.BlockedUntil) {
			return nil, ErrBindingUnavailable
		}
		b = *saved
		selection = b.Reference
	}
	if !strings.HasPrefix(selection, "group:") && !strings.HasPrefix(selection, "node:") {
		raw := selection
		if saved != nil {
			var err error
			raw, err = m.resolveBinding(b)
			if err != nil {
				return nil, err
			}
		} else if selection == "" {
			raw = m.Resolve(nil, false)
			if raw == "" {
				selection = "direct"
			} else {
				selection = "default:" + proxyHash(raw)
			}
		}
		if selection == "direct" {
			raw = ""
		}
		if raw != "" && !taskProxyCompatible(raw) {
			return nil, ErrBindingUnavailable
		}
		b.Reference, b.EndpointHash = selection, proxyHash(raw)
		return &TaskLease{URL: raw, Binding: b}, nil
	}
	if m == nil {
		return nil, ErrBindingUnavailable
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.mu.Lock()
		refs := []string{selection}
		group := strings.TrimPrefix(selection, "group:")
		isGroup := strings.HasPrefix(selection, "group:")
		start := 0
		if isGroup {
			refs = m.taskGroups[group]
			if len(refs) > 0 {
				start = m.taskCursors[group] % len(refs)
			}
		}
		usable := false
		for i := 0; i < len(refs); i++ {
			index := (start + i) % len(refs)
			ref := refs[index]
			n := m.taskNodes[ref]
			if n == nil || !n.enabled || time.Now().Before(n.blocked[platform+":"+purpose]) {
				continue
			}
			if saved != nil && b.EndpointHash != "" && b.EndpointHash != proxyHash(n.url) {
				continue
			}
			usable = true
			if n.inFlight >= n.limit {
				continue
			}
			n.inFlight++
			if isGroup {
				m.taskCursors[group] = (index + 1) % len(refs)
			}
			parts := strings.SplitN(strings.TrimPrefix(ref, "node:"), "/", 2)
			b.Reference = ref
			b.EndpointHash = proxyHash(n.url)
			b.GroupID, _ = url.PathUnescape(parts[0])
			b.NodeID, _ = url.PathUnescape(parts[1])
			lease := &TaskLease{URL: n.url, Binding: b, manager: m, node: n}
			m.mu.Unlock()
			return lease, nil
		}
		wake := m.taskWake
		m.mu.Unlock()
		if !usable {
			return nil, ErrBindingUnavailable
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
		}
	}
}

func (m *Manager) resolveBinding(b Binding) (string, error) {
	if b.Reference == "direct" {
		return "", nil
	}
	if m == nil {
		return "", ErrBindingUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var raw string
	switch {
	case strings.HasPrefix(b.Reference, "node:"):
		n := m.taskNodes[b.Reference]
		if n == nil || !n.enabled {
			return "", ErrBindingUnavailable
		}
		raw = n.url
	case strings.HasPrefix(b.Reference, "default:"):
		candidates := append([]string{m.url, m.resourceURL}, m.pool...)
		candidates = append(candidates, m.resourcePool...)
		if m.upstreamRouter != nil {
			m.upstreamRouter.refresh()
			m.upstreamRouter.mu.Lock()
			for _, n := range m.upstreamRouter.entries {
				candidates = append(candidates, n.URL)
			}
			m.upstreamRouter.mu.Unlock()
		}
		for _, candidate := range candidates {
			if candidate != "" && "default:"+proxyHash(candidate) == b.Reference {
				raw = candidate
				break
			}
		}
	default:
		raw = normalizeURL(b.Reference) // explicit legacy proxy remains supported
	}
	if !taskProxyCompatible(raw) || (b.EndpointHash != "" && b.EndpointHash != proxyHash(raw)) {
		return "", ErrBindingUnavailable
	}
	return raw, nil
}

// ResolveBound is checked by all request families before their legacy routing.
// An unresolved binding is an error, never permission to use a random/default exit.
func (m *Manager) ResolveBound(fields map[string]any) (string, bool, error) {
	if fields == nil {
		return "", false, nil
	}
	if value, exists := fields["proxy_binding"]; exists && value != nil {
		b := ParseBinding(value)
		if b == nil {
			return "", true, ErrBindingUnavailable
		}
		raw, err := m.resolveBinding(*b)
		return raw, true, err
	}
	for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
		ref := strings.TrimSpace(stringValue(fields[key]))
		if ref == "direct" {
			return "", true, nil
		}
		if strings.HasPrefix(ref, "node:") || strings.HasPrefix(ref, "default:") {
			raw, err := m.resolveBinding(Binding{Reference: ref})
			return raw, true, err
		}
	}
	return "", false, nil
}

// BlockTaskScope records an upstream refusal separately from node reachability.
// It does not affect other platforms or non-registration account requests.
func (m *Manager) BlockTaskScope(b Binding, until time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := m.taskNodes[b.Reference]; n != nil {
		if until.After(n.blocked[b.Platform+":"+b.Purpose]) {
			n.blocked[b.Platform+":"+b.Purpose] = until
		}
		m.signalTaskLocked()
	}
}

// RebindFields is called while the account store lock is held so the binding
// and its audit trail are persisted in the same account-file transaction.
func RebindFields(current map[string]any, next Binding, reason string) map[string]any {
	old := ParseBinding(current["proxy_binding"])
	oldLabel := "legacy/default"
	if old != nil {
		oldLabel = old.Label()
	}
	history := []any{}
	if entries, ok := current["proxy_binding_history"].([]any); ok {
		history = append(history, entries...)
	}
	history = append(history, map[string]any{"at": next.BoundAt, "from": oldLabel, "to": next.Label(), "reason": reason, "actor": "admin"})
	return map[string]any{"proxy": next.Reference, "proxy_binding": next, "proxy_binding_history": history}
}

func (m *Manager) TaskSnapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := []map[string]any{}
	for ref, n := range m.taskNodes {
		blocks := map[string]string{}
		for scope, until := range n.blocked {
			if time.Now().Before(until) {
				blocks[scope] = until.UTC().Format(time.RFC3339)
			}
		}
		rows = append(rows, map[string]any{"reference": ref, "enabled": n.enabled, "in_flight": n.inFlight, "limit": n.limit, "blocked_scopes": blocks})
	}
	return map[string]any{"nodes": rows, "group_count": len(m.taskGroups)}
}

// withExplicitDefault applies global direct/group only when no account route exists.
func (m *Manager) withExplicitDefault(fields map[string]any) map[string]any {
	if m == nil || fields["proxy_binding"] != nil {
		return fields
	}
	for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
		if stringValue(fields[key]) != "" {
			return fields
		}
	}
	m.mu.Lock()
	selection := m.defaultSelection
	m.mu.Unlock()
	if selection != "direct" && !strings.HasPrefix(selection, "group:") {
		return fields
	}
	next := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		next[k] = v
	}
	next["proxy"] = selection
	return next
}

// Request group selection uses its own cursor, without consuming registration slots.
func (m *Manager) resolveRequestGroupLocked(group string) string {
	refs := m.taskGroups[group]
	if len(refs) == 0 {
		return blockedProxyURL
	}
	if m.requestCursors == nil {
		m.requestCursors = map[string]int{}
	}
	start := m.requestCursors[group] % len(refs)
	for i := 0; i < len(refs); i++ {
		index := (start + i) % len(refs)
		if n := m.taskNodes[refs[index]]; n != nil && n.enabled {
			m.requestCursors[group] = (index + 1) % len(refs)
			return n.url
		}
	}
	return blockedProxyURL
}
