package rolling

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"
)

type Route struct {
	Protocol string `json:"protocol"`
	Upstream string `json:"upstream"`
	Held     bool   `json:"held"`
	Epoch    uint64 `json:"epoch"`
}

// Gateway keeps public and internal clients on one stable address. During the
// bounded single-writer handoff, it waits before reading/forwarding request
// bodies. It never replays a submitted POST or a partially delivered stream.
type Gateway struct {
	mu             sync.Mutex
	route          Route
	path           string
	allowed        map[string]bool
	active, queued int
	gate           chan struct{}
	limit          int
	wait           time.Duration
	transport      *http.Transport
}

func NewGateway(path, initial string, allowed []string, limit int, wait time.Duration, held ...bool) (*Gateway, error) {
	if limit < 1 || wait <= 0 {
		return nil, errors.New("bounded queue required")
	}
	g := &Gateway{path: path, allowed: map[string]bool{}, limit: limit, wait: wait, gate: make(chan struct{}), transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, MaxIdleConns: 128, MaxIdleConnsPerHost: 128, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 0}}
	for _, value := range allowed {
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("invalid allowed upstream")
		}
		g.allowed[value] = true
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(raw, &g.route) != nil || g.route.Protocol != Protocol {
			return nil, errors.New("invalid gateway state")
		}
	} else if os.IsNotExist(err) {
		if !g.allowed[initial] {
			return nil, errors.New("initial upstream is outside slot allowlist")
		}
		g.route = Route{Protocol: Protocol, Upstream: initial, Held: len(held) > 0 && held[0]}
		if err = g.persist(g.route); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	if !g.allowed[g.route.Upstream] {
		return nil, errors.New("persisted upstream is outside slot allowlist")
	}
	if !g.route.Held {
		close(g.gate)
	}
	return g, nil
}
func (g *Gateway) persist(route Route) error {
	raw, _ := json.Marshal(route)
	return atomicState(g.path, raw)
}
func (g *Gateway) Status() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return map[string]any{"protocol": Protocol, "route": g.route, "active_requests": g.active, "queued_requests": g.queued, "queue_limit": g.limit, "wait_budget_seconds": g.wait.Seconds()}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/livez" {
		w.WriteHeader(200)
		return
	}
	deadline := time.NewTimer(g.wait)
	defer deadline.Stop()
	countedQueue := false
	for {
		g.mu.Lock()
		if !g.route.Held {
			if countedQueue {
				g.queued--
			}
			upstream := g.route.Upstream
			// Register-events is observation only and can reconnect on handoff.
			tracked := !(r.Method == "GET" && r.URL.Path == "/api/register/events")
			if tracked {
				g.active++
			}
			g.mu.Unlock()
			if tracked {
				defer func() { g.mu.Lock(); g.active--; g.mu.Unlock() }()
			}
			u, _ := url.Parse(upstream)
			proxy := httputil.NewSingleHostReverseProxy(u)
			proxy.Transport = g.transport
			proxy.FlushInterval = -1
			proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "active backend unavailable", 502)
			}
			proxy.ServeHTTP(w, r)
			return
		}
		if !countedQueue {
			if g.queued >= g.limit {
				g.mu.Unlock()
				http.Error(w, "handoff queue is full", 429)
				return
			}
			g.queued++
			countedQueue = true
		}
		gate := g.gate
		g.mu.Unlock()
		select {
		case <-r.Context().Done():
			g.mu.Lock()
			g.queued--
			g.mu.Unlock()
			return
		case <-deadline.C:
			g.mu.Lock()
			g.queued--
			g.mu.Unlock()
			http.Error(w, "handoff exceeded waiting budget", 503)
			return
		case <-gate:
		}
	}
}

// Compare-and-swap makes an uncertain control reply inspectable. A timeout
// never authorizes repeating a cutover or restoring a different route.
func (g *Gateway) Transition(action string, epoch uint64, target string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if epoch != g.route.Epoch {
		return errors.New("stale gateway epoch")
	}
	next := g.route
	switch action {
	case "hold":
		if next.Held || g.active != 0 {
			return errors.New("gateway is not idle")
		}
		next.Held = true
	case "route":
		if !next.Held || g.active != 0 || !g.allowed[target] {
			return errors.New("route requires idle held gateway and an approved slot")
		}
		next.Upstream = target
	case "release":
		if !next.Held {
			return errors.New("gateway is not held")
		}
		next.Held = false
	default:
		return errors.New("unknown transition")
	}
	next.Epoch++
	if err := g.persist(next); err != nil {
		return err
	}
	if !g.route.Held && next.Held {
		g.gate = make(chan struct{})
	}
	g.route = next
	if !next.Held {
		close(g.gate)
	}
	g.transport.CloseIdleConnections()
	return nil
}

func (g *Gateway) Control() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/status" {
			_ = json.NewEncoder(w).Encode(g.Status())
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var body struct {
			Epoch    *uint64 `json:"epoch"`
			Upstream string  `json:"upstream"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || body.Epoch == nil {
			w.WriteHeader(400)
			return
		}
		if err := g.Transition(r.URL.Path[1:], *body.Epoch, body.Upstream); err != nil {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(g.Status())
	})
}
