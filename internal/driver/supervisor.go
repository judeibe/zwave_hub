package driver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/judeibe/zwave_hub/internal/event"
)

const (
	// StartDeadline bounds spawn → start_listening success.
	StartDeadline = 30 * time.Second
	// StopGrace is how long SIGTERM gets before SIGKILL.
	StopGrace = 10 * time.Second
	// MaxConsecutiveFailures pauses retrying until the device set changes.
	MaxConsecutiveFailures = 5

	maxRetainedLines = 200
)

// Target is the device the supervisor runs a driver for.
type Target struct {
	// CoordinatorID is the identity events and reports are attributed to.
	CoordinatorID string
	// SerialPath is the by-id link when one exists.
	SerialPath string
}

// Report is what a start attempt produced. Connection is one of the hub's
// connection states, as text, so this package does not import the hub.
type Report struct {
	CoordinatorID string
	Connection    string
	State         *State
	LastError     *LastError
	NextRetryAt   *time.Time
	SchemaVersion int
}

// LastError is the cause of the latest failure, for the last-error field.
type LastError struct {
	Code    string
	Message string
	At      time.Time
}

// Options configures a Supervisor. Every value is injected (constitution III).
type Options struct {
	// SidecarDir holds node_modules/@zwave-js/server.
	SidecarDir string
	// NodePath is the node binary; empty means "node" on PATH.
	NodePath string
	// Mock passes --mock-driver instead of a serial path.
	Mock bool
	// RunDir is where the rendered config is written (/run/zwave-hub).
	RunDir string
	// CacheDir is storage.cacheDir.
	CacheDir string
	// LogLevel is logConfig.level.
	LogLevel string
	// Emitter receives the driver.* events; nil disables them.
	Emitter *event.Emitter
	// Report receives each connection change. It is called from the
	// supervisor's goroutine and must not block for long.
	Report func(context.Context, Report)
	// Backoff overrides the retry schedule; nil uses 1, 2, 4, 8, 16, 30 s.
	Backoff func(failures int) time.Duration
	// StartDeadline overrides the 30 s default for tests.
	StartDeadline time.Duration
}

// Supervisor runs at most one sidecar child at a time for the current target.
type Supervisor struct {
	opts Options

	mu      sync.Mutex
	target  *Target
	cancel  context.CancelFunc
	stopped chan struct{}
	lines   []string
}

// NewSupervisor validates opts.
func NewSupervisor(opts Options) (*Supervisor, error) {
	switch {
	case opts.SidecarDir == "":
		return nil, errors.New("driver: sidecar directory is empty")
	case opts.RunDir == "":
		return nil, errors.New("driver: run directory is empty")
	case opts.CacheDir == "":
		return nil, errors.New("driver: cache directory is empty")
	case opts.Report == nil:
		return nil, errors.New("driver: report function is nil")
	}
	if opts.StartDeadline <= 0 {
		opts.StartDeadline = StartDeadline
	}
	if opts.Backoff == nil {
		opts.Backoff = defaultBackoff
	}
	return &Supervisor{opts: opts}, nil
}

func defaultBackoff(failures int) time.Duration {
	steps := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	return steps[min(max(failures-1, 0), len(steps)-1)]
}

// Start runs the driver for target, first stopping any previous one. It
// returns immediately; progress arrives through Options.Report.
func (s *Supervisor) Start(ctx context.Context, target Target) {
	s.Stop()
	runCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	s.mu.Lock()
	s.target = &target
	s.cancel = cancel
	s.stopped = stopped
	s.mu.Unlock()
	go func() {
		defer close(stopped)
		s.run(runCtx, target)
	}()
}

// Stop ends the current child and waits until it has exited. It is idempotent.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	cancel, stopped := s.cancel, s.stopped
	s.target, s.cancel, s.stopped = nil, nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-stopped
}

// Current returns the target being run, if any.
func (s *Supervisor) Current() (Target, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.target == nil {
		return Target{}, false
	}
	return *s.target, true
}

