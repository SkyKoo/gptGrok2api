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
	Accounts []Account         `json:"accounts"`
	Bindings map[string]string `json:"bindings"`
}
type Store struct {
	browserRefresher func(context.Context, Account) (Account, error)
	path, secret     string
	mu               sync.Mutex
	maintenance      map[string]bool
}

func NewStore(path, secret string) *Store {
	return &Store{path: path, secret: secret, maintenance: map[string]bool{}}
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
		return *a, nil
	}
	return Account{}, ErrNotFound
}

func (s *Store) Import(items []Account) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return nil, err
	}
	result := make([]Account, 0, len(items))
	for _, a := range items {
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
		if _, e = Sherlock(a.Cookie); e != nil {
			return nil, e
		}
		a.ID = "adobe:" + a.AccountID
		a.Token = strings.TrimSpace(a.Token)
		a.ExpiresAt = expiry
		a.CreatedAt = time.Now().Unix()
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
	}
	if err = s.save(d); err != nil {
		return nil, err
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
	_, err = Sherlock(a.Cookie)
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
		a.UpdatedAt = time.Now().Unix()
		if unknown {
			a.Status = "submission_unknown"
			a.LastError = "adobe_submission_unknown"
		} else {
			a.InFlight = ""
			a.SettlementUntil = time.Now().Add(30 * time.Second).Unix()
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
	identity, expires, err := TokenIdentity(replacement.Token)
	if err != nil {
		return Account{}, err
	}
	if identity != expected.AccountID {
		return Account{}, ErrIdentity
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
		if current.Revision != expected.Revision {
			return Account{}, ErrRevision
		}
		if current.AccountID != identity {
			return Account{}, ErrIdentity
		}
		mediaCookie := replacement.Cookie
		mediaStatus := "refreshed"
		if _, e := Sherlock(mediaCookie); e != nil {
			if _, oldErr := Sherlock(current.Cookie); oldErr != nil {
				return Account{}, errors.New("adobe_sherlock_session_incomplete")
			}
			mediaCookie = current.Cookie
			mediaStatus = "preserved"
		} else if mediaCookie == current.Cookie {
			mediaStatus = "preserved"
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
		current.LastRefreshAt = time.Now().Unix()
		current.UpdatedAt = current.LastRefreshAt
		current.NextRefreshAt = time.Now().Add(45 * time.Minute).Unix()
		current.LastError = ""
		d.Accounts[i] = current
		if err = s.save(d); err != nil {
			return Account{}, err
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
	return s.save(d)
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
			if a.InFlight != "" || s.maintenance[a.ID] {
				return nil, cutoff, ErrBusy
			}
			deleted = append(deleted, a)
		} else {
			keep = append(keep, a)
		}
	}
	d.Accounts = keep
	if err = s.save(d); err != nil {
		return nil, cutoff, err
	}
	return deleted, cutoff, nil
}

// Refresh performs bounded maintenance before selection or after settlement;
// it never refreshes and replays a failed generation request.
func (s *Store) Refresh(ctx context.Context, c *Client, id string, credentials bool) (Account, error) {
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
	if a.InFlight != "" || a.SettlementUntil > time.Now().Unix() || s.maintenance[id] {
		s.mu.Unlock()
		return a, ErrBusy
	}
	s.maintenance[id] = true
	browserRefresher := s.browserRefresher
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.maintenance, id); s.mu.Unlock() }()
	refreshedOnce := false
	refreshCredentials := func() error {
		refreshedOnce = true
		refreshed, e := c.Refresh(ctx, a)
		if e != nil && a.ProfileID != "" && browserRefresher != nil {
			refreshed, e = browserRefresher(ctx, a)
		}
		if e == nil {
			updated, saveErr := s.ReplaceCredentials(a, refreshed)
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
	}
	if ue := s.UpdateQuota(a.ID, a.Revision, q); ue != nil {
		return a, ue
	}
	a.Quota = q
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
	defer func() { s.mu.Lock(); delete(s.maintenance, accountID); s.mu.Unlock() }()
	refreshed, err := c.Refresh(ctx, a)
	if err != nil && a.ProfileID != "" && browserRefresher != nil {
		refreshed, err = browserRefresher(ctx, a)
	}
	if err == nil {
		updated, e := s.ReplaceCredentials(a, refreshed)
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
