package adobe

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type diskState struct {
	Accounts           []Account                           `json:"accounts"`
	Bindings           map[string]string                   `json:"bindings"`
	Resolutions        map[string]SubmissionResolution     `json:"submission_resolutions,omitempty"`
	Reconciliations    map[string]SubmissionReconciliation `json:"submission_reconciliations,omitempty"`
	PreparedMediaTasks map[string]uint64                   `json:"media_prepared_tasks,omitempty"`
	MediaFailures      map[string]mediaPreparationFailure  `json:"media_preparation_failures,omitempty"`
}
type Store struct {
	browserRefresher func(context.Context, Account) (Account, error)
	path, secret     string
	mu               sync.Mutex
	maintenance      map[string]bool
	quotaQueries     map[string]bool
	mediaReady       map[string]mediaReadiness
	mediaFlights     map[string]*mediaPreparationFlight
	accountChanged   map[string]chan struct{}
	modelFailures    map[string]modelPermissionFailure
}

func NewStore(path, secret string) *Store {
	return &Store{path: path, secret: secret, maintenance: map[string]bool{}, quotaQueries: map[string]bool{}, mediaReady: map[string]mediaReadiness{}, mediaFlights: map[string]*mediaPreparationFlight{}, accountChanged: map[string]chan struct{}{}, modelFailures: map[string]modelPermissionFailure{}}
}
func Seal(secret string, plain []byte) ([]byte, error) {
	a, err := aead(secret)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, plain, []byte("cfm-adobe-v1")), nil
}
func Open(secret string, raw []byte) ([]byte, error) {
	a, err := aead(secret)
	if err != nil {
		return nil, err
	}
	if len(raw) < a.NonceSize() {
		return nil, errors.New("adobe_store_invalid")
	}
	return a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte("cfm-adobe-v1"))
}
func aead(secret string) (cipher.AEAD, error) {
	key := sha256.Sum256([]byte(secret))
	b, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".adobe-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if ce := f.Close(); err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) load() (diskState, error) {
	d := diskState{Accounts: []Account{}, Bindings: map[string]string{}}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	raw, err = Open(s.secret, raw)
	if err != nil {
		return d, errors.New("adobe_store_decryption_failed")
	}
	if err = json.Unmarshal(raw, &d); err != nil {
		return d, errors.New("adobe_store_invalid")
	}
	if d.Bindings == nil {
		d.Bindings = map[string]string{}
	}
	for i := range d.Accounts {
		d.Accounts[i].TaskReconciliation = reconciliationForAccount(d, d.Accounts[i], time.Now())
	}
	return d, nil
}
func (s *Store) save(d diskState) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	raw, err = Seal(s.secret, raw)
	if err != nil {
		return err
	}
	return atomicWrite(s.path, raw)
}
func (s *Store) List() ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	return d.Accounts, err
}
func (s *Store) PublicList() ([]map[string]any, error) {
	a, err := s.List()
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(a))
	for _, v := range a {
		rows = append(rows, Public(v))
	}
	return rows, nil
}
func (s *Store) Get(id string) (Account, error) {
	items, err := s.List()
	if err != nil {
		return Account{}, err
	}
	for _, a := range items {
		if a.ID == id {
			return a, nil
		}
	}
	return Account{}, ErrNotFound
}

// BindProxy changes only daily egress metadata and its audit trail. In-flight
// requests retain the account snapshot they acquired before this update.
func (s *Store) BindProxy(id string, binding proxyruntime.Binding, reason string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return Account{}, err
	}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		if a.ID != id {
			continue
		}
		updates := proxyruntime.RebindFields(a.RoutingFields(), binding, reason)
		raw, err := json.Marshal(updates)
		if err != nil {
			return Account{}, err
		}
		var route Account
		if err := json.Unmarshal(raw, &route); err != nil {
			return Account{}, err
		}
		a.Proxy, a.ProxyBinding = route.Proxy, route.ProxyBinding
		a.ProxyPolicy, a.ProxyBindingHistory = route.ProxyPolicy, route.ProxyBindingHistory
		if err := s.save(d); err != nil {
			return Account{}, err
		}
		delete(s.mediaReady, id)
		delete(s.modelFailures, id)
		return *a, nil
	}
	return Account{}, ErrNotFound
}

