package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const HistoryLimit = 20

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type InstanceState string

const (
	StateStopped  InstanceState = "stopped"
	StateStarting InstanceState = "starting"
	StateRunning  InstanceState = "running"
	StateStopping InstanceState = "stopping"
	StateError    InstanceState = "error"
)

type Instance struct {
	ID             string
	UserID         string
	Name           string
	State          InstanceState
	ContainerID    string
	SelectedRepo   string
	LastError      string
	CreatedAt      time.Time
	LastActivityAt time.Time
}

type RunStatus string

const (
	RunRunning     RunStatus = "running"
	RunSucceeded   RunStatus = "succeeded"
	RunFailed      RunStatus = "failed"
	RunCancelled   RunStatus = "cancelled"
	RunTimedOut    RunStatus = "timed_out"
	RunInterrupted RunStatus = "interrupted"
)

type Run struct {
	ID           string
	InstanceID   string
	Agent        string
	Prompt       string
	Response     string
	Status       RunStatus
	SessionID    string
	Repo         string
	Model        string
	ErrorCode    string
	ErrorMessage string
	StartedAt    time.Time
	FinishedAt   time.Time
	DurationMS   int64
}

type ConnectionStatus string

const (
	ConnConnected     ConnectionStatus = "connected"
	ConnDisconnected  ConnectionStatus = "disconnected"
	ConnExpired       ConnectionStatus = "expired"
	ConnError         ConnectionStatus = "error"
	ConnSetupRequired ConnectionStatus = "setup_required"
)

type Connection struct {
	UserID    string
	Provider  string
	Status    ConnectionStatus
	Method    string
	Account   string
	Secret    string
	Error     string
	UpdatedAt time.Time
}

type OAuthState struct {
	StateHash string
	UserID    string
	Verifier  string
	ExpiresAt time.Time
}

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	return OpenPath(ctx, filepath.Join(dataDir, "gateway.db"))
}

func OpenPath(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, m := range migrations {
		if _, err := db.ExecContext(ctx, m); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	if path != ":memory:" {
		_ = os.Chmod(path, 0o600)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTS(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

func (s *Store) EnsureUser(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id, created_at) VALUES(?, ?) ON CONFLICT(id) DO NOTHING`, id, ts(now))
	return err
}

func (s *Store) CreateInstance(ctx context.Context, in Instance) (Instance, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Instance{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id, created_at) VALUES(?, ?) ON CONFLICT(id) DO NOTHING`, in.UserID, ts(in.CreatedAt)); err != nil {
		return Instance{}, err
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM instances WHERE user_id = ? LIMIT 1`, in.UserID).Scan(&existing)
	if err == nil {
		return Instance{ID: existing}, ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Instance{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO instances(id, user_id, name, state, created_at, last_activity_at) VALUES(?, ?, ?, ?, ?, ?)`,
		in.ID, in.UserID, in.Name, in.State, ts(in.CreatedAt), ts(in.LastActivityAt))
	if err != nil {
		return Instance{}, err
	}
	return in, tx.Commit()
}

const instanceCols = `id, user_id, name, state, container_id, selected_repo, last_error, created_at, last_activity_at`

func scanInstance(row interface{ Scan(...any) error }) (Instance, error) {
	var in Instance
	var created, activity string
	if err := row.Scan(&in.ID, &in.UserID, &in.Name, &in.State, &in.ContainerID, &in.SelectedRepo, &in.LastError, &created, &activity); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Instance{}, ErrNotFound
		}
		return Instance{}, err
	}
	in.CreatedAt, in.LastActivityAt = parseTS(created), parseTS(activity)
	return in, nil
}

func (s *Store) GetInstance(ctx context.Context, id string) (Instance, error) {
	return scanInstance(s.db.QueryRowContext(ctx, `SELECT `+instanceCols+` FROM instances WHERE id = ?`, id))
}

func (s *Store) InstanceByUser(ctx context.Context, userID string) (Instance, error) {
	return scanInstance(s.db.QueryRowContext(ctx, `SELECT `+instanceCols+` FROM instances WHERE user_id = ? ORDER BY created_at LIMIT 1`, userID))
}

