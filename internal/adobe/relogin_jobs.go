package adobe

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"
)

// ReloginTask contains progress only, never browser credentials or task handles.
type ReloginTask struct {
	ProgressID         string `json:"progress_id"`
	RequestID          string `json:"request_id"`
	AccountRef         string `json:"account_ref"`
	Email              string `json:"email,omitempty"`
	Stage              string `json:"stage"`
	StartedAt          string `json:"started_at"`
	UpdatedAt          string `json:"updated_at"`
	FinishedAt         string `json:"finished_at,omitempty"`
	Done               bool   `json:"done"`
	Error              string `json:"error,omitempty"`
	SessionUpdated     bool   `json:"session_updated"`
	SubmissionUnknown  bool   `json:"submission_unknown"`
	ResultUnknown      bool   `json:"result_unknown,omitempty"`
	CredentialRevision uint64 `json:"credential_revision"`
}
type reloginRequest struct {
	IDs   []string `json:"ids"`
	Tasks []string `json:"tasks"`
}
type reloginJournal struct {
	Tasks    []ReloginTask             `json:"tasks"`
	Requests map[string]reloginRequest `json:"requests"`
}
type ReloginJobs struct {
	mu           sync.Mutex
	path, secret string
	recovered    bool
	writeErr     error
}

func NewReloginJobs(path, secret string) *ReloginJobs {
	return &ReloginJobs{path: path, secret: secret}
}
func (j *ReloginJobs) load() (reloginJournal, error) {
	state := reloginJournal{Tasks: []ReloginTask{}, Requests: map[string]reloginRequest{}}
	raw, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	raw, err = Open(j.secret, raw)
	if err != nil {
		return state, errors.New("adobe_relogin_journal_invalid")
	}
	if json.Unmarshal(raw, &state) != nil || state.Requests == nil {
		return state, errors.New("adobe_relogin_journal_invalid")
	}
	return state, nil
}
func (j *ReloginJobs) save(state reloginJournal) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	raw, err = Seal(j.secret, raw)
	if err == nil {
		err = atomicWrite(j.path, raw)
	}
	if err != nil {
		j.writeErr = err
	}
	return err
}

// Recovery runs only when this process owns the deployment data lease. A
// standby can inspect the journal without rewriting another writer's progress.
func (j *ReloginJobs) RecoverInterrupted() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.recovered {
		return j.writeErr
	}
	state, err := j.load()
	if err != nil {
		return err
	}
	changed := false
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range state.Tasks {
		task := &state.Tasks[i]
		if !task.Done {
			task.Stage = "interrupted"
			task.Done = true
			task.ResultUnknown = true
			task.Error = "adobe_relogin_interrupted"
			task.UpdatedAt = stamp
			task.FinishedAt = stamp
			changed = true
		}
	}
	if changed {
		if err = j.save(state); err != nil {
			return err
		}
	}
	j.recovered = true
	return nil
}
func (j *ReloginJobs) List(requestID string) ([]ReloginTask, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.writeErr != nil {
		return nil, j.writeErr
	}
	state, err := j.load()
	if err != nil {
		return nil, err
	}
	selected := map[string]bool{}
	if requestID != "" {
		for _, id := range state.Requests[requestID].Tasks {
			selected[id] = true
		}
	}
	out := []ReloginTask{}
	for _, task := range state.Tasks {
		if requestID == "" || selected[task.ProgressID] {
			out = append(out, task)
		}
	}
	sort.SliceStable(out, func(i, k int) bool {
		if out[i].Done != out[k].Done {
			return !out[i].Done
		}
		return out[i].StartedAt > out[k].StartedAt
	})
	return out, nil
}
func (j *ReloginJobs) Start(requestID string, ids []string, store *Store) ([]ReloginTask, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.writeErr != nil {
		return nil, j.writeErr
	}
	if !j.recovered {
		return nil, &Error{Stage: "relogin", Code: "not_ready"}
	}
	state, err := j.load()
	if err != nil {
		return nil, err
	}
	if previous, ok := state.Requests[requestID]; ok {
		if !reflect.DeepEqual(previous.IDs, ids) {
			return nil, &Error{Stage: "relogin", Code: "request_conflict"}
		}
		out := []ReloginTask{}
		for _, id := range previous.Tasks {
			for _, task := range state.Tasks {
				if task.ProgressID == id {
					out = append(out, task)
					break
				}
			}
		}
		return out, nil
	}
	accepted := reloginRequest{IDs: append([]string{}, ids...), Tasks: []string{}}
	out := []ReloginTask{}
	for _, id := range ids {
		var active *ReloginTask
		for i := range state.Tasks {
			task := &state.Tasks[i]
			if !task.Done && task.AccountRef == id {
				active = task
				break
			}
		}
		if active != nil {
			out = append(out, *active)
			accepted.Tasks = append(accepted.Tasks, active.ProgressID)
			continue
		}
		random := make([]byte, 16)
		if _, err = rand.Read(random); err != nil {
			return nil, err
		}
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		task := ReloginTask{ProgressID: hex.EncodeToString(random), RequestID: requestID, AccountRef: id, Stage: "queued", StartedAt: stamp, UpdatedAt: stamp}
		account, e := store.Get(id)
		if e == nil {
			task.Email = account.Email
			task.CredentialRevision = account.Revision
			task.SubmissionUnknown = account.Status == "submission_unknown"
		}
		if e == nil && ((account.InFlight != "" && account.Status != "submission_unknown") || account.SettlementUntil > time.Now().Unix()) {
			e = ErrBusy
		}
		if e != nil {
			task.Stage = "failed"
			task.Done = true
			task.Error = SafeError(e)
			task.FinishedAt = stamp
		}
		state.Tasks = append(state.Tasks, task)
		accepted.Tasks = append(accepted.Tasks, task.ProgressID)
		out = append(out, task)
	}
	state.Requests[requestID] = accepted
	if err = j.save(state); err != nil {
		return nil, err
	}
	return out, nil
}
func (j *ReloginJobs) Next() (ReloginTask, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.writeErr != nil {
		return ReloginTask{}, false, j.writeErr
	}
	state, err := j.load()
	if err != nil {
		return ReloginTask{}, false, err
	}
	for i := range state.Tasks {
		if state.Tasks[i].Stage == "queued" && !state.Tasks[i].Done {
			state.Tasks[i].Stage = "running"
			state.Tasks[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err = j.save(state); err != nil {
				return ReloginTask{}, false, err
			}
			return state.Tasks[i], true, nil
		}
	}
	return ReloginTask{}, false, nil
}
func (j *ReloginJobs) Finish(id string, account Account, failure error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.writeErr != nil {
		return j.writeErr
	}
	state, err := j.load()
	if err != nil {
		return err
	}
	for i := range state.Tasks {
		task := &state.Tasks[i]
		if task.ProgressID != id {
			continue
		}
		if task.Done {
			return nil
		}
		task.Done = true
		task.Stage = "completed"
		task.SessionUpdated = failure == nil
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		task.FinishedAt = task.UpdatedAt
		if account.ID != "" {
			task.SubmissionUnknown = account.Status == "submission_unknown"
		}
		if failure != nil {
			task.Stage = "failed"
			task.Error = SafeError(failure)
			task.ResultUnknown = task.Error == "adobe_relogin_result_unknown" || task.Error == "adobe_relogin_cancelled"
		}
		return j.save(state)
	}
	return ErrNotFound
}