func (s *Store) Import(items []Account) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.importLocked(items)
}

func (s *Store) importLocked(items []Account) ([]Account, error) {
	d, err := s.load()
	if err != nil {
		return nil, err
	}
	result := make([]Account, 0, len(items))
	for _, a := range items {
		a.TaskReconciliation = nil
		a.AccountID = strings.TrimSpace(a.AccountID)
		id, expiry, e := TokenIdentity(strings.TrimSpace(a.Token))
		if e != nil {
			return nil, e
		}
		if a.AccountID == "" {
			a.AccountID = id
		}
		if id != a.AccountID {
			return nil, ErrIdentity
		}
		if _, _, e = MediaSessionARP(a); e != nil {
			return nil, e
		}
		a.ID = "adobe:" + a.AccountID
		// A delete/reimport cycle can reuse credential revision 1. Keep the
		// identity incarnation stable until its outstanding quota read finishes.
		if s.quotaQueries[a.ID] {
			return nil, ErrBusy
		}
		a.Token = strings.TrimSpace(a.Token)
		a.ExpiresAt = expiry
		a.CreatedAt = time.Now().Unix()
		a.MediaSessionRefreshed = false
		a.MediaSessionStatus = "imported"
		a.MediaSessionUpdatedAt = a.CreatedAt
		a.UpdatedAt = a.CreatedAt
		a.Revision = 1
		a.Quota = Quota{Status: "unknown"}
		a.Status = "pending"
		a.LastError = ""
		found := -1
		for i, v := range d.Accounts {
			if v.ID == a.ID {
				found = i
				if v.InFlight != "" || s.maintenance[v.ID] {
					return nil, ErrBusy
				}
				a.CreatedAt = v.CreatedAt
				a.Revision = v.Revision + 1
				a.Enabled = v.Enabled
				a.Proxy = v.Proxy
				a.ProxyBinding = v.ProxyBinding
				a.ProxyPolicy = v.ProxyPolicy
				a.ProxyBindingHistory = v.ProxyBindingHistory
				a.SettlementUntil = v.SettlementUntil
				if a.RegistrationJobID == "" {
					a.RegistrationJobID = v.RegistrationJobID
				}
				if a.ProfileID == "" {
					a.ProfileID = v.ProfileID
				}
				if a.RefreshCookie == "" {
					a.RefreshCookie = v.RefreshCookie
				}
				if a.Password == "" {
					a.Password = v.Password
				}
				if a.RegistrationMailbox == nil {
					a.RegistrationMailbox = v.RegistrationMailbox
				}
				break
			}
		}
		if found >= 0 {
			d.Accounts[found] = a
		} else {
			d.Accounts = append(d.Accounts, a)
		}
		result = append(result, a)
		delete(d.MediaFailures, a.ID)
	}
	if err = s.save(d); err != nil {
		return nil, err
	}
	for _, a := range result {
		delete(s.mediaReady, a.ID)
		delete(s.modelFailures, a.ID)
	}
	return result, nil
}

// Eligibility reserves one valid identity; generation preflight decides whether
// the requested model has a free resource. General credits are a separate pool.
func Eligible(a Account, now time.Time) bool {
	if !a.Enabled || a.Status == "invalid" || a.Status == "submission_unknown" || a.NextRefreshAt > now.Unix() && a.LastError != "" || a.ExpiresAt <= now.Add(time.Minute).Unix() || a.InFlight != "" || a.SettlementUntil > now.Unix() {
		return false
	}
	identity, _, err := TokenIdentity(a.Token)
	if err != nil || identity != a.AccountID {
		return false
	}
	_, _, err = MediaSessionARP(a)
	return err == nil
}

// Reserve durably binds once; failed or completed task IDs cannot generate again.
func (s *Store) Reserve(taskID string) (Account, error) { return s.reserve(taskID, "", "") }
func (s *Store) ReserveAccount(taskID, accountID string) (Account, error) {
	return s.reserve(taskID, accountID, "")
}

