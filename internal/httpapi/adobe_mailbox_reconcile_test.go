package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
)

func adobeReconcileMetadata(box mailboxstore.Mailbox) map[string]any {
	return map[string]any{"provider": "icloud_hme", "platform": "adobe", "email": box.Email, "source_id": box.SourceID, "source_type": box.SourceType, "id": box.ID, "source_mailbox_id": box.SourceMailboxID, "account_id": box.SourceAccountID}
}
func reconcileAdobeFixture(t *testing.T, s *Server) (int, []string) {
	t.Helper()
	rec := adminRequest(s.Handler(), http.MethodPost, "/api/icloud/reconcile", strings.NewReader(`{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile=%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Claims int      `json:"adobe_claims"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Claims, out.Errors
}

func TestAdobeReconcileNeverStealsOtherAccountOrTask(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		name := "active_owner"
		if reserved {
			name = "reserved_other_job"
		}
		t.Run(name, func(t *testing.T) {
			s, db := mailboxHTTPTestServer(t)
			box := seedHTTPMailbox(t, s, "fixture")
			a := adobeFixtureAccount(t, s)
			a.RegistrationJobID = "official-current-job"
			a.RegistrationMailbox = adobeReconcileMetadata(box)
			if _, err := s.adobeStore.Import([]adobe.Account{a}); err != nil {
				t.Fatal(err)
			}
			if reserved {
				if err := s.mailboxStore.EnsureRegistrationClaim(context.Background(), "adobe", "other-job", box); err != nil {
					t.Fatal(err)
				}
			} else {
				claimHTTPMailbox(t, s, box, "adobe", "adobe:other-account")
			}
			claims, diagnostics := reconcileAdobeFixture(t, s)
			if claims != 0 || !strings.Contains(strings.Join(diagnostics, " "), "其他账号或注册任务占用") {
				t.Fatalf("claims=%d errors=%v", claims, diagnostics)
			}
			var owner, job, status string
			if err := db.QueryRow(`SELECT account_id,registration_job_id,status FROM mailbox_claims WHERE mailbox_id=$1 AND platform='adobe' AND status IN ('active','reserved')`, box.ID).Scan(&owner, &job, &status); err != nil {
				t.Fatal(err)
			}
			if reserved && (status != "reserved" || job != "other-job" || owner != "") || !reserved && (status != "active" || owner != "adobe:other-account") {
				t.Fatalf("ownership stolen: owner=%s job=%s status=%s", owner, job, status)
			}
		})
	}
}

func TestAdobeReconcileDoesNotOverwriteOrInventAssets(t *testing.T) {
	for _, mode := range []string{"conflicting_source", "missing_asset", "plain_import"} {
		t.Run(mode, func(t *testing.T) {
			s, db := mailboxHTTPTestServer(t)
			box := mailboxstore.Mailbox{ID: "missing", Email: "fixture@example.test", SourceID: "fixture", SourceType: "hme", SourceAccountID: "hme-account", SourceMailboxID: "alias-fixture"}
			if mode != "missing_asset" {
				box = seedHTTPMailbox(t, s, "fixture")
			}
			a := adobeFixtureAccount(t, s)
			if mode != "plain_import" {
				a.RegistrationMailbox = adobeReconcileMetadata(box)
				if mode == "conflicting_source" {
					a.RegistrationMailbox["source_id"] = "wrong-source"
					a.RegistrationMailbox["source_mailbox_id"] = "wrong-alias"
				}
				if _, err := s.adobeStore.Import([]adobe.Account{a}); err != nil {
					t.Fatal(err)
				}
			}
			claims, diagnostics := reconcileAdobeFixture(t, s)
			if claims != 0 {
				t.Fatalf("claims=%d", claims)
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM mailboxes`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if mode == "missing_asset" {
				if count != 0 || !strings.Contains(strings.Join(diagnostics, " "), "资产未匹配") {
					t.Fatalf("count=%d errors=%v", count, diagnostics)
				}
			} else {
				fresh, err := s.mailboxStore.Get(context.Background(), box.ID)
				if err != nil || fresh.SourceID != box.SourceID || fresh.SourceMailboxID != box.SourceMailboxID {
					t.Fatal("imported metadata rewrote asset source")
				}
				if mailboxView(t, s, box.ID).AdobeClaimed {
					t.Fatal("unexpected Adobe ownership")
				}
			}
		})
	}
}

func TestAdobeReconcilePreservesOfficialJobAndIgnoresDeletedSnapshot(t *testing.T) {
	s, db := mailboxHTTPTestServer(t)
	box := seedHTTPMailbox(t, s, "fixture")
	a := adobeFixtureAccount(t, s)
	a.RegistrationJobID = "official-job"
	a.RegistrationMailbox = adobeReconcileMetadata(box)
	if _, err := s.adobeStore.Import([]adobe.Account{a}); err != nil {
		t.Fatal(err)
	}
	if err := s.mailboxStore.EnsureRegistrationClaim(context.Background(), "adobe", a.RegistrationJobID, box); err != nil {
		t.Fatal(err)
	}
	claims, _ := reconcileAdobeFixture(t, s)
	if claims != 1 {
		t.Fatalf("claims=%d", claims)
	}
	var owner, job string
	if err := db.QueryRow(`SELECT account_id,registration_job_id FROM mailbox_claims WHERE mailbox_id=$1 AND platform='adobe' AND status='active'`, box.ID).Scan(&owner, &job); err != nil {
		t.Fatal(err)
	}
	if owner != a.ID || job != a.RegistrationJobID {
		t.Fatalf("owner=%s job=%s", owner, job)
	}
	saved, _ := s.adobeStore.Get(a.ID)
	if saved.RegistrationJobID != a.RegistrationJobID || saved.RegistrationMailbox["source_mailbox_id"] != box.SourceMailboxID {
		t.Fatal("official metadata changed")
	}
	snapshot, _ := s.adobeStore.List()
	deleted, cutoff, err := s.adobeStore.DeleteWithCutoff([]string{a.ID})
	if err != nil || len(deleted) != 1 {
		t.Fatal("delete failed")
	}
	if _, err = s.mailboxStore.ReleaseForAccount(context.Background(), "adobe", a.ID, a.Email, cutoff); err != nil {
		t.Fatal(err)
	}
	// Reconciliation passes only the old ID, then re-reads under the account
	// lock. A stale list can never recreate an already deleted account claim.
	if _, err = s.linkAdobeMailbox(context.Background(), snapshot[0].ID, true); !errors.Is(err, adobe.ErrNotFound) {
		t.Fatalf("stale snapshot=%v", err)
	}
	if mailboxView(t, s, box.ID).AdobeClaimed {
		t.Fatal("deleted account claim resurrected")
	}
}

func TestAdobeReconcileKeepsManualOwnerWhenDuplicateEmailImported(t *testing.T) {
	s, db := mailboxHTTPTestServer(t)
	box := seedHTTPMailbox(t, s, "fixture")
	original := adobeFixtureAccount(t, s)
	if result := callAdobeMailboxLink(t, s, original.ID); result.Linked != 1 {
		t.Fatal("initial link failed")
	}
	current, _ := s.adobeStore.Get(original.ID)
	duplicate := current
	duplicate.AccountID = "duplicate-adobe"
	claims, _ := json.Marshal(map[string]any{"user_id": duplicate.AccountID, "exp": time.Now().Add(time.Hour).Unix()})
	duplicate.Token = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	if _, err := s.adobeStore.Import([]adobe.Account{duplicate}); err != nil {
		t.Fatal(err)
	}
	count, diagnostics := reconcileAdobeFixture(t, s)
	if count != 1 || !strings.Contains(strings.Join(diagnostics, " "), "其他账号或注册任务占用") {
		t.Fatalf("claims=%d errors=%v", count, diagnostics)
	}
	var owner string
	if err := db.QueryRow(`SELECT account_id FROM mailbox_claims WHERE mailbox_id=$1 AND platform='adobe' AND status='active'`, box.ID).Scan(&owner); err != nil || owner != original.ID {
		t.Fatalf("owner=%s err=%v", owner, err)
	}
}
