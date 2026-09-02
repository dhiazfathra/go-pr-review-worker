// Package store owns all durable state: the FIFO job queue and the per-PR
// review budget. It is the single source of truth for both, so a worker
// restart never loses a queued job nor resets a PR's review cycle count.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, keeps the static single binary
)

// ErrNoJob is returned by ClaimNext when the queue holds no runnable job.
var ErrNoJob = errors.New("no runnable job")

// JobState is the lifecycle state of a queued review job.
type JobState string

const (
	StateQueued  JobState = "queued"
	StateRunning JobState = "running"
	StateDone    JobState = "done"
	StateFailed  JobState = "failed" // retryable
	StateDead    JobState = "dead"   // dead-lettered, no further attempts
)

// Event is the provider webhook event that produced a job.
type Event string

const (
	EventOpened      Event = "opened"
	EventReopened    Event = "reopened"
	EventSynchronize Event = "synchronize"
)

// Job is one review request for one PR at one head SHA.
type Job struct {
	ID         int64
	DeliveryID string // provider delivery id, unique — the idempotency key
	Provider   string // "github" | "gitlab"
	Repo       string // "owner/name" for GitHub, project path for GitLab
	PRNumber   int    // PR number / MR iid
	HeadSHA    string
	BaseSHA    string
	Event      Event
	Attempts   int
}

// PRKey identifies a pull request across restarts and webhook deliveries.
func (j Job) PRKey() string {
	return fmt.Sprintf("%s:%s#%d", j.Provider, j.Repo, j.PRNumber)
}

// PRState is the persisted review budget for one PR.
type PRState struct {
	Cycle              int    // completed review cycles
	LastReviewedSHA    string // head SHA of the last completed cycle
	SummaryCommentID   string // provider id of the summary comment, for in-place updates
	SummaryCycle       int    // cycle that posted SummaryCommentID
	BudgetNoticePosted bool
	// Approved records that the worker already submitted an approving review,
	// so a later pass over the same PR cannot stack a second one.
	Approved bool
}

// Store is a SQLite-backed queue and review-budget store.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    delivery_id TEXT    NOT NULL UNIQUE,
    provider    TEXT    NOT NULL,
    repo        TEXT    NOT NULL,
    pr_number   INTEGER NOT NULL,
    head_sha    TEXT    NOT NULL,
    base_sha    TEXT    NOT NULL DEFAULT '',
    event       TEXT    NOT NULL,
    state       TEXT    NOT NULL DEFAULT 'queued',
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_runnable ON jobs(state, id);

CREATE TABLE IF NOT EXISTS pr_reviews (
    pr_key               TEXT    PRIMARY KEY,
    cycle                INTEGER NOT NULL DEFAULT 0,
    last_reviewed_sha    TEXT    NOT NULL DEFAULT '',
    summary_comment_id   TEXT    NOT NULL DEFAULT '',
    summary_cycle        INTEGER NOT NULL DEFAULT 0,
    budget_notice_posted INTEGER NOT NULL DEFAULT 0,
    approved             INTEGER NOT NULL DEFAULT 0,
    updated_at           TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS posted_comments (
    pr_key      TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (pr_key, fingerprint)
);
`

// Open opens (creating if needed) the SQLite database at path and applies the
// schema. The pragmas match the single-worker design: WAL so webhook writes
// never block on the worker, busy_timeout so a write waits instead of failing.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite %q: %w", path, err)
	}

	// One writer by design; more connections only invite SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("applying schema: %w", err)
	}

	if err := migrate(db); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

// migrate adds columns that a database created by an earlier version does not
// have. `CREATE TABLE IF NOT EXISTS` is a no-op on an existing table, so a new
// column in `schema` never reaches a deployed database without this.
func migrate(db *sql.DB) error {
	// Each entry is a column that must exist on pr_reviews. SQLite has no
	// "ADD COLUMN IF NOT EXISTS", so existence is checked first.
	columns := []struct{ name, ddl string }{
		{"approved", "ALTER TABLE pr_reviews ADD COLUMN approved INTEGER NOT NULL DEFAULT 0"},
	}

	for _, c := range columns {
		var count int

		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('pr_reviews') WHERE name = ?`, c.name,
		).Scan(&count); err != nil {
			return fmt.Errorf("checking pr_reviews.%s: %w", c.name, err)
		}

		if count > 0 {
			continue
		}

		if _, err := db.Exec(c.ddl); err != nil {
			return fmt.Errorf("adding pr_reviews.%s: %w", c.name, err)
		}
	}

	return nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// Enqueue appends a job. A repeated delivery ID (webhook redelivery) is a
