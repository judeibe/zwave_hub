package hub

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/judeibe/zwave_hub/internal/store"
)

const (
	// QueueCapacity is the command queue's depth (research.md R9). A full queue
	// is refused rather than made to wait: the caller gets command_queue_full
	// and writes nothing, so backpressure is visible instead of hiding a
	// twenty-second driver restart behind a request that eventually times out.
	QueueCapacity = 16

	// CommandTimeout bounds one command, including its commit (research.md R9).
	// A command that runs longer is cancelled, its transaction rolls back, and
	// nothing is published.
	CommandTimeout = 35 * time.Second
)

var (
	// ErrQueueFull reports that all QueueCapacity slots were occupied. The API
	// maps it to 503 command_queue_full with Retry-After: 1.
	ErrQueueFull = errors.New("hub: command queue is full")

	// ErrStopped reports that the worker is no longer consuming commands.
	ErrStopped = errors.New("hub: command worker is stopped")

	// ErrInvalidCommand reports a command with no Apply function.
	ErrInvalidCommand = errors.New("hub: command has no apply function")
)

// Command is one mutating operation. S is the hub's snapshot type: Apply builds
// the next snapshot from the state it is changing, and the worker publishes it
// only after the transaction, which includes the audit row, has committed. A
// reader therefore never observes a change that is not on disk.
//
// Apply must use only the transaction it is handed. Returning an error rolls the
// whole command back, audit row included; that is how a database failure or a
// timeout is reported, not how a refused operation is recorded. A refusal is a
// command that succeeds: it leaves state unchanged, carries outcome rejected in
// Audit, and returns nil.
type Command[S any] struct {
	// Audit is the trail row written in the same transaction as Apply. Its
	// action and outcome determine whether the command may be recorded at all.
	Audit store.AuditEntry

	// Apply performs the change and returns the snapshot to publish. It must
	// not call any Store method, because the transaction already holds the
	// store's single connection.
	Apply func(ctx context.Context, tx *store.Tx) (S, error)
}

// jobResult is what the worker hands back to a submitter.
type jobResult[S any] struct {
	snapshot S
	err      error
}

// job is one queued command plus its reply channel. The reply is buffered so
// the worker never blocks when the submitter has already given up.
type job[S any] struct {
	command Command[S]
	reply   chan jobResult[S]
}

// Worker is the serialized command executor: one goroutine consumes a FIFO
// channel of capacity QueueCapacity, runs each command in its own transaction
// with its audit row, and publishes the returned snapshot only after the commit.
// A total order at enqueue is what makes "later wins" well defined (FR-021,
// SC-009), which a mutex cannot provide.
//
// S must be an immutable value: the worker publishes a copy, but maps and
// slices inside it are shared with every reader and must be replaced, never
// mutated. The zero Worker is not usable; build one with NewWorker and start it
// with Run.
type Worker[S any] struct {
	store    *store.Store
	queue    chan *job[S]
	snapshot atomic.Pointer[S]
	stopped  chan struct{}
}

// NewWorker returns a worker that submits through st. It does not start the
// goroutine; call Run once.
func NewWorker[S any](st *store.Store) *Worker[S] {
	return &Worker[S]{
		store:   st,
		queue:   make(chan *job[S], QueueCapacity),
		stopped: make(chan struct{}),
	}
}

// Run consumes commands until ctx is cancelled, then fails every command still
// queued and returns. It is the worker's only goroutine and must be called
// exactly once; nothing else may read from the queue.
func (w *Worker[S]) Run(ctx context.Context) {
	defer close(w.stopped)
	for {
		select {
		case <-ctx.Done():
			w.failQueued(ctx.Err())
			return
		case j := <-w.queue:
			w.execute(ctx, j)
		}
	}
}

// execute runs one command in a transaction and, only on success, publishes its
// snapshot. The audit row is written after Apply inside the same transaction, so
// state and trail commit or roll back together.
func (w *Worker[S]) execute(parent context.Context, j *job[S]) {
	ctx, cancel := context.WithTimeout(parent, CommandTimeout)
	defer cancel()

	var next S
	err := w.store.WithTx(ctx, func(tx *store.Tx) error {
		snapshot, err := j.command.Apply(ctx, tx)
		if err != nil {
			return err
		}
		next = snapshot
		_, err = tx.AppendAudit(ctx, j.command.Audit)
		return err
	})
	if err == nil {
		w.snapshot.Store(&next)
		j.reply <- jobResult[S]{snapshot: next}
		return
	}
	// A failed command hands back nothing: an audit refusal rolls the state
	// change back, so returning it would let a caller render uncommitted state.
	j.reply <- jobResult[S]{err: err}
}

// failQueued answers every command still waiting with err, so no submitter is
// left blocked on a worker that has stopped.
func (w *Worker[S]) failQueued(err error) {
	for {
		select {
		case j := <-w.queue:
			j.reply <- jobResult[S]{err: err}
		default:
			return
		}
	}
}

// Submit enqueues one command and waits for it to commit or fail. A full queue
// returns ErrQueueFull immediately and enqueues nothing, which is the
// command_queue_full refusal of FR-021 and R9. A cancelled ctx returns ctx.Err
// (and, because the command runs under a child of that ctx, its transaction
// rolls back). A stopped worker returns ErrStopped.
func (w *Worker[S]) Submit(ctx context.Context, command Command[S]) (S, error) {
	var zero S
	if command.Apply == nil {
		return zero, ErrInvalidCommand
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}

	j := &job[S]{command: command, reply: make(chan jobResult[S], 1)}
	select {
	case w.queue <- j:
	case <-w.stopped:
		return zero, ErrStopped
	case <-ctx.Done():
		return zero, ctx.Err()
	default:
		return zero, ErrQueueFull
	}

	select {
	case result := <-j.reply:
		return result.snapshot, result.err
	case <-w.stopped:
		// The job may have run just before shutdown; prefer its result.
		select {
		case result := <-j.reply:
			return result.snapshot, result.err
		default:
			return zero, ErrStopped
		}
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// Snapshot returns the last committed snapshot and whether one exists. Readers
// call it without touching the queue, so a read never waits behind a command.
func (w *Worker[S]) Snapshot() (S, bool) {
	published := w.snapshot.Load()
	if published == nil {
		var zero S
		return zero, false
	}
	return *published, true
}
