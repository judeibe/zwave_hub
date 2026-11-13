package hub

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/store"
)

// testTimeout bounds every wait in this file, so a wedged worker fails a test
// instead of hanging the suite.
const testTimeout = 10 * time.Second

// testActorAddr is the recorded (not authenticated) client address each audit
// row carries: the store refuses an entry without one.
const testActorAddr = "192.0.2.10"

// testState is the immutable snapshot the commands in this file build. The
// hub's real snapshot arrives in a later phase; what matters here is the same
// discipline: a command returns a new value and copies the map and slice rather
// than mutating the one readers hold.
type testState struct {
	values  map[string]string
	applied []string
}

// cloneState returns a copy that shares nothing mutable with s.
func cloneState(s testState) testState {
	next := testState{
		values:  make(map[string]string, len(s.values)),
		applied: append([]string(nil), s.applied...),
	}
	for class, value := range s.values {
		next.values[class] = value
	}
	return next
}

// testAudit is a valid audit entry for a committed key replacement, which is
// the action every command in this file records.
func testAudit(requestID string) store.AuditEntry {
	return store.AuditEntry{
		Action:    store.AuditKeysReplaced,
		Outcome:   store.OutcomeSucceeded,
		ActorAddr: testActorAddr,
		RequestID: requestID,
	}
}

// appendCommand builds a command that appends id to the applied list of the
// last committed snapshot and records the audit row named by requestID.
func appendCommand(w *Worker[testState], id, requestID string) Command[testState] {
	return Command[testState]{
		Audit: testAudit(requestID),
		Apply: func(_ context.Context, _ *store.Tx) (testState, error) {
			current, _ := w.Snapshot()
			next := cloneState(current)
			next.applied = append(next.applied, id)
			return next, nil
		},
	}
}

// newTestStore opens a store over a fresh temporary directory and closes it
// when the test ends. It supplies a fixed master key, because a hub test that
// submits or generates keys seals them, and a store opened without one refuses
// to seal (R4). A real hub takes the key from ZWAVE_HUB_MASTER_KEY or
// <DataDir>/master.key; this one is 32 bytes of a test literal and is never
// anything a deployed hub would hold.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DataDir:   t.TempDir(),
		MasterKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return st
}

// newTestWorker starts a worker over a fresh store. Cleanup runs
// last-in-first-out, so registering the store first leaves the worker's stop
// ahead of the store's close.
func newTestWorker(t *testing.T) (*Worker[testState], *store.Store) {
	t.Helper()
	st := newTestStore(t)
	w := NewWorker[testState](st)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Errorf("the worker did not stop within %s", testTimeout)
		}
	})
	return w, st
}

// auditRows returns the whole audit trail, newest first.
func auditRows(t *testing.T, st *store.Store) []store.AuditEntry {
	t.Helper()
	page, err := st.ListAudit(context.Background(), store.AuditQuery{Limit: 200})
	if err != nil {
		t.Fatalf("list the audit trail: %v", err)
	}
	if page.NextBefore != nil {
		t.Fatalf("the audit page was truncated at %d rows", len(page.Items))
	}
	return page.Items
}

func auditCount(t *testing.T, st *store.Store) int {
	t.Helper()
	return len(auditRows(t, st))
}

// assertSnapshot proves the published snapshot is exactly want.
func assertSnapshot(t *testing.T, w *Worker[testState], want testState) {
	t.Helper()
	got, ok := w.Snapshot()
	if !ok {
		t.Fatalf("no snapshot is published, want %+v", want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("published snapshot is %+v, want %+v", got, want)
	}
}

// waitForSignal waits for ch to be closed or sent on, failing the test if the
// worker does not get that far.
func waitForSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testTimeout):
		t.Fatalf("timed out after %s waiting for %s", testTimeout, what)
	}
}