// no-op and reports enqueued=false, so a retried delivery can never consume a
// review cycle twice.
func (s *Store) Enqueue(ctx context.Context, j Job) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO jobs
		    (delivery_id, provider, repo, pr_number, head_sha, base_sha, event, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.DeliveryID,
		j.Provider,
		j.Repo,
		j.PRNumber,
		j.HeadSHA,
		j.BaseSHA,
		string(j.Event),
		now(),
		now(),
	)
	if err != nil {
		return false, fmt.Errorf("enqueueing job %q: %w", j.DeliveryID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting inserted rows: %w", err)
	}

	return n > 0, nil
}

// ClaimNext marks the oldest runnable job as running and returns it, giving
// strict FIFO order across every repository. A job left in 'running' by a
// crash is runnable again, which is safe because posting is fingerprint-deduped.
// Returns ErrNoJob when the queue is idle.
func (s *Store) ClaimNext(ctx context.Context) (Job, error) {
	var j Job

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return j, fmt.Errorf("beginning claim transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var event string

	row := tx.QueryRowContext(ctx, `
		SELECT id, delivery_id, provider, repo, pr_number, head_sha, base_sha, event, attempts
		FROM jobs
		WHERE state IN ('queued', 'running', 'failed')
		ORDER BY id
		LIMIT 1`)

	switch err := row.Scan(
		&j.ID,
		&j.DeliveryID,
		&j.Provider,
		&j.Repo,
		&j.PRNumber,
		&j.HeadSHA,
		&j.BaseSHA,
		&event,
		&j.Attempts,
	); {
	case errors.Is(err, sql.ErrNoRows):
		return j, ErrNoJob
	case err != nil:
		return j, fmt.Errorf("scanning next job: %w", err)
	}

	j.Event = Event(event)
	j.Attempts++

	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET state = 'running', attempts = ?, updated_at = ? WHERE id = ?`,
		j.Attempts,
		now(),
		j.ID,
	); err != nil {
		return j, fmt.Errorf("marking job %d running: %w", j.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return j, fmt.Errorf("committing claim of job %d: %w", j.ID, err)
	}

	return j, nil
}

// Finish records the terminal (or retryable) outcome of a job.
func (s *Store) Finish(ctx context.Context, id int64, state JobState, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		string(state),
		msg,
		now(),
		id,
	); err != nil {
		return fmt.Errorf("finishing job %d as %s: %w", id, state, err)
	}

	return nil
}

// PendingCount reports how many jobs are still runnable, for the health endpoint.
func (s *Store) PendingCount(ctx context.Context) (int, error) {
	var n int

	row := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE state IN ('queued', 'running', 'failed')`)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("counting pending jobs: %w", err)
	}

	return n, nil
}

// PRState reads the persisted review budget for a PR. An unknown PR reads back
// as the zero value, which means "no cycle used yet".
func (s *Store) PRState(ctx context.Context, prKey string) (PRState, error) {
	var (
		st       PRState
		notice   int
		approved int
	)

	row := s.db.QueryRowContext(ctx, `
		SELECT cycle, last_reviewed_sha, summary_comment_id, summary_cycle, budget_notice_posted, approved
		FROM pr_reviews WHERE pr_key = ?`, prKey)

	switch err := row.Scan(
		&st.Cycle,
		&st.LastReviewedSHA,
		&st.SummaryCommentID,
		&st.SummaryCycle,
		&notice,
		&approved,
	); {
	case errors.Is(err, sql.ErrNoRows):
		return PRState{}, nil
	case err != nil:
		return st, fmt.Errorf("reading pr state %q: %w", prKey, err)
	}

	st.BudgetNoticePosted = notice != 0
	st.Approved = approved != 0

	return st, nil
}

