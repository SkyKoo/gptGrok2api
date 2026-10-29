package register

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// Follow a retry chain explicitly in tests; production getJob always returns
// the requested immutable attempt rather than silently following successors.
func latestAttempt(get func(string) (RegistrationJob, error), id string) (RegistrationJob, error) {
	for n := 0; n < 32; n++ {
		j, err := get(id)
		if err != nil || j.RetryJobID == "" {
			return j, err
		}
		id = j.RetryJobID
	}
	return RegistrationJob{}, errors.New("retry chain cycle")
}

type attemptMailbox struct {
	grokSavedMailStub
	claimJob         atomic.Value
	committedAccount atomic.Value
}

func (m *attemptMailbox) PrepareFor(ctx context.Context, id string, box Mailbox) error {
	m.claimJob.Store(id)
	return m.Prepare(ctx, box)
}
func (m *attemptMailbox) Commit(_ context.Context, id string, _ Mailbox) error {
	m.committedAccount.Store(id)
	return nil
}

func TestCompensationCreatesChildWithNewIdentityAndOriginalMailboxClaim(t *testing.T) {
	for _, platform := range []string{"openai", "grok"} {
		t.Run(platform, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			parent := compensationFixture(time.Now())
			if platform == "grok" {
				parent.Mailbox.AccountID = "hme-account"
			}
			routing := resinRoutingFixture()
			lease, err := routing.Manager.AcquireTask(context.Background(), "group:shared", platform, "registration", nil, parent.ID)
			if err != nil {
				t.Fatal(err)
			}
			parent.ProxyBinding = &lease.Binding
			lease.Release()
			writeCompensationJobs(t, path, parent)
			mail := &attemptMailbox{}
			var factoryCalls atomic.Int32
			checkDurable := func() {
				factoryCalls.Add(1)
				raw, err := os.ReadFile(path)
				var jobs []RegistrationJob
				if err != nil || json.Unmarshal(raw, &jobs) != nil || len(jobs) != 2 || jobs[0].RetryJobID != jobs[1].ID || jobs[1].ProxyBinding == nil {
					t.Error("child and proxy identity must be persisted before network work")
				}
			}
			var retry func(map[string]any, string) error
			var get func(string) (RegistrationJob, error)
			var snapshot func() map[string]any
			cfg := freeTestConfig()
			if platform == "openai" {
				e := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) { checkDurable(); return mail, &fakeFlow{}, nil }, func(context.Context, string, map[string]any) error { return nil }, func(context.Context, string, map[string]any) error { return nil })
				e.SetProxyRouting(routing)
				retry, get, snapshot = e.RetryRegistration, e.getJob, e.Snapshot
			} else {
				cfg = grokConfigFixture()
				e := NewGrokEngine(path, func(GrokConfig) (MailSource, GrokRegistrationFlow, error) {
					checkDurable()
					return mail, &grokSavedFlowStub{}, nil
				}, func(context.Context, string, map[string]any) error { return nil }, func(context.Context, string, map[string]any) error { return nil })
				e.SetProxyRouting(routing)
				retry, get, snapshot = e.RetryRegistration, e.getJob, e.Snapshot
			}
			cfg["proxy"] = "group:shared"
			if err := retry(cfg, parent.ID); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for snapshot()["running"] == true && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if snapshot()["running"] == true {
				t.Fatal("child did not finish")
			}
			original, _ := get(parent.ID)
			child, err := get(original.RetryJobID)
			if err != nil {
				t.Fatal(err)
			}
			if child.ID == parent.ID || child.ParentJobID != parent.ID || child.RootJobID != parent.ID || child.Status != "completed" {
				t.Fatalf("invalid child: %+v", child)
			}
			if original.Status != parent.Status || original.Error != parent.Error || !reflect.DeepEqual(original.ProxyBinding, parent.ProxyBinding) {
				t.Fatal("original attempt rewritten")
			}
			if child.ProxyBinding == nil || child.ProxyBinding.ResinAccount == parent.ProxyBinding.ResinAccount {
				t.Fatal("child reused parent proxy identity")
			}
			if mail.claimJob.Load() != parent.ID || mail.committedAccount.Load() != child.ID || mail.prepared.Load() != 1 {
				t.Fatal("mailbox claim duplicated or account assigned to parent")
			}
			// Acquire would fail in this stub; successful completion proves no alias was created.
			if child.Mailbox != parent.Mailbox || factoryCalls.Load() != 1 {
				t.Fatal("mailbox changed or repeated registration")
			}
			if err := retry(cfg, parent.ID); err == nil || snapshot()["jobs_total"] != 2 {
				t.Fatal("stale parent request created another child")
			}
			jobs := snapshot()["jobs"].([]any)
			if jobs[0].(map[string]any)["can_retry_registration"] != false || jobs[1].(map[string]any)["parent_job_id"] != parent.ID {
				t.Fatal("public history lost lineage")
			}

		})
	}
}

func TestNewAttemptPreservesBudgetAndCooldown(t *testing.T) {
	now := time.Now()
	p := compensationFixture(now)
	p.AutoRetry.Attempts = 3
	p.RootJobID = "root"
	routing := resinRoutingFixture()
	lease, err := routing.Manager.AcquireTask(context.Background(), "group:shared", "grok", "registration", nil, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	p.ProxyBinding = &lease.Binding
	p.ProxyBinding.BlockedUntil = now.Add(time.Minute)
	_, err = newRegistrationAttempt(p, now)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "registration_cooldown" || failure.RetryAfter != p.ProxyBinding.BlockedUntil.UTC().Format(time.RFC3339) {
		t.Fatal("new identity bypassed cooldown", err)
	}
	child, err := newRegistrationAttempt(p, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if child.AutoRetry.Attempts != 3 || child.RootJobID != "root" || mailboxClaimJobID(child) != "root" || child.ProxyBinding != nil {
		t.Fatal("new task lost chain state")
	}
	child.AutoRetry.Attempts = 0
	if p.AutoRetry.Attempts != 3 {
		t.Fatal("child shares mutable retry state")
	}
}

func TestCompensationJournalFailureDoesNotCreateChildOrStartNetwork(t *testing.T) {
	for _, platform := range []string{"openai", "grok"} {
		t.Run(platform, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jobs.json")
			p := compensationFixture(time.Now())
			writeCompensationJobs(t, path, p)
			var calls atomic.Int32
			cfg := freeTestConfig()
			var retry func(map[string]any, string) error
			var snapshot func() map[string]any
			var get func(string) (RegistrationJob, error)
			if platform == "openai" {
				e := NewFreeEngine(path, func(FreeConfig) (MailSource, RegistrationFlow, error) {
					calls.Add(1)
					return nil, nil, errors.New("unexpected factory")
				}, nil, nil)
				e.path = t.TempDir()
				retry, get, snapshot = e.RetryRegistration, e.getJob, e.Snapshot
			} else {
				cfg = grokConfigFixture()
				e := NewGrokEngine(path, func(GrokConfig) (MailSource, GrokRegistrationFlow, error) {
					calls.Add(1)
					return nil, nil, errors.New("unexpected factory")
				}, nil, nil)
				e.path = t.TempDir()
				retry, get, snapshot = e.RetryRegistration, e.getJob, e.Snapshot
			}
			if err := retry(cfg, p.ID); err == nil {
				t.Fatal("save should fail")
			}
			original, _ := get(p.ID)
			if calls.Load() != 0 || snapshot()["jobs_total"] != 1 || original.RetryJobID != "" || original.Error != p.Error {
				t.Fatal("partial child commit or network work")
			}
		})
	}
}
