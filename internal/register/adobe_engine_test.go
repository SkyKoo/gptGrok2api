package register

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type adobeTestMail struct{}

func (adobeTestMail) Acquire(context.Context, string) (Mailbox, error) {
	return Mailbox{ID: "box", Email: "nod@example.test", AccountID: "mail"}, nil
}
func (adobeTestMail) WaitCode(context.Context, Mailbox, time.Time) (string, error) {
	return "", errors.New("unexpected")
}
func (adobeTestMail) Close() {}

type adobeTestFlow struct {
	calls   *atomic.Int32
	failure error
}

func (f adobeTestFlow) Register(ctx context.Context, id string, box Mailbox, cfg AdobeConfig, code func(context.Context, time.Time) (string, error), progress Progress) (map[string]any, error) {
	f.calls.Add(1)
	if err := progress("adobe_flow_started"); err != nil {
		return nil, err
	}
	if f.failure != nil {
		return nil, f.failure
	}
	return map[string]any{"email": box.Email, "account_id": "identity", "token": "SECRET_TOKEN", "cookie": "SECRET_COOKIE"}, nil
}
func (adobeTestFlow) Close() {}
func adobeTestConfig() map[string]any {
	c := grokConfigFixture()
	c["target"] = "adobe"
	c["total"] = 1
	return c
}
func waitAdobe(t *testing.T, e *AdobeEngine) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !e.Snapshot()["running"].(bool) {
			return
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatal("engine did not finish")
}
func TestAdobeSavedResultEncryptedAndRestartDoesNotRegisterAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	var calls atomic.Int32
	failImport := true
	factory := func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		return adobeTestMail{}, adobeTestFlow{calls: &calls}, nil
	}
	importer := func(context.Context, string, map[string]any) error {
		if failImport {
			return errors.New("storage unavailable")
		}
		return nil
	}
	verify := func(context.Context, string, map[string]any) error { return nil }
	e := NewAdobeEngine(path, "test-key", factory, importer, verify)
	id, err := e.Start(adobeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	waitAdobe(t, e)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET_TOKEN", "SECRET_COOKIE", "nod@example.test"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private journal leaked credentials")
		}
	}
	failImport = false
	e = NewAdobeEngine(path, "test-key", factory, importer, verify)
	if err = e.RetrySavedResult(id); err != nil {
		t.Fatal(err)
	}
	waitAdobe(t, e)
	if calls.Load() != 1 {
		t.Fatal("saved result recovery replayed registration")
	}
	jobs := e.Snapshot()["jobs"].([]any)
	job := jobs[0].(map[string]any)
	if job["status"] != "completed" {
		t.Fatalf("unexpected status %v", job["status"])
	}
	if _, ok := job["account"]; ok {
		t.Fatal("public task exposed account")
	}
	wrong := NewAdobeEngine(path, "wrong-key", factory, importer, verify)
	if _, err = wrong.Start(adobeTestConfig()); err == nil {
		t.Fatal("accepted unreadable encrypted journal")
	}
}
func TestAdobeUnknownSubmissionCannotRetrySavedResult(t *testing.T) {
	var calls atomic.Int32
	e := NewAdobeEngine(filepath.Join(t.TempDir(), "adobe.enc.json"), "test", func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		return adobeTestMail{}, adobeTestFlow{calls: &calls, failure: fail("adobe", "submission_result_unknown")}, nil
	}, nil, nil)
	id, err := e.Start(adobeTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	waitAdobe(t, e)
	if e.RetrySavedResult(id) == nil {
		t.Fatal("unknown submission became retryable")
	}
	if calls.Load() != 1 {
		t.Fatal("registration replayed")
	}
	job := e.Snapshot()["jobs"].([]any)[0].(map[string]any)
	if job["status"] != "registration_pending" || job["can_retry_registration"] != false {
		t.Fatal("unknown state not retained")
	}
}
func TestAdobeSenderAndIndependentConfiguration(t *testing.T) {
	for _, sender := range []string{"Adobe <mail@adobe.com>", "noreply@mail.adobe.com"} {
		if !isAdobeSender(sender) {
			t.Fatal("valid Adobe sender rejected")
		}
	}
	for _, sender := range []string{"Adobe <mail@adobe.com.attacker.test>", "Adobe <mail@attacker.test>", "invalid"} {
		if isAdobeSender(sender) {
			t.Fatal("untrusted sender accepted")
		}
	}
	root := t.TempDir()
	s := New(filepath.Join(root, "register.json"), filepath.Join(root, "accounts.json"))
	if _, err := s.Update(adobeTestConfig()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateForTarget("adobe", map[string]any{"total": 3, "adobe": map[string]any{"country": "US"}}); err != nil {
		t.Fatal(err)
	}
	if intValue(s.ConfigForTarget("adobe")["total"]) != 3 || intValue(s.ConfigForTarget("grok")["total"]) == 3 {
		t.Fatal("target settings crossed channels")
	}
}