func (s *Supervisor) run(ctx context.Context, target Target) {
	failures := 0
	for attempt := 1; ctx.Err() == nil; attempt++ {
		s.report(ctx, Report{CoordinatorID: target.CoordinatorID, Connection: "connecting"})
		began := time.Now()
		err := s.runOnce(ctx, target, attempt)
		if ctx.Err() != nil {
			return
		}
		failures++
		last := &LastError{Code: errorCode(err), Message: errorMessage(err), At: time.Now()}
		var next *time.Time
		connection := "retrying"
		if failures >= MaxConsecutiveFailures {
			connection = "faulted"
		} else {
			t := time.Now().Add(s.opts.Backoff(failures))
			next = &t
		}
		if s.opts.Emitter != nil {
			s.opts.Emitter.Emit(ctx, &event.DriverFailed{
				Attempt: attempt, Reason: eventReason(err),
				DurationMS: time.Since(began).Milliseconds(), NextRetryAt: next,
			}, event.Correlation{CoordinatorID: target.CoordinatorID})
			if connection == "faulted" {
				s.opts.Emitter.Emit(ctx, &event.DriverRestartPaused{
					ConsecutiveFailures: failures, Since: time.Now(),
				}, event.Correlation{CoordinatorID: target.CoordinatorID})
			}
		}
		s.report(ctx, Report{CoordinatorID: target.CoordinatorID, Connection: connection, LastError: last, NextRetryAt: next})
		if connection == "faulted" {
			return
		}
		if !sleep(ctx, time.Until(*next)) {
			return
		}
	}
}

func (s *Supervisor) report(ctx context.Context, r Report) {
	s.opts.Report(ctx, r)
}

// runOnce spawns one child, drives it to ready and waits until it ends.
func (s *Supervisor) runOnce(ctx context.Context, target Target, attempt int) error {
	port, err := reservePort()
	if err != nil {
		return err
	}
	cmd := s.command(ctx, target, port)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("driver: stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = StopGrace
	began := time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: %v", errSpawn, err)
	}
	child := &childProcess{cmd: cmd, gone: make(chan struct{})}
	go func() {
		s.forwardLogs(ctx, stdout, target)
		child.err = cmd.Wait()
		close(child.gone)
	}()

	// A child that exits before it is ready ends the dial at once instead of
	// leaving it to retry until the start deadline.
	dialCtx, cancel := context.WithTimeout(ctx, s.opts.StartDeadline)
	defer cancel()
	go func() {
		select {
		case <-child.gone:
			cancel()
		case <-dialCtx.Done():
		}
	}()
	client, err := Dial(dialCtx, fmt.Sprintf("ws://127.0.0.1:%d", port))
	if err != nil {
		child.stop()
		switch {
		case isGone(child):
			return fmt.Errorf("%w before ready: %v", errExited, errOrExit(child.err))
		case errors.Is(err, ErrProtocolUnsupported):
			return errProtocol
		case errors.Is(err, context.DeadlineExceeded):
			return errStartTimeout
		}
		return err
	}
	defer func() { _ = client.Close() }()

	state, err := ParseState(client.State)
	if err != nil {
		child.stop()
		return err
	}
	if s.opts.Emitter != nil {
		corr := event.Correlation{CoordinatorID: target.CoordinatorID, HomeID: state.HomeID}
		s.opts.Emitter.Emit(ctx, &event.DriverStarting{Attempt: attempt, SerialPath: target.SerialPath, SchemaVersion: client.Schema}, corr)
		s.opts.Emitter.Emit(ctx, &event.DriverReady{
			SchemaVersion: client.Schema, StartMS: time.Since(began).Milliseconds(),
			NodeCount: state.NodeCount, SecureNodeCount: state.SecureNodeCount,
		}, corr)
	}
	s.report(ctx, Report{CoordinatorID: target.CoordinatorID, Connection: "ready", State: &state, SchemaVersion: client.Schema})

	select {
	case <-ctx.Done():
		child.stop()
		if s.opts.Emitter != nil {
			s.opts.Emitter.Emit(context.WithoutCancel(ctx), &event.DriverStopped{Reason: "shutdown"}, event.Correlation{CoordinatorID: target.CoordinatorID})
		}
		return ctx.Err()
	case <-client.Done():
		child.stop()
		return errSocketClosed
	case <-child.gone:
		return fmt.Errorf("%w: %v", errExited, errOrExit(child.err))
	}
}

