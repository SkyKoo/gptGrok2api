package rolling

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeApp(handler http.Handler, jobs *atomic.Int32) *Application {
	return &Application{Handler: handler, Pause: func(bool) {}, Status: func() map[string]int { return map[string]int{"jobs": int(jobs.Load())} }, Start: func() {}, Shutdown: func(context.Context) error { return nil }, Seal: func(context.Context) error { return nil }}
}
func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for !condition() && time.Now().Before(end) {
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition did not converge")
	}
}

func TestStandbyDoesNotInitializeUntilExclusiveLeaseTransfers(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	var jobs atomic.Int32
	factory := func() (*Application, error) { calls.Add(1); return fakeApp(http.NotFoundHandler(), &jobs), nil }
	old, _ := NewInstance(filepath.Join(dir, "writer.lock"), filepath.Join(dir, "old.json"), factory)
	if err := old.Activate(); err != nil {
		t.Fatal(err)
	}
	next, _ := NewInstance(filepath.Join(dir, "writer.lock"), filepath.Join(dir, "next.json"), factory)
	if calls.Load() != 1 {
		t.Fatal("standby initialized shared state")
	}
	if err := next.Activate(); err == nil || calls.Load() != 1 {
		t.Fatal("second writer admitted", err)
	}
	jobs.Store(1)
	if err := old.Freeze(); err == nil {
		t.Fatal("live job frozen")
	}
	jobs.Store(0)
	if err := old.Freeze(); err != nil {
		t.Fatal(err)
	}
	if old.Status()["writer_lease"] != false {
		t.Fatal("sealed process retained writer")
	}
	if err := old.Pause(false); err == nil {
		t.Fatal("sealed memory reactivated")
	}
	if err := next.Activate(); err != nil {
		t.Fatal(err)
	}
	defer next.lease.Close()
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
}

func TestGatewayHandoffQueuesPostExactlyOnceAndPreservesStreams(t *testing.T) {
	var oldPosts, newPosts atomic.Int32
	streamStarted, finishStream := make(chan struct{}), make(chan struct{})
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			close(streamStarted)
			<-finishStream
			io.WriteString(w, "data: last\n\n")
			return
		}
		if r.Method == "POST" {
			oldPosts.Add(1)
		}
		io.Copy(w, r.Body)
	}))
	defer old.Close()
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			newPosts.Add(1)
		}
		io.Copy(w, r.Body)
	}))
	defer next.Close()
	g, err := NewGateway(filepath.Join(t.TempDir(), "gateway.json"), old.URL, []string{old.URL, next.URL}, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(g)
	defer front.Close()
	streamDone := make(chan string, 1)
	go func() {
		r, e := http.Get(front.URL + "/stream")
		if e != nil {
			streamDone <- e.Error()
			return
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		streamDone <- string(b)
	}()
	<-streamStarted
	if err := g.Transition("hold", 0, ""); err == nil {
		t.Fatal("live stream interrupted")
	}
	close(finishStream)
	if value := <-streamDone; !strings.Contains(value, "first") || !strings.Contains(value, "last") {
		t.Fatal(value)
	}
	waitUntil(t, func() bool { return g.Status()["active_requests"] == 0 })
	if err := g.Transition("hold", 0, ""); err != nil {
		t.Fatal(err)
	}
	result := make(chan string, 1)
	go func() {
		r, e := http.Post(front.URL, "text/plain", strings.NewReader("unique-input"))
		if e != nil {
			result <- e.Error()
			return
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		result <- string(b)
	}()
	waitUntil(t, func() bool { return g.Status()["queued_requests"] == 1 })
	if oldPosts.Load() != 0 || newPosts.Load() != 0 {
		t.Fatal("body forwarded before handoff")
	}
	if err := g.Transition("route", 1, next.URL); err != nil {
		t.Fatal(err)
	}
	if err := g.Transition("release", 2, ""); err != nil {
		t.Fatal(err)
	}
	if got := <-result; got != "unique-input" {
		t.Fatal(got)
	}
	if oldPosts.Load() != 0 || newPosts.Load() != 1 {
		t.Fatal("POST replayed", oldPosts.Load(), newPosts.Load())
	}
}

func TestGatewayCASPersistenceAndBoundedAdmission(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer backend.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	g, _ := NewGateway(path, backend.URL, []string{backend.URL}, 1, 50*time.Millisecond, true)
	if err := g.Transition("route", 99, backend.URL); err == nil {
		t.Fatal("stale mutation accepted")
	}
	if err := g.Transition("route", 0, "http://unapproved.invalid"); err == nil {
		t.Fatal("unapproved upstream accepted")
	}
	front := httptest.NewServer(g)
	defer front.Close()
	done := make(chan int, 1)
	go func() {
		r, e := http.Get(front.URL)
		if e != nil {
			done <- 0
			return
		}
		r.Body.Close()
		done <- r.StatusCode
	}()
	waitUntil(t, func() bool { return g.Status()["queued_requests"] == 1 })
	r, err := http.Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 429 {
		t.Fatal(r.StatusCode)
	}
	if code := <-done; code != 503 {
		t.Fatal(code)
	}
	waitUntil(t, func() bool { return g.Status()["queued_requests"] == 0 })
	restarted, err := NewGateway(path, backend.URL, []string{backend.URL}, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.Status()["route"].(Route).Held {
		t.Fatal("restart reopened an unfinished handoff")
	}
}

func TestGatewayClientCancellationDoesNotSubmitBody(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer backend.Close()
	g, _ := NewGateway(filepath.Join(t.TempDir(), "state"), backend.URL, []string{backend.URL}, 2, time.Second, true)
	front := httptest.NewServer(g)
	defer front.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", front.URL, strings.NewReader("never-send"))
	done := make(chan struct{})
	go func() {
		r, _ := http.DefaultClient.Do(req)
		if r != nil {
			r.Body.Close()
		}
		close(done)
	}()
	waitUntil(t, func() bool { return g.Status()["queued_requests"] == 1 })
	cancel()
	<-done
	waitUntil(t, func() bool { return g.Status()["queued_requests"] == 0 })
	if err := g.Transition("release", 0, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("cancelled POST was submitted")
	}
}

func TestSealedLegacyBecomesStableGatewayAndRestoresWithoutWriter(t *testing.T) {
	dir := t.TempDir()
	lease, state, route := filepath.Join(dir, "lock"), filepath.Join(dir, "legacy.json"), filepath.Join(dir, "route.json")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "new-instance") }))
	defer backend.Close()
	var calls, jobs atomic.Int32
	factory := func() (*Application, error) { calls.Add(1); return fakeApp(http.NotFoundHandler(), &jobs), nil }
	i, _ := NewInstance(lease, state, factory)
	gatewayFactory := func(existing bool) (*Gateway, error) {
		return NewGateway(route, backend.URL, []string{backend.URL}, 8, time.Second, true)
	}
	i.SetGatewayFactory(gatewayFactory)
	if err := i.Activate(); err != nil {
		t.Fatal(err)
	}
	i.gateway, _ = gatewayFactory(false)
	if err := i.Freeze(); err != nil {
		t.Fatal(err)
	}
	if i.Status()["phase"] != "gateway" {
		t.Fatal(i.Status())
	}
	if err := i.gateway.Transition("release", 0, ""); err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewInstance(lease, state, factory)
	restarted.SetGatewayFactory(gatewayFactory)
	if err := restarted.Restore(); err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(restarted)
	defer front.Close()
	r, err := http.Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if string(body) != "new-instance" || calls.Load() != 1 || restarted.Status()["writer_lease"] != false {
		t.Fatal("proxy reopened old runtime", string(body), calls.Load(), restarted.Status())
	}
}

