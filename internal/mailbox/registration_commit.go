package mailbox

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Registration retries use an existing asset and must not overwrite its source
// metadata from an older journal snapshot. The asset row also serializes claim
// changes with cleanup, account association and other registration roots.
func (s *Store) registrationAsset(ctx context.Context, tx *sql.Tx, wanted Mailbox) (Mailbox, error) {
	var item Mailbox
	err := tx.QueryRowContext(ctx, `SELECT id,email,source_id,source_type,source_account_id,source_mailbox_id,status FROM mailboxes WHERE lower(email)=lower($1) LIMIT 1 FOR UPDATE`, strings.TrimSpace(wanted.Email)).Scan(&item.ID, &item.Email, &item.SourceID, &item.SourceType, &item.SourceAccountID, &item.SourceMailboxID, &item.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrMailboxNotFound
	}
	if err != nil {
		return item, err
	}
	for _, pair := range [][2]string{{wanted.ID, item.ID}, {wanted.SourceID, item.SourceID}, {wanted.SourceType, item.SourceType}, {wanted.SourceAccountID, item.SourceAccountID}, {wanted.SourceMailboxID, item.SourceMailboxID}} {
		if pair[0] != "" && pair[0] != pair[1] {
			return item, ErrMailboxInUse
		}
	}
	if item.Status == "cleanup_pending" || item.Status == "deleted" {
		return item, ErrMailboxStateImmutable
	}
	return item, nil
}

// PrepareRegistrationClaim reacquires only this root's local reservation. An
// existing active claim is accepted solely for an idempotent saved-account
// import. It performs no alias creation, upstream preparation or verification.
func (s *Store) PrepareRegistrationClaim(ctx context.Context, platform, jobID, accountID string, wanted Mailbox) error {
	if !s.Enabled() {
		return nil
	}
	platform, jobID, accountID = strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(jobID), strings.TrimSpace(accountID)
	if platform == "" || jobID == "" || strings.TrimSpace(wanted.Email) == "" {
		return errors.New("mailbox registration owner is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	item, err := s.registrationAsset(ctx, tx, wanted)
	if err != nil {
		return err
	}
	var claimID, root, owner, status string
	err = tx.QueryRowContext(ctx, `SELECT id,registration_job_id,account_id,status FROM mailbox_claims WHERE mailbox_id=$1 AND platform=$2 AND status IN ('reserved','active') ORDER BY updated_at DESC LIMIT 1`, item.ID, platform).Scan(&claimID, &root, &owner, &status)
	if err == nil {
		if root != jobID || owner != "" && owner != accountID || status == "active" && (accountID == "" || owner != accountID) {
			return ErrMailboxInUse
		}
		if status == "reserved" {
			if err = s.checkRegistrationAllowed(ctx, tx, item.ID, platform); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = s.checkRegistrationAllowed(ctx, tx, item.ID, platform); err != nil {
		return err
	}
	claimID = newID("clm")
	if _, err = tx.ExecContext(ctx, `INSERT INTO mailbox_claims(id,mailbox_id,platform,registration_job_id,status) VALUES($1,$2,$3,$4,'reserved')`, claimID, item.ID, platform, jobID); err != nil {
		return err
	}
	if err = s.writeEventTx(ctx, tx, Event{MailboxID: item.ID, ClaimID: claimID, EventType: "claim_adopted", SourceType: item.SourceType, Platform: platform, RegistrationJob: jobID, Result: "adopted"}); err != nil {
		return err
	}
	return tx.Commit()
}

// CommitRegistration never adopts another root's reservation/account or
// revives a released claim. The caller must explicitly prepare its reservation;
// a late commit after release is rejected even when the asset is still idle.
func (s *Store) CommitRegistration(ctx context.Context, platform, jobID, accountID string, wanted Mailbox) error {
	if !s.Enabled() {
		return nil
	}
	platform, jobID, accountID = strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(jobID), strings.TrimSpace(accountID)
	if platform == "" || jobID == "" || accountID == "" || strings.TrimSpace(wanted.Email) == "" {
		return errors.New("mailbox registration owner is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	item, err := s.registrationAsset(ctx, tx, wanted)
	if err != nil {
		return err
	}
	var claimID, root, owner, status string
	err = tx.QueryRowContext(ctx, `SELECT id,registration_job_id,account_id,status FROM mailbox_claims WHERE mailbox_id=$1 AND platform=$2 AND status IN ('reserved','active') ORDER BY updated_at DESC LIMIT 1`, item.ID, platform).Scan(&claimID, &root, &owner, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMailboxInUse
	}
	if err != nil {
		return err
	}
	if root != jobID || owner != "" && owner != accountID || status == "active" && owner != accountID {
		return ErrMailboxInUse
	}
	if status == "active" {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mailbox_claims SET status='active',account_id=$1,released_at=NULL,updated_at=now() WHERE id=$2`, accountID, claimID); err != nil {
		return err
	}
	var finalStatus string
	if err = tx.QueryRowContext(ctx, `UPDATE mailboxes SET status=CASE WHEN status='disabled' THEN 'disabled' ELSE 'active' END,updated_at=now(),last_error='' WHERE id=$1 RETURNING status`, item.ID).Scan(&finalStatus); err != nil {
		return err
	}
	if err = s.writeEventTx(ctx, tx, Event{MailboxID: item.ID, ClaimID: claimID, EventType: "claim_committed", SourceType: item.SourceType, Platform: platform, AccountID: accountID, RegistrationJob: jobID, Result: finalStatus}); err != nil {
		return err
	}
	return tx.Commit()
}
