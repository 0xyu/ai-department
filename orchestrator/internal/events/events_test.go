package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/i18n"
	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/secure/securetest"
	"github.com/0xyu/aco/internal/state"
	"github.com/0xyu/aco/schemas"
)

func newStore(t *testing.T, path string, opts state.Options) *state.Store {
	t.Helper()
	if path == "" {
		path = filepath.Join(t.TempDir(), "state.db")
	}
	s, err := state.Open(context.Background(), path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newRun(t *testing.T, s *state.Store) string {
	t.Helper()
	r, err := s.CreateRun(context.Background(), state.NewRun{RepoPath: t.TempDir(), Mode: state.ModeFull,
		Input: state.InputPlan})
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func publishN(t *testing.T, b *events.Bus, runID string, n int) {
	t.Helper()
	if err := publish(b, runID, n); err != nil {
		t.Fatal(err)
	}
}

// publish is safe to call from any goroutine.
func publish(b *events.Bus, runID string, n int) error {
	for i := 0; i < n; i++ {
		if _, err := b.Publish(context.Background(), events.Event{RunID: runID, Type: events.GateCompleted,
			Payload: events.Payload{events.Int("i", int64(i))}}); err != nil {
			return err
		}
	}
	return nil
}

// collect reads n events or fails after a timeout.
func collect(t *testing.T, sub *events.Subscription, n int) []events.Event {
	t.Helper()
	var out []events.Event
	timeout := time.After(20 * time.Second)
	for len(out) < n {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed after %d events: %v", len(out), sub.Err())
			}
			out = append(out, e)
		case <-timeout:
			t.Fatalf("got %d of %d events", len(out), n)
		}
	}
	return out
}

func checkSeq(t *testing.T, evs []events.Event, from int64) {
	t.Helper()
	for i, e := range evs {
		if e.Seq != from+int64(i) {
			t.Fatalf("event %d: seq %d, want %d", i, e.Seq, from+int64(i))
		}
	}
}

// A late subscriber receives every event from the requested seq, in order,
// then live events, with no gaps or duplicates.
func TestLateSubscriberReplaysFromSeq(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newStore(t, "", state.Options{})
	b := events.NewBus(s)
	run := newRun(t, s) // seq 1: run.created
	publishN(t, b, run, 599)

	for _, from := range []int64{0, 1, 2, 257, 600} {
		sub, err := b.Subscribe(ctx, run, from)
		if err != nil {
			t.Fatal(err)
		}
		start := from
		if start == 0 {
			start = 1
		}
		got := collect(t, sub, int(600-start+1))
		checkSeq(t, got, start)
	}

	// Live delivery after replay, while another goroutine publishes.
	sub, _ := b.Subscribe(ctx, run, 595)
	go func() {
		if err := publish(b, run, 50); err != nil {
			t.Error(err)
		}
	}()
	got := collect(t, sub, 56)
	checkSeq(t, got, 595)
	if got[0].Type != events.GateCompleted || got[len(got)-1].Seq != 650 {
		t.Errorf("unexpected tail %+v", got[len(got)-1])
	}

	// Subscribing beyond the end waits for new events.
	sub2, _ := b.Subscribe(ctx, run, 700)
	publishN(t, b, run, 60)
	got = collect(t, sub2, 11)
	checkSeq(t, got, 700)

	if _, err := b.Subscribe(ctx, run, -1); !errors.Is(err, events.ErrBadSeq) {
		t.Errorf("negative seq: %v", err)
	}
}

func TestConcurrentPublishersKeepSeqMonotonic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newStore(t, "", state.Options{})
	b := events.NewBus(s)
	run := newRun(t, s)
	sub, _ := b.Subscribe(ctx, run, 1)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := publish(b, run, 25); err != nil {
				t.Error(err)
			}
		}()
	}
	got := collect(t, sub, 201)
	wg.Wait()
	checkSeq(t, got, 1)
}

// Events written by another process (another Store on the same file) reach
// a subscriber by polling.
func TestSubscriberSeesOtherProcessEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "state.db")
	reader := newStore(t, path, state.Options{})
	writer := newStore(t, path, state.Options{})
	run := newRun(t, writer)
	b := events.NewBus(reader, events.WithPollInterval(20*time.Millisecond))
	sub, _ := b.Subscribe(ctx, run, 0)
	publishN(t, events.NewBus(writer), run, 10)
	checkSeq(t, collect(t, sub, 11), 1)
}

