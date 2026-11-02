package httpapi

import (
	"context"
	"errors"
	"fmt"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	register "github.com/auucoder/gptgrok2api-go/internal/register"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGrokRegistrationVerificationRecords403ThenRecoversSavedAccount(t *testing.T) {
	var status atomic.Int32
	status.Store(403)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		if status.Load() == 200 {
			fmt.Fprint(w, `{"remainingQueries":30,"totalQueries":30}`)
		} else {
			fmt.Fprint(w, "private SECRET")
		}
	}))
	defer up.Close()
	s := New(adminTestConfig(t.TempDir()))
	s.grokQuota = provider.NewGrokQuota(up.URL, up.Client(), nil)
	a, _, err := s.registerStore.AddAccount(map[string]any{"registration_job_id": "job-recover", "sso": "secret-session", "enabled": false, "status": "待验证"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.verifyRegisteredGrokAccount(context.Background(), "job-recover", a)
	var f *register.Failure
	if !errors.As(err, &f) || f.UpstreamStatus != 403 || f.Operation != "quota_probe=access_denied" {
		t.Fatalf("missing rejection: %v", err)
	}
	items, _ := s.registerStore.ExportAccounts([]string{stringValue(a["id"])})
	if boolValue(items[0]["enabled"], true) || stringValue(items[0]["status"]) != "待验证" {
		t.Fatal("enabled rejected account")
	}
	if strings.Contains(stringValue(items[0]["last_runtime_error"]), "SECRET") {
		t.Fatal("leaked response")
	}
	pool, _ := s.grokRuntimeStore.AccountList()
	if len(pool) != 0 {
		t.Fatal("rejected account in pool")
	}
	status.Store(200)
	if err = s.verifyRegisteredGrokAccount(context.Background(), "job-recover", a); err != nil {
		t.Fatal(err)
	}
	items, _ = s.registerStore.ExportAccounts([]string{stringValue(a["id"])})
	if !boolValue(items[0]["enabled"], false) || !boolValue(items[0]["registration_verified"], false) || stringValue(items[0]["status"]) != "active" {
		t.Fatal("archive not recovered")
	}
	pool, _ = s.grokRuntimeStore.AccountList()
	if len(pool) != 1 {
		t.Fatalf("pool count %d", len(pool))
	}
	if stringValue(items[0]["last_runtime_error"]) != "" {
		t.Fatal("stale failure survived success")
	}
}
