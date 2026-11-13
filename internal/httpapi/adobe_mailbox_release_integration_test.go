package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
)

func writeAdobeMailboxHistory(t *testing.T, s *Server, jobs ...registerruntime.RegistrationJob) {
	t.Helper()
	dir := s.cfg.DataDir
	if s.cfg.RegisterPath != "" {
		dir = filepath.Dir(s.cfg.RegisterPath)
	}
	raw, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := adobe.Seal(firstNonEmpty(s.cfg.AdminKey, s.cfg.APIKey, "gptgrok2api"), raw)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(map[string]any{"version": 1, "data": sealed})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "adobe_registration_tasks.enc.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAdobeStartupReleasesUnusedAcceptedReservationWithAudit(t *testing.T) {
	s, db := mailboxHTTPTestServer(t)
	ctx := context.Background()
	box := seedHTTPMailbox(t, s, "adobe-failed-history")
	claimHTTPMailbox(t, s, box, "openai", "gpt-owner")
	claimHTTPMailbox(t, s, box, "grok", "grok-owner")
	if err := s.mailboxStore.EnsureRegistrationClaim(ctx, "adobe", "root", box); err != nil {
		t.Fatal(err)
	}
	job := registerruntime.RegistrationJob{ID: "root", Status: "registration_pending", Stage: "adobe_media_pending", Error: "adobe: media_session_incomplete", Mailbox: registerMailbox(box), AdobeRegistration: &registerruntime.AdobeRegistrationState{CreationOutcome: "accepted"}, AutoRetry: &registerruntime.RegistrationCompensation{State: "exhausted", Attempts: 3}}
	writeAdobeMailboxHistory(t, s, job)
	s.initAdobeRegistration()
	v := mailboxView(t, s, box.ID)
	if v.AdobeReserved || v.AdobeClaimed || !v.OpenAIClaimed || !v.GrokClaimed || v.AdobeRetired {
		t.Fatal(v)
	}
	s.initAdobeRegistration()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM mailbox_events WHERE mailbox_id=$1 AND platform='adobe' AND event_type='claim_released'`, box.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestAdobeStartupProtectsExternalImportedAccountAndActiveOwner(t *testing.T) {
	for _, kind := range []string{"external_import", "active_claim", "different_root", "saved_result"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := mailboxHTTPTestServer(t)
			ctx := context.Background()
			box := seedHTTPMailbox(t, s, "adobe-protected-"+kind)
			owner := "root"
			if kind == "different_root" {
				owner = "other-root"
			}
			if err := s.mailboxStore.EnsureRegistrationClaim(ctx, "adobe", owner, box); err != nil {
				t.Fatal(err)
			}
			if kind == "active_claim" {
				if err := s.mailboxStore.CommitRegistration(ctx, "adobe", owner, "adobe:saved", box); err != nil {
					t.Fatal(err)
				}
			}
			job := registerruntime.RegistrationJob{ID: "root", Status: "registration_pending", Mailbox: registerMailbox(box)}
			if kind == "saved_result" {
				job.Account = map[string]any{"account_id": "saved"}
			}
			if kind == "external_import" {
				account := adobeFixtureAccount(t, s)
				account.Email = box.Email
				account.RegistrationJobID = ""
				account.RegistrationMailbox = nil
				if _, err := s.adobeStore.Import([]adobe.Account{account}); err != nil {
					t.Fatal(err)
				}
			}
			writeAdobeMailboxHistory(t, s, job)
			s.initAdobeRegistration()
			v := mailboxView(t, s, box.ID)
			if !v.AdobeReserved && !v.AdobeClaimed {
				t.Fatal("released protected owner", v)
			}
		})
	}
}

func TestAdobeSavedImportReacquiresOnlyItsOriginalMailboxOwner(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "other_root"}[occupied], func(t *testing.T) {
			s, db := mailboxHTTPTestServer(t)
			ctx := context.Background()
			box := seedHTTPMailbox(t, s, "adobe-saved-import")
			account := adobeFixtureAccount(t, s)
			if _, err := s.adobeStore.Delete([]string{account.ID}); err != nil {
				t.Fatal(err)
			}
			account.Email = box.Email
			account.RegistrationJobID = "old-root"
			account.RegistrationMailbox = adobeReconcileMetadata(box)
			if err := s.mailboxStore.EnsureRegistrationClaim(ctx, "adobe", "old-root", box); err != nil {
				t.Fatal(err)
			}
			if err := s.mailboxStore.Release(ctx, "adobe", "old-root", box, "failed"); err != nil {
				t.Fatal(err)
			}
			if occupied {
				if err := s.mailboxStore.EnsureRegistrationClaim(ctx, "adobe", "new-root", box); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(account)
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			err := s.importRegisteredAdobeAccount(ctx, "old-root", fields)
			if occupied {
				if err == nil {
					t.Fatal("saved import stole other task")
				}
				if rows, e := s.adobeStore.List(); e != nil || len(rows) != 0 {
					t.Fatal("conflicting account imported", e)
				}
				var owner string
				if e := db.QueryRow(`SELECT registration_job_id FROM mailbox_claims WHERE mailbox_id=$1 AND platform='adobe' AND status='reserved'`, box.ID).Scan(&owner); e != nil || owner != "new-root" {
					t.Fatal(owner, e)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				stored, e := s.adobeStore.Get(account.ID)
				if e != nil || stored.RegistrationJobID != "old-root" {
					t.Fatal(e)
				}
				before := stored.Revision
				if err = s.importRegisteredAdobeAccount(ctx, "old-root", fields); err != nil {
					t.Fatal(err)
				}
				stored, e = s.adobeStore.Get(account.ID)
				if e != nil || stored.Revision != before {
					t.Fatal("idempotent import replaced credentials", e)
				}
				var active int
				if e := db.QueryRow(`SELECT count(*) FROM mailbox_claims WHERE mailbox_id=$1 AND platform='adobe' AND status='active' AND registration_job_id='old-root' AND account_id=$2 AND released_at IS NULL`, box.ID, account.ID).Scan(&active); e != nil || active != 1 {
					t.Fatal(active, e)
				}
			}
		})
	}
}