func (s *Store) ListInstances(ctx context.Context, userID string) ([]Instance, error) {
	query := `SELECT ` + instanceCols + ` FROM instances`
	var args []any
	if userID != "" {
		query += ` WHERE user_id = ?`
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Instance{}
	for rows.Next() {
		in, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) CountInstancesInStates(ctx context.Context, states ...InstanceState) (int, error) {
	var n int
	for _, st := range states {
		var c int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM instances WHERE state = ?`, st).Scan(&c); err != nil {
			return 0, err
		}
		n += c
	}
	return n, nil
}

func (s *Store) SetInstanceState(ctx context.Context, id string, state InstanceState, containerID, lastError string) error {
	return s.exec1(ctx, `UPDATE instances SET state = ?, container_id = ?, last_error = ? WHERE id = ?`, state, containerID, lastError, id)
}

func (s *Store) TouchInstance(ctx context.Context, id string, at time.Time) error {
	return s.exec1(ctx, `UPDATE instances SET last_activity_at = ? WHERE id = ?`, ts(at), id)
}

func (s *Store) UpdateInstance(ctx context.Context, id string, name, selectedRepo *string) error {
	if name != nil {
		if err := s.exec1(ctx, `UPDATE instances SET name = ? WHERE id = ?`, *name, id); err != nil {
			return err
		}
	}
	if selectedRepo != nil {
		return s.exec1(ctx, `UPDATE instances SET selected_repo = ? WHERE id = ?`, *selectedRepo, id)
	}
	return nil
}

func (s *Store) DeleteInstance(ctx context.Context, id string) error {
	return s.exec1(ctx, `DELETE FROM instances WHERE id = ?`, id)
}

func (s *Store) exec1(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) InsertRun(ctx context.Context, r Run) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO runs(id, instance_id, agent, prompt, status, session_id, repo, model, started_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.InstanceID, r.Agent, r.Prompt, r.Status, r.SessionID, r.Repo, r.Model, ts(r.StartedAt))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM runs WHERE instance_id = ? AND seq NOT IN (SELECT seq FROM runs WHERE instance_id = ? ORDER BY seq DESC LIMIT ?)`,
		r.InstanceID, r.InstanceID, HistoryLimit)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishRun(ctx context.Context, r Run) error {
	err := s.exec1(ctx, `UPDATE runs SET response = ?, status = ?, session_id = ?, error_code = ?, error_message = ?, finished_at = ?, duration_ms = ? WHERE id = ?`,
		r.Response, r.Status, r.SessionID, r.ErrorCode, r.ErrorMessage, ts(r.FinishedAt), r.DurationMS, r.ID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

const runCols = `id, instance_id, agent, prompt, response, status, session_id, repo, model, error_code, error_message, started_at, finished_at, duration_ms`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var started, finished string
	if err := row.Scan(&r.ID, &r.InstanceID, &r.Agent, &r.Prompt, &r.Response, &r.Status, &r.SessionID, &r.Repo, &r.Model, &r.ErrorCode, &r.ErrorMessage, &started, &finished, &r.DurationMS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, ErrNotFound
		}
		return Run{}, err
	}
	r.StartedAt, r.FinishedAt = parseTS(started), parseTS(finished)
	return r, nil
}

func (s *Store) GetRun(ctx context.Context, instanceID, runID string) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ? AND instance_id = ?`, runID, instanceID))
}

func (s *Store) ListRuns(ctx context.Context, instanceID string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE instance_id = ? ORDER BY seq DESC LIMIT ?`, instanceID, HistoryLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) InterruptRunningRuns(ctx context.Context, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ?, error_code = 'gateway_restarted', error_message = 'The gateway restarted while this run was active', finished_at = ? WHERE status = ?`,
		RunInterrupted, ts(at), RunRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) GetConnection(ctx context.Context, userID, provider string) (Connection, error) {
	var c Connection
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT user_id, provider, status, method, account, secret, error, updated_at FROM connections WHERE user_id = ? AND provider = ?`, userID, provider).
		Scan(&c.UserID, &c.Provider, &c.Status, &c.Method, &c.Account, &c.Secret, &c.Error, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, ErrNotFound
	}
	c.UpdatedAt = parseTS(updated)
	return c, err
}

func (s *Store) ListConnections(ctx context.Context, userID string) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, provider, status, method, account, secret, error, updated_at FROM connections WHERE user_id = ? ORDER BY provider`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		var c Connection
		var updated string
		if err := rows.Scan(&c.UserID, &c.Provider, &c.Status, &c.Method, &c.Account, &c.Secret, &c.Error, &updated); err != nil {
			return nil, err
		}
		c.UpdatedAt = parseTS(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UpsertConnection(ctx context.Context, c Connection) error {
	if err := s.EnsureUser(ctx, c.UserID, c.UpdatedAt); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO connections(user_id, provider, status, method, account, secret, error, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, provider) DO UPDATE SET status = excluded.status, method = excluded.method, account = excluded.account, secret = excluded.secret, error = excluded.error, updated_at = excluded.updated_at`,
		c.UserID, c.Provider, c.Status, c.Method, c.Account, c.Secret, c.Error, ts(c.UpdatedAt))
	return err
}

func (s *Store) PutOAuthState(ctx context.Context, st OAuthState) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oauth_states WHERE expires_at < ?`, ts(time.Now())); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO oauth_states(state_hash, user_id, verifier, expires_at) VALUES(?, ?, ?, ?)`, st.StateHash, st.UserID, st.Verifier, ts(st.ExpiresAt))
	return err
}

func (s *Store) TakeOAuthState(ctx context.Context, stateHash string) (OAuthState, error) {
	var st OAuthState
	var exp string
	err := s.db.QueryRowContext(ctx, `DELETE FROM oauth_states WHERE state_hash = ? RETURNING state_hash, user_id, verifier, expires_at`, stateHash).
		Scan(&st.StateHash, &st.UserID, &st.Verifier, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthState{}, ErrNotFound
	}
	st.ExpiresAt = parseTS(exp)
	return st, err
}