// submitRetrying submits command and retries only the command_queue_full
// refusal. Twenty concurrent submitters can outrun a queue that is 16 deep
// while the worker commits — every commit is an fsync (synchronous=FULL) — and
// ErrQueueFull is the documented retryable refusal, so the retry is the client
// behaviour the API asks for rather than a weakened assertion.
func submitRetrying(w *Worker[testState], command Command[testState]) (testState, error) {
	deadline := time.Now().Add(testTimeout)
	for {
		snapshot, err := w.Submit(context.Background(), command)
		if !errors.Is(err, ErrQueueFull) {
			return snapshot, err
		}
		if time.Now().After(deadline) {
			return testState{}, fmt.Errorf("the queue stayed full for %s: %w", testTimeout, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSubmitAppliesInArrivalOrder proves that concurrent submissions are
// serialized: every command commits, no update is lost or applied twice, and
// each Apply starts from the state the previous command left, so publication
// can only happen after a commit (FR-021, SC-009).
func TestSubmitAppliesInArrivalOrder(t *testing.T) {
	w, st := newTestWorker(t)

	const submissions = 20

	// prev is the state the previous command returned. Apply runs only on the
	// worker's goroutine, so a plain variable touched only there is enough.
	// The initial zero value is what an unpublished snapshot reads as.
	var (
		prev     testState
		commands int
	)

	command := func(id string) Command[testState] {
		return Command[testState]{
			Audit: testAudit("req-" + id),
			Apply: func(_ context.Context, _ *store.Tx) (testState, error) {
				commands++
				published, ok := w.Snapshot()
				if commands > 1 && !ok {
					t.Errorf("command %s ran with no snapshot published, but %d commands had already committed", id, commands-1)
				}
				if !reflect.DeepEqual(published, prev) {
					t.Errorf("command %s started from %+v, but the previous command left %+v: two commands overlapped or a snapshot was published before its commit",
						id, published, prev)
				}
				next := cloneState(prev)
				next.applied = append(next.applied, id)
				prev = next
				return next, nil
			},
		}
	}

	ids := make([]string, submissions)
	errs := make(chan error, submissions)
	var wg sync.WaitGroup
	for i := range ids {
		id := fmt.Sprintf("cmd-%02d", i)
		ids[i] = id
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := submitRetrying(w, command(id)); err != nil {
				errs <- fmt.Errorf("submit %s: %w", id, err)
			}
		}()
	}
	wg.Wait()
	close(errs)

	var failed []error
	for err := range errs {
		failed = append(failed, err)
	}
	if len(failed) > 0 {
		t.Fatalf("%d of %d submissions failed: %v", len(failed), submissions, failed)
	}

	final, ok := w.Snapshot()
	if !ok {
		t.Fatal("no snapshot was published")
	}
	if len(final.applied) != submissions {
		t.Fatalf("the final snapshot holds %d ids, want %d: %v", len(final.applied), submissions, final.applied)
	}
	seen := make(map[string]int, submissions)
	for _, id := range final.applied {
		seen[id]++
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("id %s appears %d times in the final snapshot, want exactly once", id, seen[id])
		}
	}

	rows := auditRows(t, st)
	if len(rows) != submissions {
		t.Fatalf("the audit trail holds %d rows, want %d", len(rows), submissions)
	}
	rowRequests := make(map[string]int, submissions)
	for _, row := range rows {
		rowRequests[row.RequestID]++
	}
	for _, id := range ids {
		if got := rowRequests["req-"+id]; got != 1 {
			t.Errorf("the audit trail holds %d rows for %s, want exactly one", got, id)
		}
	}
}

// TestLaterSubmissionWinsPerClass proves the queue gives configuration changes
// a total order: sequential submissions for the same class leave the last one
// in force, and the audit trail reads back in commit order, newest first.
func TestLaterSubmissionWinsPerClass(t *testing.T) {
	w, st := newTestWorker(t)

	type change struct {
		class     string
		value     string
		requestID string
	}
	changes := []change{
		{class: "S0_Legacy", value: "A", requestID: "req-1"},
		{class: "S2_Authenticated", value: "B", requestID: "req-2"},
		{class: "S0_Legacy", value: "C", requestID: "req-3"},
	}

	var last testState
	for _, c := range changes {
		got, err := w.Submit(context.Background(), Command[testState]{
			Audit: store.AuditEntry{
				Action:        store.AuditKeysReplaced,
				Outcome:       store.OutcomeSucceeded,
				ActorAddr:     testActorAddr,
				RequestID:     c.requestID,
				SecurityClass: c.class,
			},
			Apply: func(_ context.Context, _ *store.Tx) (testState, error) {
				current, _ := w.Snapshot()
				next := cloneState(current)
				next.values[c.class] = c.value
				return next, nil
			},
		})
		if err != nil {
			t.Fatalf("submit %s: %v", c.requestID, err)
		}
		last = got
	}

	final, ok := w.Snapshot()
	if !ok {
		t.Fatal("no snapshot was published")
	}
	if got, want := final.values["S0_Legacy"], "C"; got != want {
		t.Errorf("S0_Legacy holds %q, want %q: the later submission must win", got, want)
	}
	if got, want := final.values["S2_Authenticated"], "B"; got != want {
		t.Errorf("S2_Authenticated holds %q, want %q", got, want)
	}
	if len(final.values) != 2 {
		t.Errorf("the snapshot holds %d classes, want 2: %v", len(final.values), final.values)
	}
	if !reflect.DeepEqual(final, last) {
		t.Errorf("the last Submit returned %+v, but the published snapshot is %+v", last, final)
	}

	rows := auditRows(t, st)
	if len(rows) != len(changes) {
		t.Fatalf("the audit trail holds %d rows, want %d", len(rows), len(changes))
	}
	// Newest first: the third submission (S0 = C), then the second (S2 = B),
	// then the first (S0 = A).
	wantRequests := []string{"req-3", "req-2", "req-1"}
	wantClasses := []string{"S0_Legacy", "S2_Authenticated", "S0_Legacy"}
	for i, row := range rows {
		if row.RequestID != wantRequests[i] {
			t.Errorf("audit row %d is %s, want %s: the trail must read back in commit order", i, row.RequestID, wantRequests[i])
		}
		if row.SecurityClass != wantClasses[i] {
			t.Errorf("audit row %d names class %s, want %s", i, row.SecurityClass, wantClasses[i])
		}
		if i > 0 && row.ID >= rows[i-1].ID {
			t.Errorf("audit ids are not descending: row %d is %d, row %d is %d", i, row.ID, i-1, rows[i-1].ID)
		}
	}
}

// TestSnapshotIsPublishedOnlyAfterCommit proves the snapshot and the audit row
// commit or roll back as one: a failed Apply leaves no trace, and an audit
// entry the store refuses rolls the state change back with it. A command that
// returns an error returns no snapshot either.
func TestSnapshotIsPublishedOnlyAfterCommit(t *testing.T) {
	w, st := newTestWorker(t)

	baseline, err := w.Submit(context.Background(), Command[testState]{
		Audit: testAudit("req-baseline"),
		Apply: func(_ context.Context, _ *store.Tx) (testState, error) {
			return testState{values: map[string]string{"k": "v0"}, applied: []string{"baseline"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("commit the baseline: %v", err)
	}
	if got := auditCount(t, st); got != 1 {
		t.Fatalf("the audit trail holds %d rows after the baseline, want 1", got)
	}

	sentinel := errors.New("the change was refused")
	got, err := w.Submit(context.Background(), Command[testState]{
		Audit: testAudit("req-apply-error"),
		Apply: func(_ context.Context, _ *store.Tx) (testState, error) {
			return testState{values: map[string]string{"k": "bad"}}, sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Submit returned %v, want %v", err, sentinel)
	}
	if !reflect.DeepEqual(got, testState{}) {
		t.Errorf("a failed Apply returned the snapshot %+v, want the zero value", got)
	}
	assertSnapshot(t, w, baseline)
	rows := auditRows(t, st)
	if len(rows) != 1 {
		t.Fatalf("the audit trail holds %d rows after a failed Apply, want 1", len(rows))
	}
	if rows[0].RequestID != "req-baseline" {
		t.Errorf("the audit trail holds a row for %s after a failed Apply, want only the baseline", rows[0].RequestID)
	}

	// The Apply succeeds here, so only the store's audit validation fails the
	// command: the state change must roll back with the row.
	got, err = w.Submit(context.Background(), Command[testState]{
		Audit: store.AuditEntry{
			Action:    store.AuditKeysReplaced,
			Outcome:   store.OutcomeSucceeded,
			ActorAddr: "", // the store refuses an entry with no actor address
			RequestID: "req-bad-audit",
		},
		Apply: func(_ context.Context, _ *store.Tx) (testState, error) {
			return testState{values: map[string]string{"k": "bad2"}}, nil
		},
	})
	if err == nil {
		t.Fatal("Submit accepted a command whose audit entry is invalid")
	}
	if !reflect.DeepEqual(got, testState{}) {
		t.Errorf("a command whose audit entry was refused returned the snapshot %+v, want the zero value", got)
	}
	assertSnapshot(t, w, baseline)
	rows = auditRows(t, st)
	if len(rows) != 1 {
		t.Fatalf("the audit trail holds %d rows after a refused audit entry, want 1", len(rows))
	}
	if rows[0].RequestID != "req-baseline" {
		t.Errorf("the audit trail holds a row for %s after a refused audit entry, want only the baseline", rows[0].RequestID)
	}
}

// TestNoIntermediateSnapshot proves a reader never sees a change before it
// commits: while a command's Apply is blocked the previous snapshot stays
// published, and the new one appears only after the command is released and
// Submit returns (FR-021, SC-009).
func TestNoIntermediateSnapshot(t *testing.T) {
	w, _ := newTestWorker(t)

	baseline, err := w.Submit(context.Background(), Command[testState]{
		Audit: testAudit("req-baseline"),
		Apply: func(_ context.Context, _ *store.Tx) (testState, error) {
			return testState{values: map[string]string{"k": "v0"}, applied: []string{"baseline"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("commit the baseline: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	type outcome struct {
		snapshot testState
		err      error
	}
	pending := make(chan outcome, 1)
	go func() {
		snapshot, err := w.Submit(context.Background(), Command[testState]{
			Audit: testAudit("req-blocked"),
			Apply: func(ctx context.Context, _ *store.Tx) (testState, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return testState{}, ctx.Err()
				}
				return testState{values: map[string]string{"k": "v1"}, applied: []string{"blocked"}}, nil
			},
		})
		pending <- outcome{snapshot: snapshot, err: err}
	}()

	waitForSignal(t, entered, "the blocked command to enter Apply")

	for i := 0; i < 10; i++ {
		assertSnapshot(t, w, baseline)
		time.Sleep(time.Millisecond)
	}
	select {
	case got := <-pending:
		t.Fatalf("Submit returned while Apply was still blocked: %+v", got)
	default:
	}

	unblock()

	var blocked outcome
	select {
	case blocked = <-pending:
	case <-time.After(testTimeout):
		t.Fatal("the blocked command did not finish after it was released")
	}
	if blocked.err != nil {
		t.Fatalf("the blocked command failed: %v", blocked.err)
	}
	want := testState{values: map[string]string{"k": "v1"}, applied: []string{"blocked"}}
	if !reflect.DeepEqual(blocked.snapshot, want) {
		t.Errorf("Submit returned %+v, want %+v", blocked.snapshot, want)
	}
	assertSnapshot(t, w, want)
}

// TestFullQueueRefuses proves backpressure is visible rather than hidden: a
// full queue refuses with ErrQueueFull and writes nothing, while accepted
// commands write their audit row (R9, FR-021).
func TestFullQueueRefuses(t *testing.T) {
	w, st := newTestWorker(t)

	// Count before anything blocks: the store hands out one connection, so a
	// ListAudit issued while the worker holds a transaction would wait for that
	// transaction rather than report the trail.
	rowsBefore := auditCount(t, st)

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	blocker := make(chan error, 1)
	go func() {
		_, err := w.Submit(context.Background(), Command[testState]{
			Audit: testAudit("req-blocker"),
			Apply: func(ctx context.Context, _ *store.Tx) (testState, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return testState{}, ctx.Err()
				}
				return testState{applied: []string{"blocker"}}, nil
			},
		})
		blocker <- err
	}()
	waitForSignal(t, entered, "the blocking command to start")

	type submission struct {
		requestID string
		err       error
	}
	outcomes := map[string]error{}

	const fillers = QueueCapacity + 1
	results := make(chan submission, fillers)
	var wg sync.WaitGroup
	for i := 0; i < fillers; i++ {
		id := fmt.Sprintf("fill-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.Submit(context.Background(), appendCommand(w, id, "req-"+id))
			results <- submission{requestID: "req-" + id, err: err}
		}()
	}

	// The worker is blocked and never drains, so of fillers submissions at
	// least one finds no slot. The first result to arrive can only be a
	// refusal: an accepted submission waits for a reply that cannot come until
	// the block is released.
	var refusedFirst submission
	select {
	case refusedFirst = <-results:
	case <-time.After(testTimeout):
		t.Fatal("no submission was refused while the worker was blocked and the queue held every slot")
	}
	if !errors.Is(refusedFirst.err, ErrQueueFull) {
		t.Fatalf("submission %s returned %v, want ErrQueueFull", refusedFirst.requestID, refusedFirst.err)
	}
	outcomes[refusedFirst.requestID] = refusedFirst.err

	// The queue is full and nothing drains it, so every further submission is
	// refused without being enqueued.
	for i := 0; i < 3; i++ {
		requestID := fmt.Sprintf("req-extra-%02d", i)
		snapshot, err := w.Submit(context.Background(), appendCommand(w, "extra", requestID))
		if !errors.Is(err, ErrQueueFull) {
			t.Fatalf("submission %s returned %v, want ErrQueueFull: the queue is full and nothing drains it", requestID, err)
		}
		if !reflect.DeepEqual(snapshot, testState{}) {
			t.Errorf("a refused submission returned %+v, want the zero snapshot", snapshot)
		}
		outcomes[requestID] = err
	}

	unblock()
	select {
	case err := <-blocker:
		if err != nil {
			t.Fatalf("the blocking command failed: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the blocking command did not finish after it was released")
	}
	wg.Wait()
	close(results)

	outcomes["req-blocker"] = nil
	for result := range results {
		outcomes[result.requestID] = result.err
	}

	accepted, refused := 0, 0
	for requestID, err := range outcomes {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrQueueFull):
			refused++
		default:
			t.Errorf("submission %s returned %v, want nil or ErrQueueFull", requestID, err)
		}
	}
	if accepted > QueueCapacity+1 {
		t.Errorf("%d submissions were accepted, but at most QueueCapacity+1 = %d commands fit (one running, QueueCapacity queued)",
			accepted, QueueCapacity+1)
	}
	if refused < 1 {
		t.Errorf("%d submissions were refused, want at least one", refused)
	}

	rows := auditRows(t, st)
	if got, want := len(rows)-rowsBefore, accepted; got != want {
		t.Errorf("%d audit rows were written for %d accepted commands; refused commands must write none", got, want)
	}
	byRequest := make(map[string]int, len(rows))
	for _, row := range rows {
		byRequest[row.RequestID]++
	}
	for requestID, err := range outcomes {
		want := 1
		if err != nil {
			want = 0
		}
		if got := byRequest[requestID]; got != want {
			t.Errorf("the audit trail holds %d rows for %s, want %d (its submission returned %v)", got, requestID, want, err)
		}
	}
}

// TestSubmitAfterShutdown proves a stopped worker refuses cleanly: submissions
// return ErrStopped, and a context that is already cancelled returns
// context.Canceled without enqueuing anything.
func TestSubmitAfterShutdown(t *testing.T) {
	st := newTestStore(t)
	w := NewWorker[testState](st)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Errorf("Run did not return within %s of cancellation", testTimeout)
		}
	})

	if _, err := w.Submit(context.Background(), appendCommand(w, "before", "req-before")); err != nil {
		t.Fatalf("the running worker refused a command: %v", err)
	}

	cancel()

	// Bounded poll: the worker stops asynchronously, so wait until Submit
	// reports it rather than assuming the goroutine has already returned.
	deadline := time.Now().Add(testTimeout)
	for {
		_, err := w.Submit(context.Background(), appendCommand(w, "probe", "req-probe"))
		if errors.Is(err, ErrStopped) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Submit never returned ErrStopped within %s; last result was %v", testTimeout, err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatalf("Run did not return within %s of cancellation", testTimeout)
	}

	for i := 0; i < 3; i++ {
		snapshot, err := w.Submit(context.Background(), appendCommand(w, "after", "req-after"))
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("Submit after shutdown returned %v, want ErrStopped", err)
		}
		if !reflect.DeepEqual(snapshot, testState{}) {
			t.Errorf("a stopped Submit returned %+v, want the zero snapshot", snapshot)
		}
	}

	queued := len(w.queue)
	rowsBefore := auditCount(t, st)
	cancelled, cancelSubmit := context.WithCancel(context.Background())
	cancelSubmit()
	snapshot, err := w.Submit(cancelled, appendCommand(w, "cancelled", "req-cancelled"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Submit with a cancelled context returned %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(snapshot, testState{}) {
		t.Errorf("a cancelled Submit returned %+v, want the zero snapshot", snapshot)
	}
	if got := len(w.queue); got != queued {
		t.Errorf("a cancelled Submit changed the queue length from %d to %d: it must not enqueue", queued, got)
	}
	if got := auditCount(t, st); got != rowsBefore {
		t.Errorf("a cancelled Submit wrote %d audit rows", got-rowsBefore)
	}
}

// TestSubmitRejectsNilApply proves a command with no Apply is refused before it
// reaches the queue, so nothing is published and nothing is audited.
func TestSubmitRejectsNilApply(t *testing.T) {
	w, st := newTestWorker(t)

	got, err := w.Submit(context.Background(), Command[testState]{Audit: testAudit("req-no-apply")})
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("Submit returned %v, want ErrInvalidCommand", err)
	}
	if !reflect.DeepEqual(got, testState{}) {
		t.Errorf("Submit returned the snapshot %+v, want the zero value", got)
	}
	if snapshot, ok := w.Snapshot(); ok {
		t.Errorf("a rejected command published the snapshot %+v", snapshot)
	}
	if got := auditCount(t, st); got != 0 {
		t.Errorf("a rejected command wrote %d audit rows, want none", got)
	}
}