// ReserveForModel filters unavailable model pools before the durable binding;
// a subsequent preflight or submission failure never selects another account.
func (s *Store) ReserveForModel(taskID, model string) (Account, error) {
	return s.reserve(taskID, "", model)
}
func (s *Store) reserve(taskID, accountID, model string) (Account, error) {
	if taskID == "" {
		return Account{}, errors.New("adobe_task_id_required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return Account{}, err
	}
	if _, ok := d.Bindings[taskID]; ok {
		return Account{}, errors.New("adobe_task_already_selected")
	}
	selected := -1
	now := time.Now()
	for i, a := range d.Accounts {
		if accountID != "" && a.ID != accountID {
			continue
		}
		if model != "" && a.Quota.Generation != nil && !a.Quota.Generation.ModelAvailable(model, now) {
			continue
		}
		if !s.maintenance[a.ID] && Eligible(a, now) && (selected < 0 || a.LastUsedAt < d.Accounts[selected].LastUsedAt) {
			selected = i
		}
	}
	if selected < 0 {
		return Account{}, ErrNoAccount
	}
	a := d.Accounts[selected]
	a.InFlight = taskID
	a.LastUsedAt = now.Unix()
	a.UpdatedAt = now.Unix()
	d.Accounts[selected] = a
	d.Bindings[taskID] = a.ID
	if err = s.save(d); err != nil {
		return Account{}, err
	}
	return a, nil
}
func (s *Store) Release(taskID string) error { return s.finish(taskID, false) }

// MarkUnknown preserves the reservation after an ambiguous submission. Only
// explicit reconciliation may release it; a balance snapshot cannot prove no job.
func (s *Store) MarkUnknown(taskID string) error { return s.finish(taskID, true) }
func (s *Store) finish(taskID string, unknown bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return err
	}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		if a.InFlight != taskID {
			continue
		}
		now := time.Now()
		a.UpdatedAt = now.Unix()
		if unknown {
			a.Status = "submission_unknown"
			a.LastError = "adobe_submission_unknown"
			delete(s.mediaReady, a.ID)
		} else {
			s.recordAutomaticPollResolutionLocked(&d, *a, taskID, now)
			a.InFlight = ""
			if a.Status == "submission_unknown" {
				a.Status = "pending"
				if a.LastRefreshAt > 0 || a.Quota.LastSuccessAt != "" || a.Quota.Status == "known" {
					a.Status = "active"
				}
			}
			if a.LastError == "adobe_submission_unknown" {
				a.LastError = ""
			}
			a.QuotaRevision++
			a.SettlementUntil = now.Add(30 * time.Second).Unix()
			a.Quota.Status = "stale"
			if a.Quota.Generation != nil {
				a.Quota.Generation.Status = "stale"
			}
		}
	}
	return s.save(d)
}
func (s *Store) UpdateQuota(id string, revision uint64, q Quota) error {
	return s.update(id, revision, func(a *Account) {
		a.QuotaRevision++
		a.Quota = q
		if q.Status == "known" && a.InFlight == "" {
			a.Status = "active"
			a.LastError = ""
			next := time.Now().Add(time.Hour).Unix()
			if a.LastUsedAt > time.Now().Add(-time.Hour).Unix() {
				next = time.Now().Add(5 * time.Minute).Unix()
			}
			if expiry := a.ExpiresAt - 300; expiry < next {
				next = expiry
			}
			if next < time.Now().Unix() {
				next = time.Now().Add(30 * time.Second).Unix()
			}
			a.NextRefreshAt = next
		} else {
			a.LastError = q.Error
			if q.Status == "error" {
				a.NextRefreshAt = time.Now().Add(15 * time.Minute).Unix()
			}
		}
	})
}
func (s *Store) update(id string, revision uint64, fn func(*Account)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return err
	}
	for i := range d.Accounts {
		if d.Accounts[i].ID != id {
			continue
		}
		if revision > 0 && revision != d.Accounts[i].Revision {
			return ErrRevision
		}
		fn(&d.Accounts[i])
		d.Accounts[i].UpdatedAt = time.Now().Unix()
		return s.save(d)
	}
	return ErrNotFound
}
func (s *Store) ReplaceCredentials(expected Account, replacement Account) (Account, error) {
	return s.replaceCredentials(expected, replacement, false)
}

func (s *Store) replaceCredentials(expected Account, replacement Account, browserVerified bool) (Account, error) {
	return s.replaceCredentialsWithPreparation(expected, replacement, browserVerified, nil)
}