// SavePRState upserts the review budget for a PR.
func (s *Store) SavePRState(ctx context.Context, prKey string, st PRState) error {
	notice := 0
	if st.BudgetNoticePosted {
		notice = 1
	}

	approved := 0
	if st.Approved {
		approved = 1
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO pr_reviews
		    (pr_key, cycle, last_reviewed_sha, summary_comment_id, summary_cycle,
		     budget_notice_posted, approved, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pr_key) DO UPDATE SET
		    cycle = excluded.cycle,
		    last_reviewed_sha = excluded.last_reviewed_sha,
		    summary_comment_id = excluded.summary_comment_id,
		    summary_cycle = excluded.summary_cycle,
		    budget_notice_posted = excluded.budget_notice_posted,
		    approved = excluded.approved,
		    updated_at = excluded.updated_at`,
		prKey,
		st.Cycle,
		st.LastReviewedSHA,
		st.SummaryCommentID,
		st.SummaryCycle,
		notice,
		approved,
		now(),
	); err != nil {
		return fmt.Errorf("saving pr state %q: %w", prKey, err)
	}

	return nil
}

// ResetPRState deletes the review budget and posted-fingerprint history for a
// PR, so a reopen starts a fresh set of review cycles instead of picking up
// where the closed PR left off.
func (s *Store) ResetPRState(ctx context.Context, prKey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("resetting pr state %q: %w", prKey, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM pr_reviews WHERE pr_key = ?`, prKey); err != nil {
		return fmt.Errorf("resetting pr state %q: %w", prKey, err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM posted_comments WHERE pr_key = ?`, prKey); err != nil {
		return fmt.Errorf("resetting pr state %q: %w", prKey, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("resetting pr state %q: %w", prKey, err)
	}

	return nil
}

// UnseenFingerprints returns the subset of fingerprints never posted for this
// PR, in the order given. It records nothing: a fingerprint must only be marked
// posted once the provider has actually accepted the comment, otherwise a
// transient API failure would suppress that finding forever (RecordFingerprint
// does the marking).
func (s *Store) UnseenFingerprints(ctx context.Context, prKey string, fingerprints []string) ([]string, error) {
	if len(fingerprints) == 0 {
		return []string{}, nil
	}

	// One query for the whole batch: a per-fingerprint round-trip is wasted
	// work even at the per-pass comment cap.
	args := make([]any, 0, len(fingerprints)+1)
	args = append(args, prKey)

	for _, fp := range fingerprints {
		args = append(args, fp)
	}

	query := `SELECT fingerprint FROM posted_comments WHERE pr_key = ? AND fingerprint IN (?` +
		strings.Repeat(", ?", len(fingerprints)-1) + `)`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading comment fingerprints for %q: %w", prKey, err)
	}
	defer func() { _ = rows.Close() }()

	seen := make(map[string]struct{}, len(fingerprints))

	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scanning comment fingerprint for %q: %w", prKey, err)
		}

		seen[fp] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating comment fingerprints for %q: %w", prKey, err)
	}

	unseen := make([]string, 0, len(fingerprints))

	for _, fp := range fingerprints {
		if _, ok := seen[fp]; !ok {
			unseen = append(unseen, fp)
		}
	}

	return unseen, nil
}

// RecordFingerprint marks a fingerprint as posted for this PR. Safe to call
// twice; the primary key absorbs the repeat.
func (s *Store) RecordFingerprint(ctx context.Context, prKey, fingerprint string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO posted_comments (pr_key, fingerprint, created_at) VALUES (?, ?, ?)`,
		prKey,
		fingerprint,
		now(),
	); err != nil {
		return fmt.Errorf("recording comment fingerprint for %q: %w", prKey, err)
	}

	return nil
}
