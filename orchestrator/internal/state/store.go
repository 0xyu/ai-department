// Package state is ACO's durable run state: a SQLite database (pure Go,
// WAL mode) holding runs, stages, phases, attempts, the intent/outcome step
// log, agent invocations, gate results, reviews, findings, commits, reports,
// events, and the per-repository run lock.
//
// Every status change goes through the run and phase state machines
// (machine.go) inside one transaction that also appends the matching events,
// so the stored status and the event log never disagree. Free text is masked
// with the store's secure.Redactor before it is written.
package state

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/i18n"
	"github.com/0xyu/aco/internal/secure"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver (pure Go)
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a run, phase, step, or record does not exist.
var ErrNotFound = errors.New("not found")

// ErrNewerSchema is returned when the database was written by a newer ACO.
var ErrNewerSchema = errors.New("state database has a newer schema than this ACO version")

// Options configures a Store.
type Options struct {
	// Redactor masks free text before it is stored. Nil masks credential
	// patterns only.
	Redactor *secure.Redactor
	// Lang is the operator language of event messages (default zh-CN).
	Lang i18n.Lang
	// Now returns the current time (default time.Now). Tests inject a clock.
	Now func() time.Time
	// BusyTimeout is how long a write waits for another process holding the
	// database (default 10s).
	BusyTimeout time.Duration
}

// Store is the state database. It is safe for concurrent use, also by
// several processes sharing the same file.
type Store struct {
	db   *sql.DB
	red  *secure.Redactor
	lang i18n.Lang
	now  func() time.Time

	mu        sync.RWMutex
	listeners []func(runID string, seq int64)
}

// Open opens (creating if needed) the database at path and applies pending
// migrations.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, errors.New("state: database path must be a plain file path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = 10 * time.Second
	}
	// synchronous(FULL): an intent is durable before the step acts, even on
	// power loss. _txlock=immediate: write transactions take the write lock
	// at BEGIN, so concurrent writers wait (busy_timeout) instead of failing
	// on a lock upgrade.
	dsn := path + "?_pragma=busy_timeout(" + strconv.FormatInt(busy.Milliseconds(), 10) + ")" +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	s := &Store{db: db, red: opts.Redactor, lang: opts.Lang, now: opts.Now}
	if s.lang == "" {
		s.lang = i18n.Default
	}
	if s.now == nil {
		s.now = time.Now
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// OnAppend registers fn, called after each committed event append. It
// implements events.Log.
func (s *Store) OnAppend(fn func(runID string, seq int64)) {
	s.mu.Lock()
	s.listeners = append(s.listeners, fn)
	s.mu.Unlock()
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("state: bad migration name %s", name)
		}
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("state: migrations must be numbered 1..n without gaps (%s)", m.name)
		}
	}
	return out, nil
}

// SchemaVersion is the newest schema this build knows.
func SchemaVersion() int {
	ms, err := loadMigrations()
	if err != nil {
		return 0
	}
	return len(ms)
}

func (s *Store) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	var cur int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&cur); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if cur > len(ms) {
		return fmt.Errorf("%w (database %d, supported %d)", ErrNewerSchema, cur, len(ms))
	}
	for _, m := range ms[cur:] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("state: migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, s.nowMS()); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	return nil
}

func (s *Store) nowMS() int64 { return s.now().UnixMilli() }

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func msPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := msTime(v.Int64)
	return &t
}

// mask applies the store's redactor to free text.
func (s *Store) mask(v string) string { return s.red.String(v) }

// maskJSON validates and masks a JSON document. Masking replaces a secret
// inside a JSON string with [REDACTED], which keeps the document valid.
func (s *Store) maskJSON(b []byte, empty string) (string, error) {
	if len(b) == 0 {
		return empty, nil
	}
	if !json.Valid(b) {
		return "", errors.New("state: invalid JSON document")
	}
	out := s.mask(string(b))
	if !json.Valid([]byte(out)) {
		return "", errors.New("state: JSON document invalid after masking")
	}
	return out, nil
}

// wtx is a write transaction that collects appended events and notifies
// listeners after commit.
type wtx struct {
	*sql.Tx
	s        *Store
	appended []events.Event
}

// write runs fn in an immediate write transaction.
func (s *Store) write(ctx context.Context, fn func(tx *wtx) error) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	tx := &wtx{Tx: t, s: s}
	if err := fn(tx); err != nil {
		t.Rollback()
		return err
	}
	if err := t.Commit(); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	s.mu.RLock()
	ls := append([]func(string, int64){}, s.listeners...)
	s.mu.RUnlock()
	for _, e := range tx.appended {
		for _, fn := range ls {
			fn(e.RunID, e.Seq)
		}
	}
	return nil
}

var (
	runIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)
	nameRE  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	keyRE   = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,200}$`)
)

func checkRunID(id string) error {
	if !runIDRE.MatchString(id) {
		return errors.New("state: run ID must be 3-64 characters of a-z, 0-9, and -")
	}
	return nil
}