func (s *Store) replaceCredentialsWithPreparation(expected Account, replacement Account, browserVerified bool, preparation *mediaPreparationGuard) (Account, error) {
	var readyCtx context.Context
	if preparation != nil {
		readyCtx = preparation.ctx
	}
	return s.replaceCredentialsWithContext(expected, replacement, browserVerified, preparation, readyCtx)
}

func (s *Store) replaceCredentialsWithContext(expected Account, replacement Account, browserVerified bool, preparation *mediaPreparationGuard, readyCtx context.Context) (Account, error) {
	stage := "relogin"
	if preparation != nil {
		stage = "media_prepare"
	}
	identity, expires, err := TokenIdentity(replacement.Token)
	if err != nil {
		if browserVerified {
			return Account{}, &Error{Stage: stage, Code: "token_invalid"}
		}
		return Account{}, err
	}
	if identity != expected.AccountID {
		return Account{}, ErrIdentity
	}
	if browserVerified {
		if replacement.AccountID != expected.AccountID || !strings.EqualFold(strings.TrimSpace(replacement.Email), strings.TrimSpace(expected.Email)) {
			return Account{}, ErrIdentity
		}
		if expires <= time.Now().Unix() {
			return Account{}, &Error{Stage: stage, Code: "token_expired"}
		}
		if preparation != nil && expires <= time.Now().Add(time.Minute).Unix() {
			return Account{}, &Error{Stage: stage, Code: "token_expiring"}
		}
		if !replacement.MediaSessionRefreshed || replacement.MediaSessionStatus != "complete" {
			return Account{}, &Error{Stage: stage, Code: "media_session_incomplete"}
		}
		if _, _, err := MediaSessionARP(replacement); err != nil {
			return Account{}, &Error{Stage: stage, Code: "media_session_incomplete"}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return Account{}, err
	}
	for i, current := range d.Accounts {
		if current.ID != expected.ID {
			continue
		}
		if current.Revision != expected.Revision || (browserVerified && (current.InFlight != expected.InFlight || current.Status != expected.Status)) {
			return Account{}, ErrRevision
		}
		if current.AccountID != identity {
			return Account{}, ErrIdentity
		}
		if preparation != nil {
			if err := s.checkMediaPreparationLocked(d, current, expected, preparation); err != nil {
				return Account{}, err
			}
		}
		trustMedia := false
		if browserVerified && readyCtx != nil && current.Enabled {
			before, expectedFixed := mediaContextFingerprint(readyCtx, expected)
			after, currentFixed := mediaContextFingerprint(readyCtx, current)
			trustMedia = expectedFixed && currentFixed && before == after
		}
		mediaCookie := replacement.Cookie
		mediaStatus := "refreshed"
		if !browserVerified {
			// IMS updates authentication only. It cannot establish, remove or
			// change the official runtime's decision about the optional ARP header.
			if replacement.MediaSessionARPMode != current.MediaSessionARPMode || replacement.MediaSessionARPSource != current.MediaSessionARPSource {
				return Account{}, &Error{Stage: "refresh", Code: "arp_policy_changed"}
			}
			if _, _, e := MediaSessionARP(replacement); e != nil {
				if _, _, oldErr := MediaSessionARP(current); oldErr != nil {
					return Account{}, &Error{Stage: "refresh", Code: "media_session_incomplete"}
				}
				mediaCookie = current.Cookie
				mediaStatus = "preserved"
			} else if mediaCookie == current.Cookie {
				mediaStatus = "preserved"
			}
		} else {
			// An explicit omission must replace the entire media cookie, even
			// when empty; preserving a historical Sherlock here would be wrong.
			current.MediaSessionARPMode = replacement.MediaSessionARPMode
			current.MediaSessionARPSource = replacement.MediaSessionARPSource
		}
		current.Token = replacement.Token
		current.ExpiresAt = expires
		current.Cookie = mediaCookie
		current.MediaSessionStatus = mediaStatus
		if mediaStatus == "refreshed" {
			current.MediaSessionUpdatedAt = time.Now().Unix()
			if replacement.UserAgent != "" {
				current.UserAgent = replacement.UserAgent
			}
		}
		if replacement.RefreshCookie != "" {
			current.RefreshCookie = replacement.RefreshCookie
		}
		current.Revision++
		current.QuotaRevision++
		current.LastRefreshAt = time.Now().Unix()
		current.UpdatedAt = current.LastRefreshAt
		current.NextRefreshAt = time.Now().Add(45 * time.Minute).Unix()
		if current.Status != "submission_unknown" {
			current.LastError = ""
		}
		if browserVerified && current.Status == "invalid" {
			current.Status = "active"
		}
		d.Accounts[i] = current
		if preparation != nil && !preparation.warm {
			if d.PreparedMediaTasks == nil {
				d.PreparedMediaTasks = map[string]uint64{}
			}
			d.PreparedMediaTasks[preparation.taskKey] = current.Revision
		}
		delete(d.MediaFailures, current.ID)
		if err = s.save(d); err != nil {
			return Account{}, err
		}
		delete(s.mediaReady, current.ID)
		delete(s.modelFailures, current.ID)
		if trustMedia {
			s.recordMediaReadyLocked(readyCtx, current)
		}
		return current, nil
	}
	return Account{}, ErrNotFound
}

func (s *Store) SetEnabled(ids []string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return err
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	for i := range d.Accounts {
		if set[d.Accounts[i].ID] {
			d.Accounts[i].Enabled = enabled
			d.Accounts[i].UpdatedAt = time.Now().Unix()
		}
	}
	if err := s.save(d); err != nil {
		return err
	}
	if !enabled {
		for id := range set {
			delete(s.mediaReady, id)
			delete(s.modelFailures, id)
		}
	}
	return nil
}
func (s *Store) Delete(ids []string) ([]Account, error) {
	deleted, _, err := s.DeleteWithCutoff(ids)
	return deleted, err
}

// DeleteWithCutoff captures mailbox cleanup ownership time under the account
// lock. A concurrent association must finish before this cutoff, while a later
// reimport/relink must not be released by this deletion's cleanup.
func (s *Store) DeleteWithCutoff(ids []string) ([]Account, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now()
	d, err := s.load()
	if err != nil {
		return nil, cutoff, err
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	keep := []Account{}
	deleted := []Account{}
	for _, a := range d.Accounts {
		if set[a.ID] {
			if a.InFlight != "" || s.maintenance[a.ID] || s.quotaQueries[a.ID] {
				return nil, cutoff, ErrBusy
			}
			deleted = append(deleted, a)
			delete(d.MediaFailures, a.ID)
		} else {
			keep = append(keep, a)
		}
	}
	d.Accounts = keep
	if err = s.save(d); err != nil {
		return nil, cutoff, err
	}
	for _, a := range deleted {
		delete(s.mediaReady, a.ID)
		delete(s.modelFailures, a.ID)
	}
	return deleted, cutoff, nil
}

// Relogin always runs the normal browser login/session initialization. Unknown
// submissions may renew authentication, but their reservation and task binding
// remain pinned; this operation never submits, releases or resumes generation.
func (s *Store) Relogin(ctx context.Context, id string) (Account, error) {
	return s.relogin(ctx, id, nil)
}

// A queued task must not log into credentials replaced after its admission.
func (s *Store) ReloginRevision(ctx context.Context, id string, revision uint64) (Account, error) {
	return s.relogin(ctx, id, &revision)
}

func (s *Store) relogin(ctx context.Context, id string, revision *uint64) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, &Error{Stage: "relogin", Code: "cancelled"}
	}
	s.mu.Lock()
	d, err := s.load()
	if err != nil {
		s.mu.Unlock()
		return Account{}, err
	}
	var a Account
	for _, candidate := range d.Accounts {
		if candidate.ID == id {
			a = candidate
			break
		}
	}
	if a.ID == "" {
		s.mu.Unlock()
		return a, ErrNotFound
	}
	if revision != nil && a.Revision != *revision {
		s.mu.Unlock()
		return a, ErrRevision
	}
	if (a.InFlight != "" && a.Status != "submission_unknown") || a.SettlementUntil > time.Now().Unix() || s.maintenance[id] {
		s.mu.Unlock()
		return a, ErrBusy
	}
	browserRefresher := s.browserRefresher
	if browserRefresher == nil {
		s.mu.Unlock()
		return a, &Error{Stage: "relogin", Code: "browser_unavailable"}
	}
	s.maintenance[id] = true
	s.mu.Unlock()
	defer s.endAccountMaintenance(id)
	refreshed, err := browserRefresher(ctx, a)
	if err != nil {
		return a, err
	}
	if ctx.Err() != nil {
		return a, &Error{Stage: "relogin", Code: "cancelled"}
	}
	updated, err := s.replaceCredentialsWithContext(a, refreshed, true, nil, ctx)
	if err != nil {
		return a, err
	}
	return updated, nil
}

