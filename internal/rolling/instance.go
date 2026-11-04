// Package rolling provides a single-writer handoff for file-backed CFM state.
package rolling

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const Protocol = "cfm-single-writer-v1"

type Application struct {
	Handler  http.Handler
	Pause    func(bool)
	Status   func() map[string]int
	Start    func()
	Shutdown func(context.Context) error
	Seal     func(context.Context) error
}

type telemetryWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type Instance struct {
	mu                   sync.Mutex
	phase                string
	paused               bool
	active               int
	app                  *Application
	lease                *os.File
	leasePath, statePath string
	factory              func() (*Application, error)
	telemetry            map[uint64]*telemetryWatch
	sequence             uint64
	gateway              *Gateway
	gatewayFactory       func(bool) (*Gateway, error)
}

func NewInstance(leasePath, statePath string, factory func() (*Application, error)) (*Instance, error) {
	if !filepath.IsAbs(leasePath) || !filepath.IsAbs(statePath) || leasePath == statePath {
		return nil, errors.New("absolute, distinct runtime paths required")
	}
	i := &Instance{phase: "standby", paused: true, leasePath: leasePath, statePath: statePath, factory: factory, telemetry: map[uint64]*telemetryWatch{}}
	return i, nil
}

func atomicState(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".rolling-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
		if err == nil {
			if dir, openErr := os.Open(filepath.Dir(path)); openErr == nil {
				err = dir.Sync()
				dir.Close()
			} else {
				err = openErr
			}
		}
	}
	return err
}

func (i *Instance) savePhase(phase string) error {
	raw, _ := json.Marshal(map[string]any{"protocol": Protocol, "phase": phase, "paused": i.paused})
	return atomicState(i.statePath, raw)
}

