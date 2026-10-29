package mailbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const mailboxColumns = `id,source_id,source_type,source_account_id,source_mailbox_id,email,status,last_error,created_at,updated_at`

func scanMailbox(row interface{ Scan(...any) error }) (Mailbox, error) {
	var box Mailbox
	err := row.Scan(&box.ID, &box.SourceID, &box.SourceType, &box.SourceAccountID, &box.SourceMailboxID, &box.Email, &box.Status, &box.LastError, &box.CreatedAt, &box.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, ErrMailboxNotFound
	}
	return box, err
}

func (s *Store) Get(ctx context.Context, id string) (Mailbox, error) {
	return scanMailbox(s.db.QueryRowContext(ctx, `SELECT `+mailboxColumns+` FROM mailboxes WHERE id=$1`, strings.TrimSpace(id)))
}

// prepareDelete persists the allocation barrier before making a remote request.
// If the process or request dies later, cleanup_pending survives and prevents
// registration from acquiring an alias whose upstream result is unknown.
func (s *Store) prepareDelete(ctx context.Context, id, actor string) (Mailbox, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mailbox{}, err
	}
	defer tx.Rollback()
	box, err := scanMailbox(tx.QueryRowContext(ctx, `SELECT `+mailboxColumns+` FROM mailboxes WHERE id=$1 FOR UPDATE NOWAIT`, id))
	if err != nil {
		return Mailbox{}, mailboxLockError(err)
	}
	if box.Status == "deleted" {
		return box, nil
	}
	if actor == "account_cleanup" {
		var blocked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mailbox_registration_blocks WHERE mailbox_id=$1)`, id).Scan(&blocked); err != nil {
			return box, err
		}
		if box.Status == "disabled" || blocked {
			return box, ErrMailboxRegistrationDisabled
		}
	}
	var occupied bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mailbox_claims WHERE mailbox_id=$1 AND status IN ('reserved','active'))`, id).Scan(&occupied); err != nil {
		return box, err
	}
	if occupied {
		return box, ErrMailboxInUse
	}
	box, err = scanMailbox(tx.QueryRowContext(ctx, `UPDATE mailboxes SET status='cleanup_pending',updated_at=now() WHERE id=$1 RETURNING `+mailboxColumns, id))
	if err != nil {
		return box, err
	}
	if err := s.writeEventTx(ctx, tx, Event{MailboxID: id, SourceType: box.SourceType, EventType: "mailbox_delete_requested", Actor: actor, Result: "cleanup_pending"}); err != nil {
		return box, err
	}
	return box, tx.Commit()
}

func mailboxLockError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return ErrMailboxBusy
	}
	return err
}

// Delete serializes provider deletion against registration/reconciliation with
// an asset row lock. A retry re-reads current state and never deletes an already
// deleted alias twice. Soft deletion retains claims and management history.
func (s *Store) Delete(ctx context.Context, id, actor string, remove func(context.Context, Mailbox) error) (Mailbox, error) {
	box, err := s.prepareDelete(ctx, strings.TrimSpace(id), actor)
	if err != nil || box.Status == "deleted" {
		return box, err
	}
	// The final database write must outlive a cancelled HTTP request. The provider
	// still uses ctx, while this bounded transaction can record its failure.
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(dbCtx, nil)
	if err != nil {
		return box, err
	}
	defer tx.Rollback()
	box, err = scanMailbox(tx.QueryRowContext(dbCtx, `SELECT `+mailboxColumns+` FROM mailboxes WHERE id=$1 FOR UPDATE NOWAIT`, id))
	if err != nil {
		return box, mailboxLockError(err)
	}
	if box.Status == "deleted" {
		return box, nil
	}
	var occupied bool
	if err := tx.QueryRowContext(dbCtx, `SELECT EXISTS(SELECT 1 FROM mailbox_claims WHERE mailbox_id=$1 AND status IN ('reserved','active'))`, id).Scan(&occupied); err != nil {
		return box, err
	}
	if occupied {
		return box, ErrMailboxInUse
	}
	providerErr := remove(ctx, box)
	status, eventType, message := "deleted", "provider_cleanup_succeeded", ""
	if providerErr != nil && !errors.Is(providerErr, ErrProviderMailboxNotFound) {
		status, eventType, message = "cleanup_pending", "provider_cleanup_failed", providerErr.Error()
	}
	box, err = scanMailbox(tx.QueryRowContext(dbCtx, `UPDATE mailboxes SET status=$1,last_error=$2,updated_at=now() WHERE id=$3 RETURNING `+mailboxColumns, status, message, id))
	if err != nil {
		return box, err
	}
	if err := s.writeEventTx(dbCtx, tx, Event{MailboxID: id, SourceType: box.SourceType, EventType: eventType, Actor: actor, Result: status, Error: message}); err != nil {
		return box, err
	}
	if err := tx.Commit(); err != nil {
		return box, err
	}
	if status == "cleanup_pending" {
		return box, fmt.Errorf("上游邮箱删除未完成，资产已保留，可重试：%w", providerErr)
	}
	return box, nil
}
