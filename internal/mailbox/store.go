package mailbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Mailbox is the provider-neutral representation used by the account and
// registration layers. Source-specific identifiers remain opaque to callers.
type Mailbox struct {
	ID              string
	Email           string
	SourceID        string
	SourceType      string
	SourceAccountID string
	SourceMailboxID string
	Status          string
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Source is a configured upstream mailbox source. Config is kept internal to
// the server; API callers receive a redacted projection.
type Source struct {
	ID           string
	Type         string
	Label        string
	Enabled      bool
	Config       map[string]any
	Capabilities map[string]any
	Status       string
	LastError    string
	LastSyncAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Claim struct {
	ID              string
	MailboxID       string
	Platform        string
	AccountID       string
	RegistrationJob string
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ReleasedAt      *time.Time
}

type MailboxView struct {
	Mailbox
	OpenAIClaimed bool       `json:"openai_claimed"`
	GrokClaimed   bool       `json:"grok_claimed"`
	Claims        []Claim    `json:"claims,omitempty"`
	Capabilities  any        `json:"capabilities,omitempty"`
	Metadata      any        `json:"metadata,omitempty"`
	LastSyncAt    *time.Time `json:"last_sync_at,omitempty"`
}

type Event struct {
	ID              string    `json:"id"`
	MailboxID       string    `json:"mailbox_id"`
	ClaimID         string    `json:"claim_id"`
	EventType       string    `json:"event_type"`
	SourceType      string    `json:"source_type"`
	Platform        string    `json:"platform"`
	AccountID       string    `json:"account_id"`
	RegistrationJob string    `json:"registration_job_id"`
	Actor           string    `json:"actor,omitempty"`
	RequestID       string    `json:"request_id,omitempty"`
	Result          string    `json:"result"`
	Error           string    `json:"error,omitempty"`
	Metadata        any       `json:"metadata,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type Store struct {
	db *sql.DB
}

var (
	ErrMailboxNotFound         = errors.New("mailbox not found")
	ErrInvalidMailboxStatus    = errors.New("mailbox status must be available or disabled")
	ErrMailboxStateImmutable   = errors.New("mailbox is pending cleanup or deleted")
	ErrMailboxInUse            = errors.New("邮箱仍被平台占用，请先删除关联账号或结束注册任务")
	ErrMailboxBusy             = errors.New("邮箱正在执行删除，请稍后刷新状态")
	ErrProviderMailboxNotFound = errors.New("upstream mailbox not found")
)

// MailboxListFilter controls the mailbox asset list shown in the admin UI.
// Platform filters are based on active or reserved claims, so a mailbox can
// be selected independently for GPT and Grok.
type MailboxListFilter struct {
	Search   string
	Status   string
	SourceID string
	Platform string
}

// Open connects to PostgreSQL and applies the mailbox schema. An empty URL
// deliberately disables the module so existing JSON-only installations keep
// starting until the database is configured.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	databaseURL = strings.TrimSpace(databaseURL)
	if databaseURL == "" {
		return nil, nil
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open mailbox database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	store := &Store{db: db}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mailbox database: %w", err)
	}
	return store, nil
}

func (s *Store) Enabled() bool { return s != nil && s.db != nil }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	if !s.Enabled() {
		return errors.New("mailbox database is not configured")
	}
	return s.db.PingContext(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	if !s.Enabled() {
		return errors.New("mailbox database is not configured")
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS mailbox_schema_migrations (
			version integer PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE IF NOT EXISTS mailboxes (
			id text PRIMARY KEY,
			source_id text NOT NULL DEFAULT '',
			source_type text NOT NULL,
			source_account_id text NOT NULL DEFAULT '',
			source_mailbox_id text NOT NULL DEFAULT '',
			email text NOT NULL,
			status text NOT NULL DEFAULT 'available',
			capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
			metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
			last_sync_at timestamptz,
			last_error text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now(),
			UNIQUE (source_type, source_account_id, source_mailbox_id)
		)`,
		`ALTER TABLE mailboxes ADD COLUMN IF NOT EXISTS source_id text NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS mailboxes_email_ci_idx ON mailboxes (lower(email))`,
		`CREATE INDEX IF NOT EXISTS mailboxes_source_idx ON mailboxes (source_id, created_at ASC)`,
		`CREATE INDEX IF NOT EXISTS mailboxes_status_idx ON mailboxes (status, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS mailbox_claims (
			id text PRIMARY KEY,
			mailbox_id text NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			platform text NOT NULL,
			account_id text NOT NULL DEFAULT '',
			registration_job_id text NOT NULL DEFAULT '',
			status text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now(),
			released_at timestamptz
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS mailbox_claims_active_platform_idx
			ON mailbox_claims (mailbox_id, platform)
			WHERE status IN ('reserved', 'active')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS mailbox_claims_job_idx
			ON mailbox_claims (platform, registration_job_id)
			WHERE registration_job_id <> '' AND status IN ('reserved', 'active')`,
		`CREATE INDEX IF NOT EXISTS mailbox_claims_account_idx ON mailbox_claims (platform, account_id, status)`,
		`CREATE TABLE IF NOT EXISTS mailbox_events (
			id text PRIMARY KEY,
			mailbox_id text NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			claim_id text NOT NULL DEFAULT '',
			event_type text NOT NULL,
			source_type text NOT NULL DEFAULT '',
			platform text NOT NULL DEFAULT '',
			account_id text NOT NULL DEFAULT '',
			registration_job_id text NOT NULL DEFAULT '',
			actor text NOT NULL DEFAULT '',
			request_id text NOT NULL DEFAULT '',
			result text NOT NULL DEFAULT '',
			error text NOT NULL DEFAULT '',
			metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
			created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS mailbox_events_mailbox_idx ON mailbox_events (mailbox_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS mailbox_events_created_idx ON mailbox_events (created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS mailbox_sources (
			id text PRIMARY KEY,
			type text NOT NULL,
			label text NOT NULL DEFAULT '',
			enabled boolean NOT NULL DEFAULT true,
			config jsonb NOT NULL DEFAULT '{}'::jsonb,
			capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
			status text NOT NULL DEFAULT 'unknown',
			last_error text NOT NULL DEFAULT '',
			last_sync_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS mailbox_sources_enabled_idx ON mailbox_sources (enabled, updated_at DESC)`,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mailbox migration: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply mailbox schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_schema_migrations(version) VALUES (1) ON CONFLICT (version) DO NOTHING`); err != nil {
		return fmt.Errorf("record mailbox schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mailbox migration: %w", err)
	}
	return nil
}

func newID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func normalizeSource(sourceType, sourceAccountID, sourceMailboxID, email string) (string, string, string, string, error) {
	sourceType = strings.TrimSpace(strings.ToLower(sourceType))
	if sourceType == "" {
		sourceType = "hme"
	}
	sourceAccountID = strings.TrimSpace(sourceAccountID)
	sourceMailboxID = strings.TrimSpace(sourceMailboxID)
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return "", "", "", "", errors.New("mailbox email is required")
	}
	if sourceMailboxID == "" {
		sourceMailboxID = email
	}
	return sourceType, sourceAccountID, sourceMailboxID, email, nil
}

func (s *Store) upsertTx(ctx context.Context, tx *sql.Tx, item Mailbox) (Mailbox, error) {
	sourceType, sourceAccountID, sourceMailboxID, email, err := normalizeSource(item.SourceType, item.SourceAccountID, item.SourceMailboxID, item.Email)
	if err != nil {
		return Mailbox{}, err
	}
	// Email is the provider-independent identity of a mailbox asset. Provider
	// aliases may expose an anonymousId that must remain in source_mailbox_id;
	// it must never replace the canonical database id or make reconciliation
	// collide with the case-insensitive email index.
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE lower(email)=lower($1) LIMIT 1`, email).Scan(&id); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, fmt.Errorf("find mailbox by email: %w", err)
	}
	if id == "" {
		id = newID("mbx")
	}
	status := strings.TrimSpace(item.Status)
	if status == "" {
		status = "available"
	}
	var out Mailbox
	err = tx.QueryRowContext(ctx, `
		INSERT INTO mailboxes(id, source_id, source_type, source_account_id, source_mailbox_id, email, status, last_error, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now(),now())
		ON CONFLICT (id) DO UPDATE SET
			source_id=CASE WHEN EXCLUDED.source_id <> '' THEN EXCLUDED.source_id ELSE mailboxes.source_id END,
			source_type=EXCLUDED.source_type,
			source_account_id=EXCLUDED.source_account_id,
			source_mailbox_id=EXCLUDED.source_mailbox_id,
			email=EXCLUDED.email,
			status=CASE WHEN mailboxes.status IN ('reserved','active','cleanup_pending','disabled','deleted') THEN mailboxes.status ELSE EXCLUDED.status END,
			last_error=CASE WHEN EXCLUDED.last_error <> '' THEN EXCLUDED.last_error ELSE mailboxes.last_error END,
			updated_at=now()
		RETURNING id, source_id, source_type, source_account_id, source_mailbox_id, email, status, last_error, created_at, updated_at`,
		id, strings.TrimSpace(item.SourceID), sourceType, sourceAccountID, sourceMailboxID, email, status, strings.TrimSpace(item.LastError)).Scan(
		&out.ID, &out.SourceID, &out.SourceType, &out.SourceAccountID, &out.SourceMailboxID, &out.Email, &out.Status, &out.LastError, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return Mailbox{}, fmt.Errorf("upsert mailbox: %w", err)
	}
	return out, nil
}