// Refresh performs bounded maintenance before selection or after settlement;
// it never refreshes and replays a failed generation request.
func (s *Store) Refresh(ctx context.Context, c *Client, id string, credentials bool) (Account, error) {
	return s.refresh(ctx, c, id, credentials, false, nil)
}

// RefreshWithPermissions keeps both balance and generation-permission reads
// behind the same maintenance gate used by preparation and account selection.
func (s *Store) RefreshWithPermissions(ctx context.Context, c *Client, id string, credentials bool) (Account, error) {
	return s.refresh(ctx, c, id, credentials, true, nil)
}

type maintenanceDueGuard struct {
	now       time.Time
	attempted bool
	model     string
}

func MaintenanceDue(a Account, now time.Time) bool {
	if !a.Enabled || a.InFlight != "" || a.SettlementUntil > now.Unix() || (a.NextRefreshAt > now.Unix() && a.LastError != "") {
		return false
	}
	return !QuotaRefreshDueAt(a, now).After(now)
}

// QuotaRefreshDueAt orders maintenance fairly; the caller still checks enabled,
// in-flight and settlement state through MaintenanceDue before starting work.
func QuotaRefreshDueAt(a Account, now time.Time) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, a.Quota.CheckedAt)
	interval := time.Hour
	if a.LastUsedAt > now.Add(-time.Hour).Unix() {
		interval = 5 * time.Minute
	}
	due := at.Add(interval)
	if a.Quota.Status == "stale" || at.IsZero() {
		due = time.Time{}
	}
	if tokenDue := time.Unix(a.ExpiresAt, 0).Add(-5 * time.Minute); tokenDue.Before(due) {
		due = tokenDue
	}
	if retry := time.Unix(a.NextRefreshAt, 0); a.LastError != "" && retry.After(due) {
		due = retry
	}
	return due
}

