package proxy

import "context"

type accountEgressKey struct{}
type accountEgress struct{ request, resource string }

// WithAccountEgress captures the exits for one account attempt. A later default
// change affects the next attempt, not the upload, generation or download steps
// already in progress. Explicit request selections always take precedence.
func (m *Manager) WithAccountEgress(ctx context.Context, fields map[string]any) context.Context {
	if _, selected := URLSelectionFromContext(ctx); selected {
		return ctx
	}
	if _, selected := ctx.Value(accountEgressKey{}).(accountEgress); selected {
		return ctx
	}
	fields = AccountRoutingFields(fields)
	egress := accountEgress{}
	accountRoute := fields["proxy_binding"] != nil
	for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
		accountRoute = accountRoute || stringValue(fields[key]) != ""
	}
	if accountRoute {
		// A fixed account route also overrides the resource default. Resolve
		// an account group once, so its cursor cannot select a second node.
		egress.request = m.Resolve(fields, false)
		egress.resource = egress.request
	} else if m != nil {
		m.mu.Lock()
		egress.request = m.resolveDefaultLocked(false)
		egress.resource = egress.request
		if m.resourceSelection != "" || m.resourceURL != "" || len(m.resourcePool) > 0 {
			egress.resource = m.resolveDefaultLocked(true)
		}
		m.mu.Unlock()
	}
	return context.WithValue(ctx, accountEgressKey{}, egress)
}

// RequestContext chooses the already pinned account exit when present. WithURL
// may still explicitly select a particular request route, including direct.
func (m *Manager) RequestContext(ctx context.Context, fields map[string]any, resource bool) context.Context {
	if _, selected := URLSelectionFromContext(ctx); selected {
		return ctx
	}
	if egress, selected := ctx.Value(accountEgressKey{}).(accountEgress); selected {
		if resource {
			return WithURL(ctx, egress.resource)
		}
		return WithURL(ctx, egress.request)
	}
	return WithURL(ctx, m.Resolve(fields, resource))
}
