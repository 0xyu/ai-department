package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"
)

// DefaultLockTTL is how long a lock stays valid without a heartbeat.
const DefaultLockTTL = 30 * time.Second

// LockInfo describes a lock holder.
type LockInfo struct {
	RepoKey     string
	RunID       string
	Owner       string // random token of the holding process and acquisition
	PID         int
	Host        string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
	TTL         time.Duration
}

// Stale reports whether the holder missed its heartbeat deadline at now.
func (l LockInfo) Stale(now time.Time) bool { return now.After(l.HeartbeatAt.Add(l.TTL)) }

// LockedError is returned when another live holder has the repository.
type LockedError struct{ Holder LockInfo }

func (e *LockedError) Error() string {
	return fmt.Sprintf("repository is locked by run %s (pid %d on %s, last heartbeat %s)", e.Holder.RunID,
		e.Holder.PID, e.Holder.Host, e.Holder.HeartbeatAt.Format(time.RFC3339))
}

// ErrLocked matches a *LockedError with errors.Is.
var ErrLocked = errors.New("repository is locked by another run")

// Is lets errors.Is(err, ErrLocked) match.
func (e *LockedError) Is(target error) bool { return target == ErrLocked }

// ErrLockLost is returned by Heartbeat and Release when the lock was taken
// over (this holder was considered stale) or removed.
var ErrLockLost = errors.New("run lock lost")

// Lock is a held repository lock.
type Lock struct {
	s    *Store
	info LockInfo
}

// Info returns the lock as acquired.
func (l *Lock) Info() LockInfo { return l.info }

// Owner returns the owner token; steps record it (StepIntent.Owner).
func (l *Lock) Owner() string { return l.info.Owner }

func newOwner() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

const lockCols = `repo_key, run_id, owner, pid, host, acquired_at, heartbeat_at, ttl_ms`

func scanLock(sc scanner) (LockInfo, error) {
	var (
		l            LockInfo
		acq, hb, ttl int64
	)
	if err := sc.Scan(&l.RepoKey, &l.RunID, &l.Owner, &l.PID, &l.Host, &acq, &hb, &ttl); err != nil {
		return LockInfo{}, err
	}
	l.AcquiredAt, l.HeartbeatAt, l.TTL = msTime(acq), msTime(hb), time.Duration(ttl)*time.Millisecond
	return l, nil
}

// AcquireLock takes the run lock of the repository at repoPath for runID.
// It fails with *LockedError while another holder heartbeats. A stale lock
// (no heartbeat for its TTL; the holder crashed or was killed) is taken
// over; the previous holder is returned so the caller can report it and
// reconcile the run (MarkInterrupted). ttl <= 0 uses DefaultLockTTL.
func (s *Store) AcquireLock(ctx context.Context, repoPath, runID string, ttl time.Duration) (*Lock, *LockInfo, error) {
	if ttl <= 0 {
		ttl = DefaultLockTTL
	}
	key, err := RepoKey(repoPath)
	if err != nil {
		return nil, nil, fmt.Errorf("state: repository path: %w", err)
	}
	host, _ := os.Hostname()
	info := LockInfo{RepoKey: key, RunID: runID, Owner: newOwner(), PID: os.Getpid(), Host: host, TTL: ttl}
	var prev *LockInfo
	err = s.write(ctx, func(tx *wtx) error {
		now := s.now()
		cur, err := scanLock(tx.QueryRowContext(ctx, `SELECT `+lockCols+` FROM locks WHERE repo_key = ?`, key))
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("state: %w", err)
		case !cur.Stale(now):
			return &LockedError{Holder: cur}
		default:
			prev = &cur
		}
		info.AcquiredAt, info.HeartbeatAt = msTime(now.UnixMilli()), msTime(now.UnixMilli())
		_, err = tx.ExecContext(ctx, `INSERT INTO locks (`+lockCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (repo_key) DO UPDATE SET run_id = excluded.run_id, owner = excluded.owner, pid = excluded.pid,
			host = excluded.host, acquired_at = excluded.acquired_at, heartbeat_at = excluded.heartbeat_at,
			ttl_ms = excluded.ttl_ms`, key, runID, info.Owner, info.PID, s.mask(host), now.UnixMilli(), now.UnixMilli(),
			ttl.Milliseconds())
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return &Lock{s: s, info: info}, prev, nil
}

// GetLock returns the current holder of the repository's lock.
func (s *Store) GetLock(ctx context.Context, repoPath string) (LockInfo, error) {
	key, err := RepoKey(repoPath)
	if err != nil {
		return LockInfo{}, err
	}
	l, err := scanLock(s.db.QueryRowContext(ctx, `SELECT `+lockCols+` FROM locks WHERE repo_key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return LockInfo{}, fmt.Errorf("state: lock: %w", ErrNotFound)
	}
	return l, err
}

// Heartbeat extends the lock. ErrLockLost means another process took it
// over: stop acting on the run at once.
func (l *Lock) Heartbeat(ctx context.Context) error {
	return l.s.write(ctx, func(tx *wtx) error {
		now := l.s.nowMS()
		res, err := tx.ExecContext(ctx, `UPDATE locks SET heartbeat_at = ? WHERE repo_key = ? AND owner = ?`,
			now, l.info.RepoKey, l.info.Owner)
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrLockLost
		}
		l.info.HeartbeatAt = msTime(now)
		return nil
	})
}

// Release drops the lock if this holder still has it.
func (l *Lock) Release(ctx context.Context) error {
	return l.s.write(ctx, func(tx *wtx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE repo_key = ? AND owner = ?`, l.info.RepoKey, l.info.Owner)
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrLockLost
		}
		return nil
	})
}

// KeepAlive heartbeats every interval (TTL/3 when interval <= 0) until ctx
// ends. The returned channel receives ErrLockLost (or a database error that
// outlasted the TTL) and is then closed; it is closed without a value when
// ctx ends.
func (l *Lock) KeepAlive(ctx context.Context, interval time.Duration) <-chan error {
	if interval <= 0 {
		interval = l.info.TTL / 3
	}
	out := make(chan error, 1)
	go func() {
		defer close(out)
		t := time.NewTicker(interval)
		defer t.Stop()
		lastOK := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			err := l.Heartbeat(ctx)
			switch {
			case err == nil:
				lastOK = time.Now()
			case ctx.Err() != nil:
				return
			case errors.Is(err, ErrLockLost) || time.Since(lastOK) > l.info.TTL:
				out <- err
				return
			}
			// Other errors (a busy database) are retried until the TTL.
		}
	}()
	return out
}