// RefreshDueWithPermissions rechecks the scheduler's due decision while holding
// the same account gate as requests, and reports whether network work began.
func (s *Store) RefreshDueWithPermissions(ctx context.Context, c *Client, id string, now time.Time) (Account, bool, error) {
	guard := &maintenanceDueGuard{now: now}
	a, err := s.refresh(ctx, c, id, false, true, guard)
	return a, guard.attempted, err
}

// RefreshModelPermissions is request-driven and independent of background
// maintenance settings. Concurrent readers share the existing account gate;
// once admitted the latest model observation is checked again before any read.
func (s *Store) RefreshModelPermissions(ctx context.Context, c *Client, id, model string) (Account, bool, error) {
	guard := &maintenanceDueGuard{model: model, now: time.Now()}
	a, err := s.refresh(ctx, c, id, false, true, guard)
	if errors.Is(err, ErrBusy) {
		s.mu.Lock()
		var signal <-chan struct{}
		if s.maintenance[id] || s.quotaQueries[id] {
			signal = s.accountChangeSignalLocked(id)
		}
		s.mu.Unlock()
		if signal != nil {
			if waitErr := waitMediaSignal(ctx, signal); waitErr != nil {
				return a, false, waitErr
			}
		}
		a, err = s.refresh(ctx, c, id, false, true, guard)
	}
	if err == nil && a.Quota.Generation.ModelSelectionState(model, time.Now()) == ModelNeedsRefresh {
		err = &Error{Stage: "entitlements", Code: "observation_incomplete"}
	}
	return a, guard.attempted, err
}

