package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func TestRegistrationTargetSettingsAPIAndScheduledBatch(t *testing.T) {
	s := scheduleServer(t)
	h := s.Handler()
	save := func(body string) map[string]any {
		t.Helper()
		response := adminRequest(h, http.MethodPost, "/api/register", strings.NewReader(body))
		if response.Code != 200 {
			t.Fatalf("save status=%d body=%s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "PRIVATE-PASSWORD") {
			t.Fatal("mail password exposed")
		}
		var payload map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return mapValue(payload["register"])
	}
	save(`{"proxy":"http://openai.example.test:8080","total":3,"threads":2}`)
	save(`{"target":"grok","proxy":"direct","total":1,"threads":1}`)
	got := save(`{"target":"openai"}`)
	if got["proxy"] != "http://openai.example.test:8080" || intValue(got["total"]) != 3 || intValue(got["threads"]) != 2 {
		t.Fatal("switch did not restore OpenAI")
	}
	var openaiCount, grokCount atomic.Int32
	s.freeRegister = registerruntime.NewFreeEngine(filepath.Join(t.TempDir(), "openai.json"), func(c registerruntime.FreeConfig) (registerruntime.MailSource, registerruntime.RegistrationFlow, error) {
		if c.Proxy != "http://openai.example.test:8080" || c.Total != 3 || c.Threads != 2 {
			t.Error("OpenAI runtime used another profile")
		}
		openaiCount.Add(1)
		return nil, nil, &registerruntime.Failure{Stage: "test", Code: "fixture"}
	}, nil, nil)
	s.grokRegister = registerruntime.NewGrokEngine(filepath.Join(t.TempDir(), "grok.json"), func(c registerruntime.GrokConfig) (registerruntime.MailSource, registerruntime.GrokRegistrationFlow, error) {
		if c.Proxy != "direct" || c.Total != 1 || c.Threads != 1 {
			t.Error("Grok runtime used another profile")
		}
		grokCount.Add(1)
		return nil, nil, &registerruntime.Failure{Stage: "test", Code: "fixture"}
	}, nil, nil)
	for _, target := range []string{"openai", "grok"} {
		save(`{"target":"` + target + `"}`)
		response := adminRequest(h, http.MethodPost, "/api/register/schedule", strings.NewReader(`{"enabled":true,"interval_minutes":1}`))
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
		s.runRegistrationSchedule(scheduleDue(t, s))
		deadline := time.Now().Add(3 * time.Second)
		for s.freeRegister.Snapshot()["running"] == true || s.grokRegister.Snapshot()["running"] == true {
			if time.Now().After(deadline) {
				t.Fatal("fixture batch timed out")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if openaiCount.Load() != 3 || grokCount.Load() != 1 {
		t.Fatalf("batches: openai=%d grok=%d", openaiCount.Load(), grokCount.Load())
	}
}

func TestRegistrationResinTargetBindingsDoNotChangeOnOtherTargetEdits(t *testing.T) {
	s := scheduleServer(t)
	save := func(body string) {
		t.Helper()
		if res := adminRequest(s.Handler(), http.MethodPost, "/api/register", strings.NewReader(body)); res.Code != 200 {
			t.Fatal(res.Body.String())
		}
	}
	save(`{"target":"openai","proxy":"resin:registration","resin_proxy_url":"http://OPENAI:fixture-a@resin-a.example.test:2260"}`)
	acquire := func(ref, platform, job string) string {
		t.Helper()
		lease, err := s.proxyManager.AcquireTask(context.Background(), ref, platform, "registration", nil, job)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		return lease.URL
	}
	legacy := acquire("resin:registration", "openai", "legacy")
	first := acquire("resin:registration:openai", "openai", "new-openai")
	save(`{"target":"grok","proxy":"resin:registration","resin_proxy_url":"http://GROK:fixture-b@resin-b.example.test:2260"}`)
	grok := acquire("resin:registration:grok", "grok", "new-grok")
	if !strings.Contains(grok, "resin-b.example.test") || !strings.Contains(first, "resin-a.example.test") {
		t.Fatal("target Resin gateways mixed")
	}
	if acquire("resin:registration", "openai", "legacy") != legacy || acquire("resin:registration:openai", "openai", "new-openai") != first {
		t.Fatal("editing Grok changed existing OpenAI route")
	}
}