func (s *Store) writeEventTx(ctx context.Context, tx *sql.Tx, event Event) error {
	metadata, err := json.Marshal(event.Metadata)
	if err != nil {
		metadata = []byte(`{}`)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_events
		(id, mailbox_id, claim_id, event_type, source_type, platform, account_id, registration_job_id, actor, request_id, result, error, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		firstNonEmpty(event.ID, newID("evt")), strings.TrimSpace(event.MailboxID), strings.TrimSpace(event.ClaimID), strings.TrimSpace(event.EventType), strings.TrimSpace(event.SourceType), strings.TrimSpace(event.Platform), strings.TrimSpace(event.AccountID), strings.TrimSpace(event.RegistrationJob), strings.TrimSpace(event.Actor), strings.TrimSpace(event.RequestID), strings.TrimSpace(event.Result), strings.TrimSpace(event.Error), metadata)
	return err
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Upsert imports a provider mailbox into the canonical asset registry. It is
// intentionally non-destructive: reconciliation never releases claims or
// deletes an upstream alias.
func (s *Store) Upsert(ctx context.Context, item Mailbox, eventType string) (Mailbox, error) {
	if !s.Enabled() {
		return Mailbox{}, errors.New("mailbox database is not configured")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mailbox{}, err
	}
	item, err = s.upsertTx(ctx, tx, item)
	if err != nil {
		tx.Rollback()
		return Mailbox{}, err
	}
	if eventType == "" {
		eventType = "mailbox_synced"
	}
	if err := s.writeEventTx(ctx, tx, Event{MailboxID: item.ID, EventType: eventType, SourceType: item.SourceType, Result: "ok"}); err != nil {
		tx.Rollback()
		return Mailbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return Mailbox{}, err
	}
	return item, nil
}

// Reserve reserves an existing mailbox that is free for the requested
// platform, or creates one through acquire when no suitable mailbox exists.
// The prepare callback re-authenticates an existing provider mailbox.
func (s *Store) Reserve(ctx context.Context, platform, jobID, sourceID string, acquire func(context.Context) (Mailbox, error), prepare func(context.Context, Mailbox) error) (Mailbox, error) {
	if !s.Enabled() {
		return acquire(ctx)
	}
	platform = strings.ToLower(strings.TrimSpace(platform))
	jobID = strings.TrimSpace(jobID)
	sourceID = strings.TrimSpace(sourceID)
	if platform == "" || jobID == "" {
		return Mailbox{}, errors.New("mailbox platform and job are required")
	}
	if existing, ok, err := s.claimForJob(ctx, platform, jobID); err != nil {
		return Mailbox{}, err
	} else if ok {
		if prepare != nil {
			if err := prepare(ctx, existing); err != nil {
				return Mailbox{}, err
			}
		}
		return existing, nil
	}

	// Claim an existing alias first. The partial unique index serializes races
	// between GPT and Grok while keeping their claims independent. Creation time
	// is the allocation order: a later claim, sync, or error update must not
	// move an older mailbox behind newer assets.
	var candidate Mailbox
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mailbox{}, err
	}
	row := tx.QueryRowContext(ctx, `SELECT m.id,m.source_id,m.source_type,m.source_account_id,m.source_mailbox_id,m.email,m.status,m.last_error,m.created_at,m.updated_at
		FROM mailboxes m
		WHERE m.status IN ('available','active')
		AND ($1='' OR m.source_id=$1)
		AND NOT EXISTS (SELECT 1 FROM mailbox_claims c WHERE c.mailbox_id=m.id AND c.platform=$2 AND c.status IN ('reserved','active'))
		ORDER BY m.created_at ASC, m.id ASC FOR UPDATE SKIP LOCKED LIMIT 1`, sourceID, platform)
	if scanErr := row.Scan(&candidate.ID, &candidate.SourceID, &candidate.SourceType, &candidate.SourceAccountID, &candidate.SourceMailboxID, &candidate.Email, &candidate.Status, &candidate.LastError, &candidate.CreatedAt, &candidate.UpdatedAt); scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
		tx.Rollback()
		return Mailbox{}, scanErr
	}
	if candidate.ID != "" {
		claimID := newID("clm")
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_claims(id, mailbox_id, platform, registration_job_id, status) VALUES ($1,$2,$3,$4,'reserved')`, claimID, candidate.ID, platform, jobID); err != nil {
			tx.Rollback()
			return Mailbox{}, fmt.Errorf("reserve mailbox: %w", err)
		}
		if err := s.writeEventTx(ctx, tx, Event{MailboxID: candidate.ID, ClaimID: claimID, EventType: "claim_reserved", SourceType: candidate.SourceType, Platform: platform, RegistrationJob: jobID, Result: "existing"}); err != nil {
			tx.Rollback()
			return Mailbox{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET status='active', updated_at=now() WHERE id=$1`, candidate.ID); err != nil {
			tx.Rollback()
			return Mailbox{}, err
		}
		if err := tx.Commit(); err != nil {
			return Mailbox{}, err
		}
		if prepare != nil {
			if err := prepare(ctx, candidate); err != nil {
				_ = s.Release(ctx, platform, jobID, candidate, "provider_prepare_failed")
				return Mailbox{}, err
			}
		}
		return candidate, nil
	}
	tx.Rollback()

	created, err := acquire(ctx)
	if err != nil {
		return Mailbox{}, err
	}
	created.SourceType = firstNonEmpty(created.SourceType, "hme")
	created.SourceMailboxID = firstNonEmpty(created.SourceMailboxID, created.ID, created.Email)
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mailbox{}, err
	}
	item, err := s.upsertTx(ctx, tx, created)
	if err != nil {
		tx.Rollback()
		return Mailbox{}, err
	}
	if item.Status == "cleanup_pending" || item.Status == "deleted" {
		tx.Rollback()
		return Mailbox{}, ErrMailboxStateImmutable
	}
	claimID := newID("clm")
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_claims(id, mailbox_id, platform, registration_job_id, status) VALUES ($1,$2,$3,$4,'reserved')`, claimID, item.ID, platform, jobID); err != nil {
		tx.Rollback()
		return Mailbox{}, fmt.Errorf("claim created mailbox: %w", err)
	}
	if err := s.writeEventTx(ctx, tx, Event{MailboxID: item.ID, ClaimID: claimID, EventType: "mailbox_created", SourceType: item.SourceType, Platform: platform, RegistrationJob: jobID, Result: "created"}); err != nil {
		tx.Rollback()
		return Mailbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return Mailbox{}, err
	}
	return item, nil
}

func (s *Store) claimForJob(ctx context.Context, platform, jobID string) (Mailbox, bool, error) {
	var item Mailbox
	err := s.db.QueryRowContext(ctx, `SELECT m.id,m.email,m.source_id,m.source_type,m.source_account_id,m.source_mailbox_id,m.status,m.last_error,m.created_at,m.updated_at
		FROM mailbox_claims c JOIN mailboxes m ON m.id=c.mailbox_id
		WHERE c.platform=$1 AND c.registration_job_id=$2 AND c.status IN ('reserved','active')
		ORDER BY c.updated_at DESC LIMIT 1`, platform, jobID).Scan(&item.ID, &item.Email, &item.SourceID, &item.SourceType, &item.SourceAccountID, &item.SourceMailboxID, &item.Status, &item.LastError, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, false, nil
	}
	if err != nil {
		return Mailbox{}, false, err
	}
	return item, true, nil
}

// EnsureClaim adopts a mailbox already present in a registration journal or
// account record. It is idempotent and never creates an upstream alias.
func (s *Store) EnsureClaim(ctx context.Context, platform, jobID string, item Mailbox) error {
	if !s.Enabled() {
		return nil
	}
	platform, jobID = strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(jobID)
	if platform == "" || jobID == "" {
		return errors.New("mailbox platform and job are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	item, err = s.upsertTx(ctx, tx, item)
	if err != nil {
		tx.Rollback()
		return err
	}
	if item.Status == "cleanup_pending" || item.Status == "deleted" {
		tx.Rollback()
		return ErrMailboxStateImmutable
	}
	var claimID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM mailbox_claims WHERE mailbox_id=$1 AND platform=$2 AND status IN ('reserved','active') LIMIT 1`, item.ID, platform).Scan(&claimID)
	if errors.Is(err, sql.ErrNoRows) {
		claimID = newID("clm")
		if _, err = tx.ExecContext(ctx, `INSERT INTO mailbox_claims(id, mailbox_id, platform, registration_job_id, status) VALUES ($1,$2,$3,$4,'reserved')`, claimID, item.ID, platform, jobID); err != nil {
			tx.Rollback()
			return err
		}
		if err = s.writeEventTx(ctx, tx, Event{MailboxID: item.ID, ClaimID: claimID, EventType: "claim_adopted", SourceType: item.SourceType, Platform: platform, RegistrationJob: jobID, Result: "adopted"}); err != nil {
			tx.Rollback()
			return err
		}
	} else if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) Commit(ctx context.Context, platform, jobID, accountID string, item Mailbox) error {
	if !s.Enabled() {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var canonicalID, currentStatus string
	if err := tx.QueryRowContext(ctx, `SELECT id,status FROM mailboxes WHERE lower(email)=lower($1) LIMIT 1 FOR UPDATE`, item.Email).Scan(&canonicalID, &currentStatus); err == nil {
		item.ID = canonicalID
		if currentStatus == "cleanup_pending" || currentStatus == "deleted" {
			tx.Rollback()
			return ErrMailboxStateImmutable
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return err
	} else if strings.TrimSpace(item.ID) == "" {
		tx.Rollback()
		return s.EnsureClaim(ctx, platform, jobID, item)
	}
	var claimID, sourceType string
	err = tx.QueryRowContext(ctx, `SELECT c.id,m.source_type FROM mailbox_claims c JOIN mailboxes m ON m.id=c.mailbox_id WHERE c.mailbox_id=$1 AND c.platform=$2 AND (c.registration_job_id=$3 OR c.status IN ('reserved','active')) ORDER BY c.updated_at DESC LIMIT 1`, item.ID, strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(jobID)).Scan(&claimID, &sourceType)
	if err != nil {
		tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return s.EnsureClaim(ctx, platform, jobID, item)
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox_claims SET status='active', account_id=$1, updated_at=now() WHERE id=$2`, strings.TrimSpace(accountID), claimID); err != nil {
		tx.Rollback()
		return err
	}
	var finalStatus string
	if err := tx.QueryRowContext(ctx, `UPDATE mailboxes SET status=CASE WHEN status='disabled' THEN 'disabled' ELSE 'active' END, updated_at=now(), last_error='' WHERE id=$1 RETURNING status`, item.ID).Scan(&finalStatus); err != nil {
		tx.Rollback()
		return err
	}
	if err := s.writeEventTx(ctx, tx, Event{MailboxID: item.ID, ClaimID: claimID, EventType: "claim_committed", SourceType: sourceType, Platform: platform, AccountID: accountID, RegistrationJob: jobID, Result: finalStatus}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) Release(ctx context.Context, platform, jobID string, item Mailbox, reason string) error {
	if !s.Enabled() {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var canonicalID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE lower(email)=lower($1) LIMIT 1`, item.Email).Scan(&canonicalID); err == nil {
		item.ID = canonicalID
	} else if !errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return err
	} else if strings.TrimSpace(item.ID) == "" {
		tx.Rollback()
		return nil
	}
	var claimID, sourceType string
	err = tx.QueryRowContext(ctx, `SELECT c.id,m.source_type FROM mailbox_claims c JOIN mailboxes m ON m.id=c.mailbox_id WHERE c.mailbox_id=$1 AND c.platform=$2 AND (c.registration_job_id=$3 OR c.status IN ('reserved','active')) ORDER BY c.updated_at DESC LIMIT 1`, item.ID, strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(jobID)).Scan(&claimID, &sourceType)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		return nil
	}
	if err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox_claims SET status='released', released_at=now(), updated_at=now() WHERE id=$1`, claimID); err != nil {
		tx.Rollback()
		return err
	}
	status := "available"
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_claims WHERE mailbox_id=$1 AND status IN ('reserved','active')`, item.ID).Scan(&count); err != nil {
		tx.Rollback()
		return err
	}
	if count > 0 {
		status = "active"
	}
	var finalStatus string
	if err := tx.QueryRowContext(ctx, `UPDATE mailboxes SET status=CASE WHEN status IN ('disabled','cleanup_pending','deleted') THEN status ELSE $1 END, updated_at=now() WHERE id=$2 RETURNING status`, status, item.ID).Scan(&finalStatus); err != nil {
		tx.Rollback()
		return err
	}
	if err := s.writeEventTx(ctx, tx, Event{MailboxID: item.ID, ClaimID: claimID, EventType: "claim_released", SourceType: sourceType, Platform: platform, RegistrationJob: jobID, Result: finalStatus, Error: reason}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ReleaseForAccount always releases local platform ownership, independently of
// provider availability or the optional upstream deletion policy. Returned assets
// include shared and disabled mailboxes, so callers can report retention precisely.
func (s *Store) ReleaseForAccount(ctx context.Context, platform, accountID, email string, deletedBefore ...time.Time) ([]Mailbox, error) {
	before := time.Now()
	if len(deletedBefore) > 0 {
		before = deletedBefore[0]
	}
	return s.releaseAccountClaims(ctx, platform, accountID, email, "", before, false)
}

// ReleaseDeletedAccount repairs a historical deletion only when a claim's saved
// account/job identity matches and it predates the deletion. The caller also
// verifies that no current account uses this email. Reservations are untouched.
func (s *Store) ReleaseDeletedAccount(ctx context.Context, platform, accountID, jobID, email string, deletedAt time.Time) ([]Mailbox, error) {
	return s.releaseAccountClaims(ctx, platform, accountID, email, jobID, deletedAt, true)
}

func (s *Store) releaseAccountClaims(ctx context.Context, platform, accountID, email, jobID string, before time.Time, exactOnly bool) ([]Mailbox, error) {
	if !s.Enabled() {
		return nil, nil
	}
	platform = strings.ToLower(strings.TrimSpace(platform))
	accountID, email = strings.TrimSpace(accountID), strings.ToLower(strings.TrimSpace(email))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Email fallback is needed for legacy active claims whose account_id stored
	// a registration job ID. Never release a reservation by email; the cutoff
	// also protects newer activations while a deletion request is in flight.
	predicate := `c.platform=$1 AND c.status='active' AND c.updated_at <= $4
		AND (($2 <> '' AND c.account_id=$2) OR ($5 <> '' AND c.registration_job_id=$5)
		 OR (NOT $6 AND $3 <> '' AND lower(m.email)=$3))`
	rows, err := tx.QueryContext(ctx, `SELECT m.id FROM mailboxes m WHERE EXISTS (
		SELECT 1 FROM mailbox_claims c WHERE c.mailbox_id=m.id AND `+predicate+`)
		ORDER BY m.id FOR UPDATE OF m`, platform, accountID, email, before, jobID, exactOnly)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var result []Mailbox
	for _, id := range ids {
		var claimID string
		err := tx.QueryRowContext(ctx, `UPDATE mailbox_claims c SET status='released', released_at=now(), updated_at=now()
			FROM mailboxes m WHERE m.id=$7 AND c.mailbox_id=m.id AND `+predicate+`
			RETURNING c.id`, platform, accountID, email, before, jobID, exactOnly, id).Scan(&claimID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		item, err := scanMailbox(tx.QueryRowContext(ctx, `UPDATE mailboxes SET status=CASE
			WHEN status IN ('disabled','cleanup_pending','deleted') THEN status
			WHEN EXISTS (SELECT 1 FROM mailbox_claims WHERE mailbox_id=$1 AND status IN ('reserved','active')) THEN 'active'
			ELSE 'available' END, updated_at=now() WHERE id=$1 RETURNING `+mailboxColumns, id))
		if err != nil {
			return nil, err
		}
		if err := s.writeEventTx(ctx, tx, Event{MailboxID: id, ClaimID: claimID, EventType: "account_claim_released", SourceType: item.SourceType, Platform: platform, AccountID: accountID, Result: item.Status}); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) MarkCleanup(ctx context.Context, item Mailbox, success bool, message string) error {
	if !s.Enabled() {
		return nil
	}
	status := "cleanup_pending"
	eventType := "provider_cleanup_failed"
	result := "failed"
	if success {
		status = "deleted"
		eventType = "provider_cleanup_succeeded"
		result = "deleted"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var sourceType string
	if err := tx.QueryRowContext(ctx, `UPDATE mailboxes SET status=$1,last_error=$2,updated_at=now() WHERE id=$3 RETURNING source_type`, status, strings.TrimSpace(message), item.ID).Scan(&sourceType); err != nil {
		tx.Rollback()
		return err
	}
	if err := s.writeEventTx(ctx, tx, Event{MailboxID: item.ID, EventType: eventType, SourceType: sourceType, Result: result, Error: strings.TrimSpace(message)}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SetStatus changes the administrative availability of a mailbox asset.
// Disabled assets remain in the registry and in the upstream provider, but
// Reserve deliberately skips them. Enabling an asset restores the derived
// status from its active claims.
func (s *Store) SetStatus(ctx context.Context, mailboxID, requested string) (Mailbox, error) {
	if !s.Enabled() {
		return Mailbox{}, errors.New("mailbox database is not configured")
	}
	mailboxID = strings.TrimSpace(mailboxID)
	requested = strings.ToLower(strings.TrimSpace(requested))
	if mailboxID == "" {
		return Mailbox{}, ErrMailboxNotFound
	}
	if requested != "disabled" && requested != "available" {
		return Mailbox{}, ErrInvalidMailboxStatus
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mailbox{}, err
	}
	defer tx.Rollback()
	var current Mailbox
	err = tx.QueryRowContext(ctx, `SELECT id,source_id,source_type,source_account_id,source_mailbox_id,email,status,last_error,created_at,updated_at
		FROM mailboxes WHERE id=$1 FOR UPDATE`, mailboxID).Scan(
		&current.ID, &current.SourceID, &current.SourceType, &current.SourceAccountID, &current.SourceMailboxID,
		&current.Email, &current.Status, &current.LastError, &current.CreatedAt, &current.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, ErrMailboxNotFound
	}
	if err != nil {
		return Mailbox{}, err
	}
	if current.Status == "cleanup_pending" || current.Status == "deleted" {
		return Mailbox{}, ErrMailboxStateImmutable
	}
	finalStatus := requested
	if requested == "available" {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_claims WHERE mailbox_id=$1 AND status IN ('reserved','active')`, mailboxID).Scan(&active); err != nil {
			return Mailbox{}, err
		}
		if active > 0 {
			finalStatus = "active"
		}
	}
	var updated Mailbox
	if err := tx.QueryRowContext(ctx, `UPDATE mailboxes SET status=$1,updated_at=now() WHERE id=$2
		RETURNING id,source_id,source_type,source_account_id,source_mailbox_id,email,status,last_error,created_at,updated_at`, finalStatus, mailboxID).Scan(
		&updated.ID, &updated.SourceID, &updated.SourceType, &updated.SourceAccountID, &updated.SourceMailboxID,
		&updated.Email, &updated.Status, &updated.LastError, &updated.CreatedAt, &updated.UpdatedAt); err != nil {
		return Mailbox{}, err
	}
	eventType := "mailbox_enabled"
	if requested == "disabled" {
		eventType = "mailbox_disabled"
	}
	if err := s.writeEventTx(ctx, tx, Event{
		MailboxID: updated.ID, EventType: eventType, SourceType: updated.SourceType,
		Actor: "admin", Result: updated.Status,
		Metadata: map[string]any{"previous_status": current.Status, "requested_status": requested},
	}); err != nil {
		return Mailbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return Mailbox{}, err
	}
	return updated, nil
}

func (s *Store) List(ctx context.Context, page, pageSize int, filter MailboxListFilter) ([]MailboxView, int, error) {
	if !s.Enabled() {
		return nil, 0, errors.New("mailbox database is not configured")
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 20
	}
	args := []any{}
	conditions := []string{"1=1"}
	addArg := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if search := strings.TrimSpace(filter.Search); search != "" {
		placeholder := addArg("%" + strings.ToLower(search) + "%")
		conditions = append(conditions, "(lower(m.email) LIKE "+placeholder+" OR lower(m.source_type) LIKE "+placeholder+" OR lower(m.source_account_id) LIKE "+placeholder+")")
	}
	if status := strings.TrimSpace(strings.ToLower(filter.Status)); status != "" {
		conditions = append(conditions, "m.status="+addArg(status))
	}
	if sourceID := strings.TrimSpace(filter.SourceID); sourceID != "" {
		conditions = append(conditions, "m.source_id="+addArg(sourceID))
	}
	switch platform := strings.TrimSpace(strings.ToLower(filter.Platform)); platform {
	case "openai", "grok":
		conditions = append(conditions, "EXISTS (SELECT 1 FROM mailbox_claims fp WHERE fp.mailbox_id=m.id AND fp.platform="+addArg(platform)+" AND fp.status IN ('reserved','active'))")
	case "unclaimed":
		conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM mailbox_claims fp WHERE fp.mailbox_id=m.id AND fp.status IN ('reserved','active'))")
	}
	where := "WHERE " + strings.Join(conditions, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM mailboxes m "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limitPlaceholder := addArg(pageSize)
	offsetPlaceholder := addArg((page - 1) * pageSize)
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.source_id,m.source_type,m.source_account_id,m.source_mailbox_id,m.email,m.status,m.last_error,m.created_at,m.updated_at,m.last_sync_at,
		COALESCE(bool_or(c.platform='openai' AND c.status IN ('reserved','active')),false), COALESCE(bool_or(c.platform='grok' AND c.status IN ('reserved','active')),false)
		FROM mailboxes m LEFT JOIN mailbox_claims c ON c.mailbox_id=m.id `+where+` GROUP BY m.id ORDER BY m.updated_at DESC LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	views := []MailboxView{}
	for rows.Next() {
		var view MailboxView
		if err := rows.Scan(&view.ID, &view.SourceID, &view.SourceType, &view.SourceAccountID, &view.SourceMailboxID, &view.Email, &view.Status, &view.LastError, &view.CreatedAt, &view.UpdatedAt, &view.LastSyncAt, &view.OpenAIClaimed, &view.GrokClaimed); err != nil {
			return nil, 0, err
		}
		views = append(views, view)
	}
	return views, total, rows.Err()
}

func (s *Store) Events(ctx context.Context, page, pageSize int, mailboxID, platform string) ([]Event, int, error) {
	if !s.Enabled() {
		return nil, 0, errors.New("mailbox database is not configured")
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 500 {
		pageSize = 50
	}
	where := []string{"1=1"}
	args := []any{}
	if mailboxID = strings.TrimSpace(mailboxID); mailboxID != "" {
		args = append(args, mailboxID)
		where = append(where, fmt.Sprintf("mailbox_id=$%d", len(args)))
	}
	if platform = strings.TrimSpace(strings.ToLower(platform)); platform != "" {
		args = append(args, platform)
		where = append(where, fmt.Sprintf("platform=$%d", len(args)))
	}
	whereSQL := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM mailbox_events WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `SELECT id,mailbox_id,claim_id,event_type,source_type,platform,account_id,registration_job_id,actor,request_id,result,error,metadata,created_at FROM mailbox_events WHERE `+whereSQL+` ORDER BY created_at DESC LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var event Event
		var metadata []byte
		if err := rows.Scan(&event.ID, &event.MailboxID, &event.ClaimID, &event.EventType, &event.SourceType, &event.Platform, &event.AccountID, &event.RegistrationJob, &event.Actor, &event.RequestID, &event.Result, &event.Error, &metadata, &event.CreatedAt); err != nil {
			return nil, 0, err
		}
		if len(metadata) > 0 {
			_ = json.Unmarshal(metadata, &event.Metadata)
		}
		events = append(events, event)
	}
	return events, total, rows.Err()
}

func sourceJSON(value map[string]any) ([]byte, error) {
	if value == nil {
		return []byte(`{}`), nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// UpsertSource stores a source configuration during migration or from the
// mailbox management module. Secrets remain server-side and are never exposed
// by ListSources.
func (s *Store) UpsertSource(ctx context.Context, source Source) (Source, error) {
	if !s.Enabled() {
		return Source{}, errors.New("mailbox database is not configured")
	}
	source.ID = strings.TrimSpace(source.ID)
	source.Type = strings.TrimSpace(strings.ToLower(source.Type))
	if source.ID == "" || source.Type == "" {
		return Source{}, errors.New("mailbox source id and type are required")
	}
	config, err := sourceJSON(source.Config)
	if err != nil {
		return Source{}, err
	}
	capabilities, err := sourceJSON(source.Capabilities)
	if err != nil {
		return Source{}, err
	}
	var out Source
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO mailbox_sources(id,type,label,enabled,config,capabilities,status,last_error,last_sync_at,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now(),now())
		ON CONFLICT (id) DO UPDATE SET
			type=EXCLUDED.type,
			label=EXCLUDED.label,
			enabled=EXCLUDED.enabled,
			config=EXCLUDED.config,
			capabilities=EXCLUDED.capabilities,
			status=CASE WHEN EXCLUDED.status <> '' THEN EXCLUDED.status ELSE mailbox_sources.status END,
			last_error=CASE WHEN EXCLUDED.last_error <> '' THEN EXCLUDED.last_error ELSE mailbox_sources.last_error END,
			updated_at=now()
		RETURNING id,type,label,enabled,config,capabilities,status,last_error,last_sync_at,created_at,updated_at`,
		source.ID, source.Type, strings.TrimSpace(source.Label), source.Enabled, config, capabilities, strings.TrimSpace(source.Status), strings.TrimSpace(source.LastError), source.LastSyncAt).
		Scan(&out.ID, &out.Type, &out.Label, &out.Enabled, &config, &capabilities, &out.Status, &out.LastError, &out.LastSyncAt, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return Source{}, err
	}
	_ = json.Unmarshal(config, &out.Config)
	_ = json.Unmarshal(capabilities, &out.Capabilities)
	return out, nil
}

func (s *Store) ListSources(ctx context.Context) ([]Source, error) {
	if !s.Enabled() {
		return nil, errors.New("mailbox database is not configured")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,type,label,enabled,config,capabilities,status,last_error,last_sync_at,created_at,updated_at FROM mailbox_sources ORDER BY created_at ASC,id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Source{}
	for rows.Next() {
		var source Source
		var config, capabilities []byte
		if err := rows.Scan(&source.ID, &source.Type, &source.Label, &source.Enabled, &config, &capabilities, &source.Status, &source.LastError, &source.LastSyncAt, &source.CreatedAt, &source.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(config, &source.Config)
		_ = json.Unmarshal(capabilities, &source.Capabilities)
		result = append(result, source)
	}
	return result, rows.Err()
}

func (s *Store) GetSource(ctx context.Context, id string) (Source, error) {
	if !s.Enabled() {
		return Source{}, errors.New("mailbox database is not configured")
	}
	var source Source
	var config, capabilities []byte
	err := s.db.QueryRowContext(ctx, `SELECT id,type,label,enabled,config,capabilities,status,last_error,last_sync_at,created_at,updated_at FROM mailbox_sources WHERE id=$1`, strings.TrimSpace(id)).Scan(&source.ID, &source.Type, &source.Label, &source.Enabled, &config, &capabilities, &source.Status, &source.LastError, &source.LastSyncAt, &source.CreatedAt, &source.UpdatedAt)
	if err != nil {
		return Source{}, err
	}
	_ = json.Unmarshal(config, &source.Config)
	_ = json.Unmarshal(capabilities, &source.Capabilities)
	return source, nil
}

// BackfillSourceID associates legacy mailbox rows with a migrated source. It
// only touches rows that have no source id, so an explicit association made by
// reconciliation or an administrator is preserved.
func (s *Store) BackfillSourceID(ctx context.Context, sourceID, sourceType, accountID string) (int64, error) {
	if !s.Enabled() {
		return 0, errors.New("mailbox database is not configured")
	}
	sourceID = strings.TrimSpace(sourceID)
	sourceType = strings.TrimSpace(strings.ToLower(sourceType))
	accountID = strings.TrimSpace(accountID)
	if sourceID == "" {
		return 0, errors.New("mailbox source id is required")
	}
	args := []any{sourceID}
	where := "source_id = '' AND lower(source_type) IN ('hme','icloud_hme')"
	if sourceType != "" && sourceType != "hme" && sourceType != "icloud_hme" {
		where = "source_id = '' AND lower(source_type) = $2"
		args = append(args, sourceType)
	}
	if accountID != "" {
		args = append(args, accountID)
		where += fmt.Sprintf(" AND source_account_id = $%d", len(args))
	}
	result, err := s.db.ExecContext(ctx, "UPDATE mailboxes SET source_id=$1, updated_at=updated_at WHERE "+where, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) Counts(ctx context.Context) (map[string]any, error) {
	if !s.Enabled() {
		return nil, errors.New("mailbox database is not configured")
	}
	result := map[string]any{}
	for key, query := range map[string]string{
		"mailboxes":       "SELECT count(*) FROM mailboxes",
		"available":       "SELECT count(*) FROM mailboxes WHERE status='available'",
		"active":          "SELECT count(*) FROM mailboxes WHERE status='active'",
		"disabled":        "SELECT count(*) FROM mailboxes WHERE status='disabled'",
		"cleanup_pending": "SELECT count(*) FROM mailboxes WHERE status='cleanup_pending'",
		"events":          "SELECT count(*) FROM mailbox_events",
	} {
		var count int
		if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return nil, err
		}
		result[key] = count
	}
	return result, nil
}