func (s *Store) refresh(ctx context.Context, c *Client, id string, credentials, includePermissions bool, due *maintenanceDueGuard) (Account, error) {
	s.mu.Lock()
	d, err := s.load()
	if err != nil {
		s.mu.Unlock()
		return Account{}, err
	}
	var a Account
	for _, x := range d.Accounts {
		if x.ID == id {
			a = x
			break
		}
	}
	if a.ID == "" {
		s.mu.Unlock()
		return a, ErrNotFound
	}
	if a.InFlight != "" || a.SettlementUntil > time.Now().Unix() || s.maintenance[id] || s.quotaQueries[id] {
		s.mu.Unlock()
		return a, ErrBusy
	}
	if due != nil {
		needed := MaintenanceDue(a, due.now)
		if due.model != "" {
			if !a.Enabled || a.Status == "invalid" || (a.NextRefreshAt > time.Now().Unix() && a.LastError != "") {
				s.mu.Unlock()
				return a, ErrBusy
			}
			state := a.Quota.Generation.ModelSelectionState(due.model, time.Now())
			needed = state == ModelNeedsRefresh || (state == ModelUsable && !Eligible(a, time.Now()))
			if needed && s.modelPermissionsCoolingLocked(ctx, a, time.Now()) {
				s.mu.Unlock()
				return a, &Error{Stage: "entitlements", Code: "observation_incomplete"}
			}
		}
		if !needed {
			s.mu.Unlock()
			return a, nil
		}
	}
	if ctx.Err() != nil {
		s.mu.Unlock()
		return a, &Error{Stage: "refresh", Code: "cancelled"}
	}
	if due != nil {
		due.attempted = true
	}
	s.maintenance[id] = true
	browserRefresher := s.browserRefresher
	s.mu.Unlock()
	defer s.endAccountMaintenance(id)
	refreshedOnce := false
	refreshCredentials := func() error {
		refreshedOnce = true
		browserVerified := false
		refreshed, e := c.Refresh(ctx, a)
		if e != nil && a.ProfileID != "" && browserRefresher != nil {
			browserVerified = true
			refreshed, e = browserRefresher(ctx, a)
		}
		if e == nil {
			updated, saveErr := s.replaceCredentialsWithContext(a, refreshed, browserVerified, nil, ctx)
			e = saveErr
			if e == nil {
				a = updated
			}
		}
		if e != nil {
			_ = s.update(a.ID, a.Revision, func(p *Account) {
				p.LastError = SafeError(e)
				p.NextRefreshAt = time.Now().Add(15 * time.Minute).Unix()
			})
		}
		return e
	}
	if credentials || a.ExpiresAt <= time.Now().Add(5*time.Minute).Unix() {
		if err = refreshCredentials(); err != nil {
			return a, err
		}
	}
	q, e := c.Balance(ctx, a)
	var upstream *Error
	if e != nil && !refreshedOnce && errors.As(e, &upstream) && upstream.Status == http.StatusUnauthorized {
		if refreshErr := refreshCredentials(); refreshErr != nil {
			return a, refreshErr
		}
		q, e = c.Balance(ctx, a)
	}
	if e != nil {
		q = a.Quota
		q.Status = "error"
		q.Error = SafeError(e)
		q.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	} else {
		// General credits and model pools are independent observations. Keep
		// the previous model snapshot until the guarded permissions read ends.
		q.Generation = a.Quota.Generation
	}
	if ue := s.UpdateQuota(a.ID, a.Revision, q); ue != nil {
		return a, ue
	}
	a.Quota = q
	if e == nil && includePermissions {
		generation, generationErr := c.GenerationPermissions(ctx, a)
		if generationErr != nil {
			generation.Status, generation.Error = "error", SafeError(generationErr)
		}
		if saveErr := s.UpdateGenerationPermissions(a.ID, a.Revision, generation); saveErr != nil {
			return a, saveErr
		}
		targetModel := ""
		if due != nil {
			targetModel = due.model
		}
		s.recordModelPermissionObservation(ctx, a, generation, targetModel)
		return s.Get(a.ID)
	}
	return a, e
}
func (s *Store) EnsureAvailable(ctx context.Context, c *Client) error {
	items, err := s.List()
	if err != nil {
		return err
	}
	for _, a := range items {
		if Eligible(a, time.Now()) {
			return nil
		}
	}
	attempts := 0
	for _, a := range items {
		if !a.Enabled || a.InFlight != "" || a.SettlementUntil > time.Now().Unix() || a.NextRefreshAt > time.Now().Unix() && a.LastError != "" {
			continue
		}
		attempts++
		fresh, e := s.Refresh(ctx, c, a.ID, false)
		if e == nil && Eligible(fresh, time.Now()) {
			return nil
		}
		if attempts >= 4 {
			break
		}
	}
	return ErrNoAccount
}
func (s *Store) jobPath(taskID string) string {
	sum := sha256.Sum256([]byte(taskID))
	return filepath.Join(filepath.Dir(s.path), "adobe_jobs", hex.EncodeToString(sum[:])+".json.enc")
}
func (s *Store) SaveJob(taskID string, job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return err
	}
	if _, resolved := d.Resolutions[taskID]; resolved {
		return ErrTaskResolved
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	raw, err = Seal(s.secret, raw)
	if err != nil {
		return err
	}
	return atomicWrite(s.jobPath(taskID), raw)
}
func (s *Store) LoadJob(taskID string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadJobLocked(taskID)
}
func (s *Store) loadJobLocked(taskID string) (Job, error) {
	var j Job
	raw, err := os.ReadFile(s.jobPath(taskID))
	if err != nil {
		return j, err
	}
	raw, err = Open(s.secret, raw)
	if err != nil {
		return j, errors.New("adobe_job_decryption_failed")
	}
	err = json.Unmarshal(raw, &j)
	return j, err
}