// childProcess is one running sidecar. gone is closed once the process has
// exited and its output has been drained; err is valid after that.
type childProcess struct {
	cmd  *exec.Cmd
	gone chan struct{}
	err  error
}

func isGone(c *childProcess) bool {
	select {
	case <-c.gone:
		return true
	default:
		return false
	}
}

// stop sends SIGTERM and waits for exit, escalating to SIGKILL after
// StopGrace. It never asks the driver to shut down over the socket.
func (c *childProcess) stop() {
	if isGone(c) {
		return
	}
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.gone:
	case <-time.After(StopGrace):
		_ = c.cmd.Process.Kill()
		<-c.gone
	}
}

var (
	errProtocol     = errors.New("driver_protocol_unsupported")
	errStartTimeout = errors.New("driver_start_timeout")
	errSocketClosed = errors.New("driver_socket_closed")
	errSpawn        = errors.New("driver_spawn_failed")
	errExited       = errors.New("driver_exited")
)

func errOrExit(err error) error {
	if err == nil {
		return errors.New("exit status 0")
	}
	return err
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, errProtocol):
		return "driver_protocol_unsupported"
	case errors.Is(err, errStartTimeout):
		return "driver_start_timeout"
	case errors.Is(err, errSocketClosed):
		return "driver_socket_closed"
	default:
		return "driver_start_failed"
	}
}

// errorMessage is a fixed sentence per code: child output can name paths, and a
// last-error field must never carry a log line.
// eventReason maps a failure to the closed driver.failed reason set of
// contracts/events.md §3.4. The API's last-error code is separate: it names the
// cause for an operator, this names the phase for a log reader.
func eventReason(err error) string {
	switch {
	case errors.Is(err, errProtocol):
		return "protocol_unsupported"
	case errors.Is(err, errStartTimeout):
		return "handshake_timeout"
	case errors.Is(err, errSpawn):
		return "spawn_failed"
	case errors.Is(err, errSocketClosed), errors.Is(err, errExited):
		return "exited"
	default:
		return "start_listening_failed"
	}
}

func errorMessage(err error) string {
	switch errorCode(err) {
	case "driver_protocol_unsupported":
		return "The sidecar's protocol schema is older than the supported floor."
	case "driver_start_timeout":
		return "The driver did not become ready before the start deadline."
	case "driver_socket_closed":
		return "The driver connection closed unexpectedly."
	default:
		return "The driver process failed to start or exited."
	}
}

// command builds the child invocation of contracts/zwave-server-sidecar.md §1
// with an allow-listed environment. The config file is only passed once keys
// exist: the real server refuses --config without securityKeys.S0_Legacy, and
// this phase has no keys to render.
func (s *Supervisor) command(ctx context.Context, target Target, port int) *exec.Cmd {
	node := s.opts.NodePath
	if node == "" {
		node = "node"
	}
	entry := filepath.Join(s.opts.SidecarDir, "node_modules", "@zwave-js", "server", "dist-esm", "bin", "server.js")
	args := []string{entry}
	if s.opts.Mock {
		args = append(args, "--mock-driver")
	} else {
		args = append(args, target.SerialPath)
	}
	args = append(args, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--disable-dns-sd")
	if !s.opts.Mock {
		args = append(args, "--reconnect")
	}
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"NODE_ENV=production",
		"TZ=UTC",
		"HOME=/nonexistent",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// reservePort binds 127.0.0.1:0, reads the port and closes it.
func reservePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("driver: reserve port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// forwardLogs reads the child's output, forwards each line as driver.log and
// keeps the last maxRetainedLines in memory.
func (s *Supervisor) forwardLogs(ctx context.Context, r io.Reader, target Target) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		s.mu.Lock()
		s.lines = append(s.lines, line)
		if len(s.lines) > maxRetainedLines {
			s.lines = s.lines[len(s.lines)-maxRetainedLines:]
		}
		s.mu.Unlock()
		if s.opts.Emitter != nil {
			s.opts.Emitter.Emit(ctx, &event.DriverLog{Severity: slog.LevelInfo, Line: line},
				event.Correlation{CoordinatorID: target.CoordinatorID})
		}
	}
}