func TestTelemetryFinishesBeforeLeaseIsReleased(t *testing.T) {
	dir := t.TempDir()
	started, finished := make(chan struct{}), make(chan struct{})
	var jobs atomic.Int32
	i, _ := NewInstance(filepath.Join(dir, "lock"), filepath.Join(dir, "state"), func() (*Application, error) {
		app := fakeApp(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(finished) }), &jobs)
		app.Seal = func(context.Context) error {
			select {
			case <-finished:
				return nil
			default:
				t.Error("lease sealed before observation handler finished")
				return context.Canceled
			}
		}
		return app, nil
	})
	if err := i.Activate(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		i.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/register/events", nil))
		close(done)
	}()
	<-started
	if err := i.Freeze(); err != nil {
		t.Fatal(err)
	}
	<-done
	if i.Status()["writer_lease"] != false {
		t.Fatal("lease not released")
	}
}

func TestFailedInitializerRetainsLeaseAndCannotAutoRestart(t *testing.T) {
	dir := t.TempDir()
	lease, state := filepath.Join(dir, "lock"), filepath.Join(dir, "state")
	failed, _ := NewInstance(lease, state, func() (*Application, error) { return nil, context.Canceled })
	if failed.Activate() == nil {
		t.Fatal("failed initialization accepted")
	}
	defer failed.lease.Close()
	var calls atomic.Int32
	var jobs atomic.Int32
	candidate, _ := NewInstance(lease, filepath.Join(dir, "next"), func() (*Application, error) { calls.Add(1); return fakeApp(http.NotFoundHandler(), &jobs), nil })
	if candidate.Activate() == nil || calls.Load() != 0 {
		t.Fatal("failed initializer lost lease")
	}
	restarted, _ := NewInstance(lease, state, candidate.factory)
	if restarted.Restore() == nil || restarted.Status()["phase"] != "recovery_required" || calls.Load() != 0 {
		t.Fatal("partial init silently replayed")
	}
}

func TestBootstrapQueuesAreBoundedWhileOldDiskWriterSeals(t *testing.T) {
	dir := t.TempDir()
	var jobs atomic.Int32
	entered, finish := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer backend.Close()
	i, _ := NewInstance(filepath.Join(dir, "lock"), filepath.Join(dir, "state"), func() (*Application, error) {
		app := fakeApp(http.NotFoundHandler(), &jobs)
		app.Seal = func(context.Context) error { close(entered); <-finish; return nil }
		return app, nil
	})
	if err := i.Activate(); err != nil {
		t.Fatal(err)
	}
	i.gateway, _ = NewGateway(filepath.Join(dir, "route"), backend.URL, []string{backend.URL}, 1, time.Second, true)
	done := make(chan error, 1)
	go func() { done <- i.Freeze() }()
	<-entered
	front := httptest.NewServer(i)
	defer front.Close()
	accepted := make(chan int, 1)
	go func() {
		res, err := http.Get(front.URL)
		if err != nil {
			accepted <- 0
			return
		}
		res.Body.Close()
		accepted <- res.StatusCode
	}()
	waitUntil(t, func() bool { return i.gateway.Status()["queued_requests"] == 1 })
	res, err := http.Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 429 || i.Status()["writer_lease"] != true {
		t.Fatal("seal bypassed queue bound or lease", res.StatusCode)
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := i.gateway.Transition("release", 0, ""); err != nil {
		t.Fatal(err)
	}
	if code := <-accepted; code != 204 {
		t.Fatal(code)
	}
}
