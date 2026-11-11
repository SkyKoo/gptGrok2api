package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/adobe"
	mailboxstore "github.com/auucoder/gptgrok2api-go/internal/mailbox"
)

func callAdobeMailboxLink(t *testing.T, s *Server, ids ...string) adobeMailboxLinkSummary {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"ids": ids})
	rec := adminRequest(s.Handler(), http.MethodPost, "/api/adobe/accounts/link-mailboxes", strings.NewReader(string(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("link status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result adobeMailboxLinkSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "sherlockToken") || strings.Contains(rec.Body.String(), "signature") {
		t.Fatal("link response exposed credentials")
	}
	return result
}

func TestAdobeMailboxLinkRequiresAdminAndDatabase(t *testing.T) {
	s := imageTaskTestServer(t)
	rec := imageTaskCall(s, "POST", "/api/adobe/accounts/link-mailboxes", "api-secret", `{"ids":["adobe:any"]}`)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin could associate assets: %d", rec.Code)
	}
	rec = imageTaskCall(s, "POST", "/api/adobe/accounts/link-mailboxes", "admin-secret", `{"ids":["adobe:any"]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing database status=%d", rec.Code)
	}
}

func TestAdobeMailboxLinkMatchesExistingAssetAndDeletion(t *testing.T) {
	s, db := mailboxHTTPTestServer(t)
	ctx := context.Background()
	box := seedHTTPMailbox(t, s, "fixture")
	_, err := s.mailboxStore.UpsertSource(ctx, mailboxstore.Source{ID: "fixture", Type: "icloud_hme", Enabled: true, Config: map[string]any{"delete_hme_mailbox_on_account_delete": false}})
	if err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"openai", "grok"} {
		claimHTTPMailbox(t, s, box, platform, platform+"-account")
	}
	if _, err := s.mailboxStore.SetPlatformStatus(ctx, box.ID, "adobe", "disabled"); err != nil {
		t.Fatal(err)
	}
	a := adobeFixtureAccount(t, s)
	a.Email = "  FIXTURE@Example.Test  "
	if _, err := s.adobeStore.Import([]adobe.Account{a}); err != nil {
		t.Fatal(err)
	}
	if err := s.adobeStore.SetEnabled([]string{a.ID}, false); err != nil {
		t.Fatal(err)
	}
	before, err := s.adobeStore.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := callAdobeMailboxLink(t, s, a.ID, " "+a.ID+" ")
	if result.Linked != 1 || len(result.Results) != 1 {
		t.Fatalf("result=%+v", result)
	}
	after, err := s.adobeStore.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	metadata := after.RegistrationMailbox
	if stringValue(metadata["id"]) != box.ID || stringValue(metadata["email"]) != box.Email || stringValue(metadata["source_id"]) != box.SourceID || stringValue(metadata["account_id"]) != box.SourceAccountID || stringValue(metadata["source_mailbox_id"]) != box.SourceMailboxID || metadata["platform"] != "adobe" || metadata["provider"] != "icloud_hme" {
		t.Fatalf("metadata=%v", metadata)
	}
	after.RegistrationMailbox, after.UpdatedAt = before.RegistrationMailbox, before.UpdatedAt
	if !reflect.DeepEqual(before, after) {
		t.Fatal("link changed account state beyond metadata/timestamp")
	}
	view := mailboxView(t, s, box.ID)
	if !view.AdobeClaimed || view.AdobeReserved || !view.OpenAIClaimed || !view.GrokClaimed || !view.AdobeDisabled {
		t.Fatalf("view=%+v", view)
	}
	var owner string
	if err := db.QueryRow(`SELECT account_id FROM mailbox_claims WHERE platform='adobe' AND status='active'`).Scan(&owner); err != nil || owner != a.ID {
		t.Fatalf("owner=%s err=%v", owner, err)
	}
	result = callAdobeMailboxLink(t, s, a.ID)
	if result.AlreadyLinked != 1 || result.Linked != 0 {
		t.Fatalf("repeat=%+v", result)
	}
	var assets, events int
	if err := db.QueryRow(`SELECT count(*) FROM mailboxes`).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM mailbox_events WHERE event_type='account_mailbox_linked'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if assets != 1 || events != 1 {
		t.Fatalf("assets=%d link events=%d", assets, events)
	}
	raw, _ := json.Marshal(map[string]any{"ids": []string{a.ID}})
	rec := adminRequest(s.Handler(), http.MethodDelete, "/api/adobe/accounts", strings.NewReader(string(raw)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"released":1`) {
		t.Fatalf("delete=%d %s", rec.Code, rec.Body.String())
	}
	view = mailboxView(t, s, box.ID)
	if view.AdobeClaimed || !view.AdobeRetired || !view.GrokClaimed || !view.OpenAIClaimed {
		t.Fatalf("delete view=%+v", view)
	}
}

func TestAdobeMailboxLinkRejectsConflictsAndNeverCreatesAssets(t *testing.T) {
	for _, mode := range []string{"unmatched", "active_other", "reserved_other", "cleanup_pending", "deleted", "metadata_email", "metadata_source", "unsupported_source"} {
		t.Run(mode, func(t *testing.T) {
			s, db := mailboxHTTPTestServer(t)
			ctx := context.Background()
			a := adobeFixtureAccount(t, s)
			var box mailboxstore.Mailbox
			if mode != "unmatched" {
				box = seedHTTPMailbox(t, s, "fixture")
			}
			switch mode {
			case "active_other":
				claimHTTPMailbox(t, s, box, "adobe", "adobe:other")
			case "reserved_other":
				if err := s.mailboxStore.EnsureRegistrationClaim(ctx, "adobe", "another-job", box); err != nil {
					t.Fatal(err)
				}
			case "cleanup_pending", "deleted":
				if _, err := db.Exec(`UPDATE mailboxes SET status=$1 WHERE id=$2`, mode, box.ID); err != nil {
					t.Fatal(err)
				}
			case "metadata_email":
				a.RegistrationMailbox = map[string]any{"email": "different@example.test"}
			case "metadata_source":
				a.RegistrationMailbox = map[string]any{"source_id": "different"}
			case "unsupported_source":
				if _, err := db.Exec(`UPDATE mailboxes SET source_type='custom' WHERE id=$1`, box.ID); err != nil {
					t.Fatal(err)
				}
			}
			if a.RegistrationMailbox != nil {
				if _, err := s.adobeStore.Import([]adobe.Account{a}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.adobeStore.Get(a.ID)
			result := callAdobeMailboxLink(t, s, a.ID)
			if mode == "unmatched" {
				if result.Unmatched != 1 {
					t.Fatalf("unmatched=%+v", result)
				}
			} else if result.Conflicts != 1 {
				t.Fatalf("conflict=%+v", result)
			}
			after, _ := s.adobeStore.Get(a.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected link changed account")
			}
			var assets, ownClaims int
			if err := db.QueryRow(`SELECT count(*) FROM mailboxes`).Scan(&assets); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM mailbox_claims WHERE platform='adobe' AND account_id=$1`, a.ID).Scan(&ownClaims); err != nil {
				t.Fatal(err)
			}
			wantAssets := 1
			if mode == "unmatched" {
				wantAssets = 0
			}
			if assets != wantAssets || ownClaims != 0 {
				t.Fatalf("assets=%d claims=%d", assets, ownClaims)
			}
		})
	}
}

func TestAdobeMailboxLinkOwnReservationAndExplicitSelection(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	box := seedHTTPMailbox(t, s, "fixture")
	a := adobeFixtureAccount(t, s)
	a.RegistrationJobID = "own-registration"
	a.RegistrationMailbox = map[string]any{"provider": "hme", "id": box.SourceMailboxID, "email": box.Email}
	if _, err := s.adobeStore.Import([]adobe.Account{a}); err != nil {
		t.Fatal(err)
	}
	if err := s.mailboxStore.EnsureRegistrationClaim(context.Background(), "adobe", a.RegistrationJobID, box); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"ids":[]}`, `{"ids":[" "]}`} {
		rec := adminRequest(s.Handler(), "POST", "/api/adobe/accounts/link-mailboxes", strings.NewReader(body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("empty selection=%d", rec.Code)
		}
	}
	if !mailboxView(t, s, box.ID).AdobeReserved {
		t.Fatal("empty selection mutated reservation")
	}
	result := callAdobeMailboxLink(t, s, a.ID, "adobe:missing")
	if result.Linked != 1 || result.Failed != 1 {
		t.Fatalf("partial batch=%+v", result)
	}
	if !mailboxView(t, s, box.ID).AdobeClaimed {
		t.Fatal("own reservation not completed")
	}
}

func TestAdobeMailboxLinkSaveFailureRetainsGuardAndRetryRepairs(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	box := seedHTTPMailbox(t, s, "fixture")
	a := adobeFixtureAccount(t, s)
	before, _ := s.adobeStore.Get(a.ID)
	path := filepath.Join(s.cfg.DataDir, "adobe_accounts.json.enc")
	backup := path + ".test-backup"
	_, err := s.adobeStore.LinkMailbox(a.ID, func(current adobe.Account) (map[string]any, bool, error) {
		_, claimed, err := s.mailboxStore.LinkExistingAccount(context.Background(), "adobe", current.ID, "", current.Email, nil)
		if err != nil {
			return nil, false, err
		}
		if err := os.Rename(path, backup); err != nil {
			return nil, false, err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			return nil, false, err
		}
		return map[string]any{"id": box.ID, "email": box.Email}, claimed, nil
	})
	if err == nil {
		t.Fatal("expected encrypted store write failure")
	}
	if !mailboxView(t, s, box.ID).AdobeClaimed {
		t.Fatal("confirmed guard was released after save failure")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	after, err := s.adobeStore.Get(a.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed save changed original account")
	}
	result := callAdobeMailboxLink(t, s, a.ID)
	if result.Linked != 1 {
		t.Fatalf("repair=%+v", result)
	}
	after, _ = s.adobeStore.Get(a.ID)
	if after.RegistrationMailbox["id"] != box.ID || after.Token != before.Token || after.Cookie != before.Cookie || after.Enabled != before.Enabled || after.Revision != before.Revision {
		t.Fatal("repair failed or changed credentials/state")
	}
	if result := callAdobeMailboxLink(t, s, a.ID); result.AlreadyLinked != 1 {
		t.Fatalf("post-repair repeat=%+v", result)
	}
	if _, err := s.mailboxStore.Delete(context.Background(), box.ID, "admin", func(context.Context, mailboxstore.Mailbox) error {
		t.Fatal("linked asset reached provider deletion")
		return nil
	}); !errors.Is(err, mailboxstore.ErrMailboxInUse) {
		t.Fatalf("delete guard=%v", err)
	}
}

func TestAdobeMailboxRelinkSurvivesEarlierDeletionCleanup(t *testing.T) {
	s, _ := mailboxHTTPTestServer(t)
	box := seedHTTPMailbox(t, s, "fixture")
	a := adobeFixtureAccount(t, s)
	if result := callAdobeMailboxLink(t, s, a.ID); result.Linked != 1 {
		t.Fatalf("link=%+v", result)
	}
	deleted, cutoff, err := s.adobeStore.DeleteWithCutoff([]string{a.ID})
	if err != nil || len(deleted) != 1 {
		t.Fatalf("delete=%v count=%d", err, len(deleted))
	}
	if _, err := s.adobeStore.Import(deleted); err != nil {
		t.Fatal(err)
	}
	if result := callAdobeMailboxLink(t, s, a.ID); result.AlreadyLinked != 1 {
		t.Fatalf("relink=%+v", result)
	}
	rows := []map[string]any{{"id": a.ID, "account_id": a.AccountID, "email": a.Email, "platform": "adobe", "registration_mailbox": deleted[0].RegistrationMailbox}}
	summary := s.cleanupHMEMailboxes(context.Background(), rows, cutoff)
	if summary.Released != 0 || summary.Deleted != 0 || summary.Failed != 0 {
		t.Fatalf("old cleanup=%+v", summary)
	}
	if !mailboxView(t, s, box.ID).AdobeClaimed {
		t.Fatal("old deletion released the reimported account")
	}
	if _, err := s.adobeStore.Get(a.ID); err != nil {
		t.Fatal(err)
	}
}
