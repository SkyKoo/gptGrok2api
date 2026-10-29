package mailbox

import (
	"context"
	"database/sql"
	"strings"
)

type policyQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Legacy disabled assets block both channels without a destructive migration.
// Policy is independent of upstream sync and account claim lifecycle.
func (s *Store) checkRegistrationAllowed(ctx context.Context, q policyQuerier, id, platform string) error {
	var status string
	var blocked bool
	err := q.QueryRowContext(ctx, `SELECT status,EXISTS(SELECT 1 FROM mailbox_registration_blocks b WHERE b.mailbox_id=m.id AND b.platform=$2) FROM mailboxes m WHERE id=$1`, id, platform).Scan(&status, &blocked)
	if err != nil {
		return err
	}
	if status == "deleted" || status == "cleanup_pending" {
		return ErrMailboxStateImmutable
	}
	if status == "disabled" || blocked {
		return ErrMailboxRegistrationDisabled
	}
	return nil
}

// SetPlatformStatus changes only the selected channel; an empty platform is the
// backwards-compatible all-channel action. The row lock serializes allocation,
// policy updates and deletion. Existing accounts/claims are never removed.
func (s *Store) SetPlatformStatus(ctx context.Context, id, platform, requested string) (Mailbox, error) {
	if !s.Enabled() {
		return Mailbox{}, ErrMailboxNotFound
	}
	id, platform, requested = strings.TrimSpace(id), strings.ToLower(strings.TrimSpace(platform)), strings.ToLower(strings.TrimSpace(requested))
	if platform != "" && platform != "openai" && platform != "grok" {
		return Mailbox{}, ErrInvalidMailboxPlatform
	}
	if requested != "available" && requested != "disabled" {
		return Mailbox{}, ErrInvalidMailboxStatus
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mailbox{}, err
	}
	defer tx.Rollback()
	current, err := scanMailbox(tx.QueryRowContext(ctx, `SELECT `+mailboxColumns+` FROM mailboxes WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return Mailbox{}, err
	}
	if current.Status == "cleanup_pending" || current.Status == "deleted" {
		return Mailbox{}, ErrMailboxStateImmutable
	}
	if current.Status == "disabled" {
		// Preserve the other channel when enabling one channel of a legacy asset.
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_registration_blocks(mailbox_id,platform) VALUES ($1,'openai'),($1,'grok') ON CONFLICT DO NOTHING`, id); err != nil {
			return Mailbox{}, err
		}
	}
	var previous []string
	rows, err := tx.QueryContext(ctx, `SELECT platform FROM mailbox_registration_blocks WHERE mailbox_id=$1 ORDER BY platform`, id)
	if err != nil {
		return Mailbox{}, err
	}
	previous = []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			return Mailbox{}, err
		}
		previous = append(previous, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Mailbox{}, err
	}
	if requested == "disabled" {
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_registration_blocks(mailbox_id,platform) SELECT $1,p FROM (VALUES ('openai'),('grok')) channels(p) WHERE $2='' OR p=$2 ON CONFLICT DO NOTHING`, id, platform)
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM mailbox_registration_blocks WHERE mailbox_id=$1 AND ($2='' OR platform=$2)`, id, platform)
	}
	if err != nil {
		return Mailbox{}, err
	}
	updated, err := scanMailbox(tx.QueryRowContext(ctx, `UPDATE mailboxes SET status=CASE
  WHEN (SELECT count(*) FROM mailbox_registration_blocks WHERE mailbox_id=$1)=2 THEN 'disabled'
  WHEN EXISTS(SELECT 1 FROM mailbox_claims WHERE mailbox_id=$1 AND status IN ('reserved','active')) THEN 'active'
  ELSE 'available' END,updated_at=now() WHERE id=$1 RETURNING `+mailboxColumns, id))
	if err != nil {
		return Mailbox{}, err
	}
	eventType := "mailbox_enabled"
	if requested == "disabled" {
		eventType = "mailbox_disabled"
	}
	if err := s.writeEventTx(ctx, tx, Event{MailboxID: id, EventType: eventType, SourceType: updated.SourceType, Platform: platform, Actor: "admin", Result: updated.Status, Metadata: map[string]any{"previous_status": current.Status, "previous_disabled_platforms": previous, "requested_status": requested, "scope": firstNonEmpty(platform, "all")}}); err != nil {
		return Mailbox{}, err
	}
	return updated, tx.Commit()
}
