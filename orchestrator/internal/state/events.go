package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/0xyu/aco/internal/events"
)

var _ events.Log = (*Store)(nil)

// AppendEvent stores e under the next Seq of its run and returns it as
// stored (Seq, Time, Message filled in; strings masked). It implements
// events.Log.
func (s *Store) AppendEvent(ctx context.Context, e events.Event) (events.Event, error) {
	var out events.Event
	err := s.write(ctx, func(tx *wtx) error {
		var err error
		out, err = tx.appendEvent(ctx, e)
		return err
	})
	return out, err
}

// emit appends a state-machine event inside the transaction.
func (tx *wtx) emit(ctx context.Context, runID string, t events.Type, stage Stage, phase int, payload ...events.Field) error {
	_, err := tx.appendEvent(ctx, events.Event{RunID: runID, Type: t, Stage: string(stage), Phase: phase, Payload: payload})
	return err
}

func (tx *wtx) appendEvent(ctx context.Context, e events.Event) (events.Event, error) {
	if !events.Known(e.Type) {
		return events.Event{}, fmt.Errorf("state: unknown event type %q", e.Type)
	}
	if e.Stage != "" && !Stage(e.Stage).Valid() {
		return events.Event{}, fmt.Errorf("state: unknown stage %q", e.Stage)
	}
	if e.Phase < 0 {
		return events.Event{}, errors.New("state: negative phase")
	}
	if e.Payload == nil {
		e.Payload = events.Payload{}
	}
	if err := e.Payload.Validate(); err != nil {
		return events.Event{}, fmt.Errorf("state: %w", err)
	}
	s := tx.s
	e.Payload = e.Payload.Redact(s.red)
	if e.Message == "" {
		e.Message = events.Message(s.lang, e.Type)
	}
	e.Message = s.mask(e.Message)
	if e.Time.IsZero() {
		e.Time = s.now()
	}
	e.Time = msTime(e.Time.UnixMilli())
	payload, err := e.Payload.MarshalJSON()
	if err != nil {
		return events.Event{}, err
	}
	err = tx.QueryRowContext(ctx, `UPDATE runs SET last_event_seq = last_event_seq + 1 WHERE id = ? RETURNING last_event_seq`,
		e.RunID).Scan(&e.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		return events.Event{}, fmt.Errorf("state: run %s: %w", e.RunID, ErrNotFound)
	}
	if err != nil {
		return events.Event{}, fmt.Errorf("state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (run_id, seq, type, time, stage, phase, payload, message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, e.RunID, e.Seq, string(e.Type), e.Time.UnixMilli(), e.Stage, e.Phase,
		string(payload), e.Message); err != nil {
		return events.Event{}, fmt.Errorf("state: %w", err)
	}
	tx.appended = append(tx.appended, e)
	return e, nil
}

// EventsAfter returns up to limit events of runID with Seq > after, in Seq
// order. It implements events.Log.
func (s *Store) EventsAfter(ctx context.Context, runID string, after int64, limit int) ([]events.Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, type, time, stage, phase, payload, message FROM events
		WHERE run_id = ? AND seq > ? ORDER BY seq LIMIT ?`, runID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []events.Event
	for rows.Next() {
		var (
			e       events.Event
			typ     string
			ms      int64
			payload string
		)
		if err := rows.Scan(&e.Seq, &typ, &ms, &e.Stage, &e.Phase, &payload, &e.Message); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		e.RunID, e.Type, e.Time = runID, events.Type(typ), msTime(ms)
		if err := e.Payload.UnmarshalJSON([]byte(payload)); err != nil {
			return nil, fmt.Errorf("state: event %d payload: %w", e.Seq, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
