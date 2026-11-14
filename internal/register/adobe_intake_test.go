package register

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
)

// The accepted session must remain the source of credentials until capture;
// once captured, a storage failure must recover without opening any browser.
func TestAdobeAcceptedSessionIntakeSurvivesImportFailureAndRestart(t *testing.T) {
	now := time.Now()
	parent := adobeCompensationFixture(t, now)
	path := filepath.Join(t.TempDir(), "adobe.enc.json")
	var browserCalls, imports, verifies atomic.Int32
	factory := func(AdobeConfig) (MailSource, AdobeRegistrationFlow, error) {
		browserCalls.Add(1)
		return &adobeCompensationMail{}, adobeCompensationFlow{run: func(_ context.Context, cfg AdobeConfig, _ Progress) (map[string]any, error) {
			if cfg.RecoveryMode != "resume_session" || cfg.BrowserProfileID != parent.AdobeRegistration.BrowserProfileID {
				t.Error("accepted account was recreated or lost its original profile")
			}
			return map[string]any{"account_id": "verified-identity", "email": parent.Mailbox.Email, "token": "PRIVATE-INTAKE-TOKEN"}, nil
		}}, nil
	}
	importer := func(_ context.Context, claim string, account map[string]any) error {
		// Decode the on-disk journal without invoking restart recovery.
		raw, readErr := os.ReadFile(path)
		var envelope struct{ Data []byte }
		var persisted []RegistrationJob
		if readErr != nil || json.Unmarshal(raw, &envelope) != nil {
			t.Error("captured account journal is missing")
			return errors.New("journal unavailable")
		}
		plain, openErr := adobe.Open("fixture", envelope.Data)
		if openErr != nil || json.Unmarshal(plain, &persisted) != nil || len(persisted) < 2 {
			t.Error("captured account journal is invalid")
			return errors.New("journal invalid")
		}
		captured := persisted[len(persisted)-1]
		if captured.Account["token"] != "PRIVATE-INTAKE-TOKEN" || claim != parent.ID || account["registration_job_id"] != parent.ID || account["enabled"] != false {
			t.Error("import ran before durable capture or changed account ownership")
		}
		if imports.Add(1) == 1 {
			return errors.New("transient local storage failure")
		}
		return nil
	}
	verifier := func(context.Context, string, map[string]any) error {
		verifies.Add(1)
		return nil
	}
	engine := NewAdobeEngine(path, "fixture", factory, importer, verifier)
	seedAdobeCompensation(t, engine, parent)
	if err := engine.Reregister(adobeTestConfig(), parent.ID); safeFailure(err) != "task: registration_already_accepted" {
		t.Fatalf("accepted creation allowed a fresh signup: %v", err)
	}
	if err := engine.RetryRegistration(adobeTestConfig(), parent.ID); err != nil {
		t.Fatal(err)
	}
	waitAdobe(t, engine)
	failed := latestAdobe(t, engine, parent.ID)
	if failed.Status != "import_pending" || len(failed.Account) == 0 || failed.Imported || failed.Verified || failed.Error != "import: account_write_failed" || verifies.Load() != 0 {
		t.Fatal("transient import failure lost the captured account")
	}
	engine = NewAdobeEngine(path, "fixture", factory, importer, verifier)
	// The saved result no longer depends on the current HME/browser config.
	if ran, err := engine.RunCompensation(map[string]any{"target": "adobe"}, now.Add(24*time.Hour)); !ran || err != nil {
		t.Fatalf("saved account could not resume after restart: %v", err)
	}
	waitAdobe(t, engine)
	completed := latestAdobe(t, engine, parent.ID)
	if completed.Status != "completed" || !completed.Imported || !completed.Verified || len(completed.Account) != 0 || browserCalls.Load() != 1 || imports.Load() != 2 || verifies.Load() != 1 {
		t.Fatal("saved-result recovery reopened the browser or did not finish intake")
	}
}
