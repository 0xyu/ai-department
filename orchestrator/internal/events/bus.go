package events

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Log is durable event storage (implemented by internal/state.Store).
type Log interface {
	// AppendEvent assigns the next Seq of e.RunID, masks and stores e, and
	// returns it as stored.
	AppendEvent(ctx context.Context, e Event) (Event, error)
	// EventsAfter returns up to limit events of runID with Seq > after, in
	// Seq order.
	EventsAfter(ctx context.Context, runID string, after int64, limit int) ([]Event, error)
	// OnAppend registers fn, called after every committed append (including
	// events the Log writes itself, such as state transitions).
	OnAppend(fn func(runID string, seq int64))
}

// Bus publishes events to a Log and delivers them to subscribers. A
// subscriber reads from the Log, so it receives every event from its
// starting Seq in order, without gaps or duplicates, however late it joins
// and however slowly it reads. Events appended by another process are picked
// up by polling.
type Bus struct {
	log   Log
	poll  time.Duration
	batch int

	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}
}

// Option configures a Bus.
type Option func(*Bus)

// WithPollInterval sets how often subscribers re-read the Log without a
// local notification (events from other processes). Default 1s.
func WithPollInterval(d time.Duration) Option { return func(b *Bus) { b.poll = d } }

// NewBus returns a Bus over log.
func NewBus(log Log, opts ...Option) *Bus {
	b := &Bus{log: log, poll: time.Second, batch: 256, subs: map[string]map[*Subscription]struct{}{}}
	for _, o := range opts {
		o(b)
	}
	log.OnAppend(b.notify)
	return b
}

// Publish appends e to the Log; subscribers are woken by the Log callback.
func (b *Bus) Publish(ctx context.Context, e Event) (Event, error) {
	return b.log.AppendEvent(ctx, e)
}

func (b *Bus) notify(runID string, _ int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs[runID] {
		select {
		case s.wake <- struct{}{}:
		default: // already pending
		}
	}
}

// Subscription delivers events on C until its context ends or reading the
// Log fails; then C is closed and Err reports why.
type Subscription struct {
	C    <-chan Event
	wake chan struct{}
	done chan struct{}
	err  error
}

// Err returns nil while C is open, then the reason it closed
// (context.Canceled, context.DeadlineExceeded, or a Log error).
func (s *Subscription) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// ErrBadSeq is returned for a negative starting Seq.
var ErrBadSeq = errors.New("events: fromSeq must be >= 0")

// Subscribe delivers every event of runID with Seq >= fromSeq (0 or 1 = from
// the beginning), then new events as they are appended.
func (b *Bus) Subscribe(ctx context.Context, runID string, fromSeq int64) (*Subscription, error) {
	if fromSeq < 0 {
		return nil, ErrBadSeq
	}
	ch := make(chan Event)
	s := &Subscription{C: ch, wake: make(chan struct{}, 1), done: make(chan struct{})}
	// Register before the first read so no append can be missed.
	b.mu.Lock()
	if b.subs[runID] == nil {
		b.subs[runID] = map[*Subscription]struct{}{}
	}
	b.subs[runID][s] = struct{}{}
	b.mu.Unlock()

	last := fromSeq - 1
	if last < 0 {
		last = 0
	}
	go func() {
		defer func() {
			b.mu.Lock()
			delete(b.subs[runID], s)
			if len(b.subs[runID]) == 0 {
				delete(b.subs, runID)
			}
			b.mu.Unlock()
			close(ch)
			close(s.done)
		}()
		ticker := time.NewTicker(b.poll)
		defer ticker.Stop()
		for {
			evs, err := b.log.EventsAfter(ctx, runID, last, b.batch)
			if err != nil {
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				s.err = err
				return
			}
			for _, e := range evs {
				select {
				case ch <- e:
					last = e.Seq
				case <-ctx.Done():
					s.err = ctx.Err()
					return
				}
			}
			if len(evs) == b.batch {
				continue
			}
			select {
			case <-ctx.Done():
				s.err = ctx.Err()
				return
			case <-s.wake:
			case <-ticker.C:
			}
		}
	}()
	return s, nil
}
