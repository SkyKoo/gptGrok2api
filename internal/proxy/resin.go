package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
)

// ValidateNodeType never infers gateway semantics from a hostname or username.
func ValidateNodeType(kind, raw string) error {
	if kind == "" || kind == "standard" {
		return nil
	}
	if kind != "resin" {
		return errors.New("unsupported proxy type")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User == nil {
		return errors.New("Resin requires an HTTP(S) gateway, Platform and proxy Token")
	}
	platform := u.User.Username()
	token, _ := u.User.Password()
	if platform == "" || strings.ContainsAny(platform, ".:/\\ \t\r\n") || token == "" {
		return errors.New("Resin requires a Platform without Account suffix and a nonempty proxy Token")
	}
	return nil
}

func taskResinBinding(raw string, b *Binding, taskIDs []string) error {
	if err := ValidateNodeType("resin", raw); err != nil {
		return ErrBindingUnavailable
	}
	u, _ := url.Parse(raw)
	platform := u.User.Username()
	if b.ResinAccount != "" {
		if b.ResinPlatform != platform {
			return ErrBindingUnavailable
		}
		return nil
	}
	id := ""
	if len(taskIDs) > 0 {
		id = taskIDs[0]
	}
	if id == "" {
		var seed [16]byte
		if _, err := rand.Read(seed[:]); err != nil {
			return err
		}
		id = hex.EncodeToString(seed[:])
	}
	// Hash rather than exposing email addresses or other caller-provided IDs.
	b.ResinAccount = "task-" + proxyHash(b.Platform + "\x00" + b.Purpose + "\x00" + id)[:32]
	b.ResinPlatform = platform
	return nil
}

func boundResinURL(raw string, b Binding) (string, error) {
	if b.ResinAccount == "" {
		return raw, nil
	}
	if err := ValidateNodeType("resin", raw); err != nil {
		return "", ErrBindingUnavailable
	}
	u, _ := url.Parse(raw)
	if u.User.Username() != b.ResinPlatform || strings.ContainsAny(b.ResinAccount, ".:/\\ \t\r\n") {
		return "", ErrBindingUnavailable
	}
	token, _ := u.User.Password()
	u.User = url.UserPassword(b.ResinPlatform+"."+b.ResinAccount, token)
	return u.String(), nil
}

// RegistrationResinChannel is a credential-free reference. Its gateway is
// configured independently of ordinary proxy nodes and groups.
const RegistrationResinChannel = "resin:registration"

// ConfigureResinChannel keeps credentials in configuration, never in a task's
// binding. Changing/removing the gateway invalidates old endpoint hashes.
func (m *Manager) ConfigureResinChannel(reference, raw string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resinChannels == nil {
		m.resinChannels = map[string]string{}
	}
	if strings.HasPrefix(reference, "resin:") {
		m.resinChannels[reference] = strings.TrimSpace(raw)
	}
}

func (m *Manager) acquireResinChannel(reference string, b Binding, taskIDs []string) (*TaskLease, error) {
	if m == nil {
		return nil, ErrBindingUnavailable
	}
	m.mu.Lock()
	raw := m.resinChannels[reference]
	m.mu.Unlock()
	if ValidateNodeType("resin", raw) != nil || (b.EndpointHash != "" && b.EndpointHash != proxyHash(raw)) {
		return nil, ErrBindingUnavailable
	}
	if err := taskResinBinding(raw, &b, taskIDs); err != nil {
		return nil, err
	}
	b.Reference, b.EndpointHash = reference, proxyHash(raw)
	resolved, err := boundResinURL(raw, b)
	if err != nil {
		return nil, err
	}
	return &TaskLease{URL: resolved, Binding: b}, nil
}
