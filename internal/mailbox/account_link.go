package mailbox

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// LinkExistingAccount attaches an existing account to an existing asset only.
// Unlike historical reconciliation, it never creates assets or takes over a
// different account/task. The asset lock serializes linking with allocation and
// deletion. Registration blocks/retirement remain intact: this operation records
// an existing account, it does not permit another registration.
func (s *Store) LinkExistingAccount(ctx context.Context, platform, accountID, jobID, email string, validate func(Mailbox) error) (Mailbox, bool, error) {
	if !s.Enabled() {
		return Mailbox{}, false, ErrMailboxNotFound
	}
	platform = strings.ToLower(strings.TrimSpace(platform))
	accountID, jobID, email = strings.TrimSpace(accountID), strings.TrimSpace(jobID), strings.TrimSpace(email)
	if platform != "openai" && platform != "grok" && platform != "adobe" {
		return Mailbox{}, false, ErrInvalidMailboxPlatform
	}
	if accountID == "" || email == "" {
		return Mailbox{}, false, ErrMailboxNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mailbox{}, false, err
	}
	defer tx.Rollback()
	box, err := scanMailbox(tx.QueryRowContext(ctx, `SELECT `+mailboxColumns+` FROM mailboxes WHERE lower(email)=lower($1) FOR UPDATE`, email))
	if err != nil {
		return box, false, err
	}
	if box.Status == "cleanup_pending" || box.Status == "deleted" {
		return box, false, ErrMailboxStateImmutable
	}
	if validate != nil {
		if err := validate(box); err != nil {
			return box, false, err
		}
	}
	var elsewhere bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mailbox_claims WHERE platform=$1 AND account_id=$2 AND status='active' AND mailbox_id<>$3)`, platform, accountID, box.ID).Scan(&elsewhere); err != nil {
		return box, false, err
	}
	if elsewhere {
		return box, false, ErrMailboxInUse
	}
	var claimID, owner, claimJob, status string
	err = tx.QueryRowContext(ctx, `SELECT id,account_id,registration_job_id,status FROM mailbox_claims WHERE mailbox_id=$1 AND platform=$2 AND status IN ('reserved','active')`, box.ID, platform).Scan(&claimID, &owner, &claimJob, &status)
	if err == nil {
		if status == "active" {
			if owner != accountID {
				return box, false, ErrMailboxInUse
			}
			// Refresh the guard even for an idempotent link. Cleanup from an
			// earlier account deletion must not release a subsequent re-link.
			if _, err := tx.ExecContext(ctx, `UPDATE mailbox_claims SET updated_at=now() WHERE id=$1`, claimID); err != nil {
				return box, false, err
			}
			return box, false, tx.Commit()
		}
		// Only the account's saved registration job may adopt its reservation.
		if jobID == "" || claimJob != jobID || owner != "" && owner != accountID {
			return box, false, ErrMailboxInUse
		}
		_, err = tx.ExecContext(ctx, `UPDATE mailbox_claims SET status='active',account_id=$1,updated_at=now() WHERE id=$2`, accountID, claimID)
	} else if errors.Is(err, sql.ErrNoRows) {
		claimID = newID("clm")
		if jobID == "" {
			jobID = "manual-link:" + accountID
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_claims(id,mailbox_id,platform,account_id,registration_job_id,status) VALUES ($1,$2,$3,$4,$5,'active')`, claimID, box.ID, platform, accountID, jobID)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return box, false, ErrMailboxInUse
		}
		return box, false, err
	}
	box, err = scanMailbox(tx.QueryRowContext(ctx, `UPDATE mailboxes SET status=CASE WHEN status='disabled' THEN status ELSE 'active' END,updated_at=now() WHERE id=$1 RETURNING `+mailboxColumns, box.ID))
	if err != nil {
		return box, false, err
	}
	if err := s.writeEventTx(ctx, tx, Event{MailboxID: box.ID, ClaimID: claimID, EventType: "account_mailbox_linked", SourceType: box.SourceType, Platform: platform, AccountID: accountID, RegistrationJob: jobID, Actor: "admin", Result: "active"}); err != nil {
		return box, false, err
	}
	if err := tx.Commit(); err != nil {
		// A lost commit response is uncertain. Never release ownership here;
		// repeating the same account link will inspect the committed state.
		return box, false, err
	}
	return box, true, nil
}