// Resume can only recover an existing durable route; it never chooses an account.
// A persisted upstream handle is required, so it cannot recover by resubmission.
func (s *Store) Resume(taskID, accountID string) (Account, error) {
	job, err := s.LoadJob(taskID)
	if err != nil || job.PollURL == "" {
		return Account{}, errors.New("adobe_resume_handle_required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return Account{}, err
	}
	if d.Bindings[taskID] != accountID {
		return Account{}, errors.New("adobe_resume_binding_mismatch")
	}
	if _, resolved := d.Resolutions[taskID]; resolved {
		return Account{}, ErrTaskResolved
	}
	for i, a := range d.Accounts {
		if a.ID != accountID {
			continue
		}
		if a.InFlight != "" && a.InFlight != taskID || s.maintenance[a.ID] {
			return Account{}, ErrBusy
		}
		a.InFlight = taskID
		d.Accounts[i] = a
		if err = s.save(d); err != nil {
			return Account{}, err
		}
		return a, nil
	}
	return Account{}, ErrNotFound
}

func (s *Store) SetBrowserRefresher(f func(context.Context, Account) (Account, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.browserRefresher = f
}

// RefreshTask renews only the identity of a pinned upstream job. It cannot select
// a different account, clear its reservation, reconcile quota or submit media.
func (s *Store) RefreshTask(ctx context.Context, c *Client, taskID, accountID string) (Account, error) {
	job, err := s.LoadJob(taskID)
	if err != nil || job.PollURL == "" {
		return Account{}, errors.New("adobe_resume_handle_required")
	}
	s.mu.Lock()
	d, err := s.load()
	if err != nil {
		s.mu.Unlock()
		return Account{}, err
	}
	if d.Bindings[taskID] != accountID {
		s.mu.Unlock()
		return Account{}, errors.New("adobe_resume_binding_mismatch")
	}
	if _, resolved := d.Resolutions[taskID]; resolved {
		s.mu.Unlock()
		return Account{}, ErrTaskResolved
	}
	var a Account
	for _, v := range d.Accounts {
		if v.ID == accountID {
			a = v
			break
		}
	}
	if a.ID == "" {
		s.mu.Unlock()
		return a, ErrNotFound
	}
	if a.InFlight != taskID || s.maintenance[accountID] {
		s.mu.Unlock()
		return a, ErrBusy
	}
	s.maintenance[accountID] = true
	browserRefresher := s.browserRefresher
	s.mu.Unlock()
	defer s.endAccountMaintenance(accountID)
	browserVerified := false
	refreshed, err := c.Refresh(ctx, a)
	if err != nil && a.ProfileID != "" && browserRefresher != nil {
		browserVerified = true
		refreshed, err = browserRefresher(ctx, a)
	}
	if err == nil {
		updated, e := s.replaceCredentialsWithContext(a, refreshed, browserVerified, nil, ctx)
		err = e
		if e == nil {
			a = updated
		}
	}
	if err != nil {
		_ = s.update(a.ID, a.Revision, func(p *Account) {
			p.LastError = SafeError(err)
			p.NextRefreshAt = time.Now().Add(15 * time.Minute).Unix()
		})
	}
	return a, err
}