func TestSubscriptionEndsWithContext(t *testing.T) {
	s := newStore(t, "", state.Options{})
	b := events.NewBus(s)
	run := newRun(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	sub, _ := b.Subscribe(ctx, run, 0)
	if sub.Err() != nil {
		t.Fatal("open subscription has an error")
	}
	collect(t, sub, 1)
	cancel()
	for range sub.C {
	}
	if !errors.Is(sub.Err(), context.Canceled) {
		t.Errorf("err %v", sub.Err())
	}
}

func TestEventJSONRoundTrip(t *testing.T) {
	e := events.Event{RunID: "20261109-abcd", Seq: 7, Type: events.RunBlocked,
		Time: time.Date(2026, 11, 9, 1, 2, 3, 4000000, time.UTC), Stage: "execution", Phase: 3,
		Payload: events.Payload{events.Str("class", "unsafe"), events.Int("attempt", 2), events.Bool("x", false),
			events.Strs("ids", "F-03-001", "F-03-002")}, Message: "运行已阻塞"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":"aco.event/v1","run_id":"20261109-abcd","seq":7,"type":"run.blocked",` +
		`"time":"2026-11-09T01:02:03.004Z","stage":"execution","phase":3,` +
		`"payload":{"class":"unsafe","attempt":2,"x":false,"ids":["F-03-001","F-03-002"]},"message":"运行已阻塞"}`
	if string(b) != want {
		t.Errorf("encoding:\n got %s\nwant %s", b, want)
	}
	var back events.Event
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, e) {
		t.Errorf("round trip:\n got %+v\nwant %+v", back, e)
	}
	for _, bad := range []string{
		`{"schema":"aco.event/v2"}`,
		`{"schema":"aco.event/v1","payload":{"n":1.5}}`,
		`{"schema":"aco.event/v1","payload":{"o":{}}}`,
		`{"schema":"aco.event/v1","payload":{"l":[1]}}`,
		`{"schema":"aco.event/v1","payload":{"A":"x"}}`,
		`{"schema":"aco.event/v1","payload":{"a":"x","a":"y"}}`,
		`{"schema":"aco.event/v1","payload":[]}`,
	} {
		if err := json.Unmarshal([]byte(bad), &back); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := json.Marshal(events.Event{Payload: events.Payload{{Key: "no_value"}}}); err == nil {
		t.Error("field without a value encoded")
	}
}

// The JSON schema lists exactly the Go event types.
func TestEventSchemaMatchesTypes(t *testing.T) {
	var sch struct {
		Properties struct {
			Type struct {
				Enum []string `json:"enum"`
			} `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemas.Event, &sch); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, ty := range events.All {
		want = append(want, string(ty))
	}
	if !reflect.DeepEqual(sch.Properties.Type.Enum, want) {
		t.Errorf("schema enum differs from events.All:\n schema %v\n go     %v", sch.Properties.Type.Enum, want)
	}
}

func TestEveryTypeHasMessages(t *testing.T) {
	seen := map[events.Type]bool{}
	for _, ty := range events.All {
		if seen[ty] {
			t.Errorf("duplicate type %s", ty)
		}
		seen[ty] = true
		if !events.Known(ty) {
			t.Errorf("%s not known", ty)
		}
		for _, l := range i18n.Langs {
			if m := events.Message(l, ty); m == "" || strings.HasPrefix(m, "!(") {
				t.Errorf("%s: no %s message", ty, l)
			}
		}
	}
	if events.Known("run.unknown") || events.Message(i18n.EN, "run.unknown") != "" {
		t.Error("unknown type accepted")
	}
	if len(events.All) != 56 {
		t.Errorf("%d event types; update docs/lifecycle.md and the schema when the set changes", len(events.All))
	}
}

// Events written with fake secrets are masked in storage, replay, and JSON.
func TestEventsNeverCarrySecrets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var secrets []secure.Secret
	for _, f := range securetest.All {
		secrets = append(secrets, secure.NewSecret(f))
	}
	s := newStore(t, "", state.Options{Redactor: secure.NewRedactor(secrets...)})
	b := events.NewBus(s)
	run := newRun(t, s)
	all := strings.Join(securetest.All, " ")
	if _, err := b.Publish(ctx, events.Event{RunID: run, Type: events.AgentInvocationFailed, Message: all,
		Payload: events.Payload{events.Str("error", all), events.Strs("args", securetest.All...)}}); err != nil {
		t.Fatal(err)
	}
	sub, _ := b.Subscribe(ctx, run, 0)
	var out strings.Builder
	for _, e := range collect(t, sub, 2) {
		j, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(j)
	}
	if !strings.Contains(out.String(), secure.Mask) {
		t.Fatal("nothing masked")
	}
	securetest.AssertNoLeak(t, "events", out.String())
}