// Restore only a positively completed activation. An intentionally frozen old
// slot must stay passive after a Docker/host restart.
func (i *Instance) Restore() error {
	raw, err := os.ReadFile(i.statePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state struct {
		Protocol string `json:"protocol"`
		Phase    string `json:"phase"`
		Paused   *bool  `json:"paused"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Protocol != Protocol || state.Paused == nil {
		return errors.New("invalid instance state")
	}
	if state.Phase == "active" {
		if err := i.Activate(); err != nil {
			return err
		}
		if !*state.Paused {
			return i.Pause(false)
		}
		return nil
	}
	if state.Phase == "gateway" {
		if i.gatewayFactory == nil {
			return errors.New("gateway factory unavailable")
		}
		gateway, err := i.gatewayFactory(true)
		if err != nil {
			i.phase = "recovery_required"
			return err
		}
		i.gateway = gateway
		i.phase = "gateway"
		return nil
	}
	if state.Phase == "starting" || state.Phase == "failed" {
		i.phase = "recovery_required"
		return errors.New("unfinished activation requires operator recovery")
	}
	if state.Phase != "frozen" {
		i.phase = "recovery_required"
		return errors.New("unknown instance phase")
	}
	i.phase = "frozen"
	return nil
}

func (i *Instance) Activate() error {
	i.mu.Lock()
	if i.phase != "standby" && !(i.phase == "frozen" && i.app == nil) {
		i.mu.Unlock()
		return errors.New("instance is not standby")
	}
	// Never open configuration, journals or Redis consumers before the lease.
	f, err := acquireLease(i.leasePath)
	if err != nil {
		i.mu.Unlock()
		return errors.New("another instance owns the data lease or leases are unsupported")
	}
	i.lease = f
	if err = i.savePhase("starting"); err != nil {
		f.Close()
		i.lease = nil
		i.mu.Unlock()
		return err
	}
	i.phase = "starting"
	i.mu.Unlock()
	app, err := i.factory()
	i.mu.Lock()
	defer i.mu.Unlock()
	if err != nil || app == nil {
		i.phase = "failed"
		_ = i.savePhase("failed")
		return errors.New("application initialization failed; retain lease until process stops")
	}
	i.app = app
	app.Pause(true)
	if err = i.savePhase("active"); err != nil {
		i.phase = "failed"
		return err
	}
	i.phase = "active"
	i.paused = true
	return nil
}

func (i *Instance) Pause(paused bool) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.phase != "active" {
		return errors.New("instance is not active")
	}
	before := i.paused
	i.paused = paused
	if err := i.savePhase("active"); err != nil {
		i.paused = before
		return err
	}
	i.app.Pause(paused)
	if !paused {
		i.app.Start()
	}
	return nil
}

func (i *Instance) Freeze() error {
	var app *Application
	var watches []*telemetryWatch
	phase := "frozen"
	err := func() error {
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.phase != "active" || !i.paused || i.active != 0 {
			return errors.New("instance is not quiescent")
		}
		for _, count := range i.app.Status() {
			if count != 0 {
				return errors.New("background work has not drained")
			}
		}
		if i.gateway != nil {
			if !i.gateway.Status()["route"].(Route).Held {
				return errors.New("gateway must be held before sealing")
			}
			phase = "gateway"
		}
		if err := i.savePhase(phase); err != nil {
			return err
		}
		i.phase = "sealing"
		app = i.app
		for _, watch := range i.telemetry {
			watches = append(watches, watch)
		}
		return nil
	}()
	if err != nil {
		return err
	}
	// New legacy-ingress requests already use the held bounded gateway while
	// telemetry and delayed disk writers finish; do not block them on i.mu.
	// Generation streams were counted and drained before entering this phase.
	for _, watch := range watches {
		watch.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, watch := range watches {
		select {
		case <-watch.done:
		case <-ctx.Done():
			return errors.New("observation stream did not drain")
		}
	}
	if err := app.Seal(ctx); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := releaseLease(i.lease); err != nil {
		return err
	}
	i.app = nil
	i.lease = nil
	i.phase = phase
	return nil
}

func (i *Instance) Status() map[string]any {
	i.mu.Lock()
	defer i.mu.Unlock()
	r := map[string]any{"protocol": Protocol, "phase": i.phase, "paused": i.paused, "active_requests": i.active, "writer_lease": i.lease != nil}
	if i.app != nil {
		r["jobs"] = i.app.Status()
	}
	return r
}

func (i *Instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	if i.phase == "gateway" || (i.phase == "sealing" && i.gateway != nil) {
		gateway := i.gateway
		i.mu.Unlock()
		gateway.ServeHTTP(w, r)
		return
	}
	if i.phase != "active" {
		i.mu.Unlock()
		http.Error(w, "instance is not active", 503)
		return
	}
	if r.URL.Path == "/readyz" {
		i.mu.Unlock()
		w.WriteHeader(200)
		return
	}
	app := i.app
	if r.Method == "GET" && r.URL.Path == "/api/register/events" {
		ctx, cancel := context.WithCancel(r.Context())
		i.sequence++
		id := i.sequence
		watch := &telemetryWatch{cancel: cancel, done: make(chan struct{})}
		i.telemetry[id] = watch
		i.mu.Unlock()
		defer func() { cancel(); i.mu.Lock(); delete(i.telemetry, id); i.mu.Unlock() }()
		defer close(watch.done)
		app.Handler.ServeHTTP(w, r.WithContext(ctx))
		return
	}
	i.active++
	i.mu.Unlock()
	defer func() { i.mu.Lock(); i.active--; i.mu.Unlock() }()
	app.Handler.ServeHTTP(w, r)
}

func (i *Instance) Control() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/gateway/") {
			i.mu.Lock()
			if r.URL.Path == "/gateway/init" && r.Method == "POST" && i.phase == "active" && i.gateway == nil && i.gatewayFactory != nil {
				gateway, err := i.gatewayFactory(false)
				if err != nil {
					i.mu.Unlock()
					w.WriteHeader(409)
					return
				}
				i.gateway = gateway
			}
			gateway := i.gateway
			i.mu.Unlock()
			if gateway == nil {
				w.WriteHeader(409)
				return
			}
			copy := r.Clone(r.Context())
			u := *r.URL
			copy.URL = &u
			copy.URL.Path = strings.TrimPrefix(r.URL.Path, "/gateway")
			if copy.URL.Path == "/init" {
				copy.URL.Path = "/status"
				copy.Method = "GET"
			}
			gateway.Control().ServeHTTP(w, copy)
			return
		}
		if r.URL.Path == "/status" && r.Method == "GET" {
			_ = json.NewEncoder(w).Encode(i.Status())
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var err error
		switch r.URL.Path {
		case "/activate":
			err = i.Activate()
		case "/pause":
			err = i.Pause(true)
		case "/resume":
			err = i.Pause(false)
		case "/freeze":
			err = i.Freeze()
		default:
			w.WriteHeader(404)
			return
		}
		if err != nil {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(i.Status())
	})
}

func (i *Instance) SetGatewayFactory(factory func(bool) (*Gateway, error)) {
	i.gatewayFactory = factory
}

// Close after the HTTP listener has drained. A non-sealed application's lease
// remains held until exit; only a successful irreversible seal may release it.
func (i *Instance) Shutdown(ctx context.Context) error {
	i.mu.Lock()
	app := i.app
	i.mu.Unlock()
	if app != nil {
		return app.Shutdown(ctx)
	}
	return nil
}
