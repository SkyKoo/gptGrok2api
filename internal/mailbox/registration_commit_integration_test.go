package mailbox

import (
	"context"
	"errors"
	"testing"
)

func TestAdobeRegistrationCommitReacquiresWithoutRevivingReleasedClaim(t *testing.T) {
	s := isolatedMailboxStore(t)
	ctx := context.Background()
	box := seedMailbox(t, s, "adobe-saved")
	if err := s.EnsureRegistrationClaim(ctx, "adobe", "root", box); err != nil {
		t.Fatal(err)
	}
	var oldID string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM mailbox_claims WHERE mailbox_id=$1 AND platform='adobe'`, box.ID).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "adobe", "root", box, "registration_failed_without_account"); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRegistration(ctx, "adobe", "root", "adobe:account", box); !errors.Is(err, ErrMailboxInUse) {
		t.Fatal("late commit revived released claim", err)
	}
	if err := s.PrepareRegistrationClaim(ctx, "adobe", "root", "adobe:account", box); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRegistration(ctx, "adobe", "root", "adobe:account", box); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitRegistration(ctx, "adobe", "root", "adobe:account", box); err != nil {
		t.Fatal(err)
	}
	var released, active, committed int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_claims WHERE id=$1 AND status='released' AND released_at IS NOT NULL`, oldID).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_claims WHERE mailbox_id=$1 AND platform='adobe' AND id<>$2 AND status='active' AND released_at IS NULL AND registration_job_id='root' AND account_id='adobe:account'`, box.ID, oldID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_events WHERE mailbox_id=$1 AND platform='adobe' AND event_type='claim_committed'`, box.ID).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if released != 1 || active != 1 || committed != 1 {
		t.Fatal(released, active, committed)
	}
}

func TestAdobeRegistrationCommitCannotStealOtherRootOrAccount(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "reserved", true: "active"}[active], func(t *testing.T) {
			s := isolatedMailboxStore(t)
			ctx := context.Background()
			box := seedMailbox(t, s, "adobe-owner")
			if err := s.EnsureRegistrationClaim(ctx, "adobe", "old-root", box); err != nil {
				t.Fatal(err)
			}
			if err := s.Release(ctx, "adobe", "old-root", box, "failed"); err != nil {
				t.Fatal(err)
			}
			if err := s.EnsureRegistrationClaim(ctx, "adobe", "new-root", box); err != nil {
				t.Fatal(err)
			}
			if active {
				if err := s.CommitRegistration(ctx, "adobe", "new-root", "adobe:new", box); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.CommitRegistration(ctx, "adobe", "old-root", "adobe:old", box); !errors.Is(err, ErrMailboxInUse) {
				t.Fatal("stole new owner", err)
			}
			if active {
				if err := s.CommitRegistration(ctx, "adobe", "new-root", "adobe:other", box); !errors.Is(err, ErrMailboxInUse) {
					t.Fatal("changed account owner", err)
				}
			}
			var oldActive int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_claims WHERE mailbox_id=$1 AND registration_job_id='old-root' AND status IN ('reserved','active')`, box.ID).Scan(&oldActive); err != nil || oldActive != 0 {
				t.Fatal(oldActive, err)
			}
		})
	}
}

func TestAdobeRegistrationClaimRejectsStaleAssetSourceWithoutRewriting(t *testing.T) {
	s := isolatedMailboxStore(t)
	ctx := context.Background()
	box := seedMailbox(t, s, "adobe-source-check")
	if err := s.EnsureRegistrationClaim(ctx, "adobe", "root", box); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE mailboxes SET source_mailbox_id='new-upstream-id' WHERE id=$1`, box.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareRegistrationClaim(ctx, "adobe", "root", "", box); !errors.Is(err, ErrMailboxInUse) {
		t.Fatal("stale source prepared", err)
	}
	if err := s.CommitRegistration(ctx, "adobe", "root", "adobe:account", box); !errors.Is(err, ErrMailboxInUse) {
		t.Fatal("stale source committed", err)
	}
	var current string
	if err := s.db.QueryRowContext(ctx, `SELECT source_mailbox_id FROM mailboxes WHERE id=$1`, box.ID).Scan(&current); err != nil || current != "new-upstream-id" {
		t.Fatal(current, err)
	}
}
