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
	// KeyRevision is the key revision the child was started with. It is set on a
	// ready report and is zero when the child ran without keys.
	KeyRevision int
}

// KeySet is the key material one child start is rendered with, and the
// revision it belongs to. Values are raw bytes; the supervisor wipes them once
// the file is written.
type KeySet struct {
	Revision  int
	Keys      map[string][]byte
	LongRange map[string][]byte
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
	// Keys reads the committed key set for the next child start. Nil means the
	// child is started without keys. It is called once per start attempt, so a
	// restart always renders the database as it is now (FR-012).
	Keys func(ctx context.Context) (KeySet, error)
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
	// client is the connection of the ready child, nil until one is ready and
	// again once it ends. It is what a reset runs through.
	client *Client
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

// Resume restarts the supervision loop of the current target without changing
// it. It is what a settled device-set change calls: the same device may work now
// that it has been plugged back in or re-enumerated, and retrying is paused
// until exactly that (R14 §8). It is a no-op when no target is being run, so a
// stray notification cannot start a driver for nothing.
func (s *Supervisor) Resume(ctx context.Context) {
	s.mu.Lock()
	target := s.target
	s.mu.Unlock()
	if target == nil {
		return
	}
	s.Start(ctx, *target)
}

// Apply restarts only the child so it comes back with the keys now in the
// database (FR-012). It is Resume under the name that says why: the target is
// kept, the supervision loop restarts and clears its failure count, and the new
// child's ready report carries the revision it started with. With no target
// there is nothing to restart.
func (s *Supervisor) Apply(ctx context.Context) {
	s.Resume(ctx)
}

// renderKeys reads the key set and writes the config file for one start. With
// no key source, or no keys, there is no file and no --config. The plaintext is
// wiped as soon as the file is written, so the bytes live only as long as the
// render takes.
func (s *Supervisor) renderKeys(ctx context.Context) (KeySet, *ConfigFile, error) {
	if s.opts.Keys == nil {
		return KeySet{}, nil, nil
	}
	set, err := s.opts.Keys(ctx)
	if err != nil {
		return KeySet{}, nil, fmt.Errorf("%w: %v", errKeys, err)
	}
	defer wipe(set.Keys)
	defer wipe(set.LongRange)
	spec := ConfigSpec{CacheDir: s.opts.CacheDir, LogLevel: s.opts.LogLevel, Keys: set.Keys, LongRange: set.LongRange}
	if !spec.HasKeys() {
		return KeySet{Revision: set.Revision}, nil, nil
	}
	file, err := WriteConfig(s.opts.RunDir, spec)
	if err != nil {
		return KeySet{}, nil, fmt.Errorf("%w: %v", errKeys, err)
	}
	return KeySet{Revision: set.Revision}, file, nil
}

// wipe zeroes every value of a key map.
func wipe(values map[string][]byte) {
	for _, value := range values {
		for i := range value {
			value[i] = 0
		}
	}
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

// HardReset asks the live driver to wipe the controller's network
// configuration. A nil error means the request was accepted, never that the
// reset happened: the caller confirms it with ControllerState
// (contracts/zwave-server-sidecar.md §6). With no ready child there is nothing
// to ask, which is ErrClosed: the same answer a call on a closed connection
// gets.
func (s *Supervisor) HardReset(ctx context.Context) error {
	c := s.liveClient()
	if c == nil {
		return ErrClosed
	}
	return c.HardReset(ctx)
}

// ControllerState re-reads the live driver's state. It is the poll a reset
// waits on, so it answers ErrClosed exactly when HardReset would.
func (s *Supervisor) ControllerState(ctx context.Context) (State, error) {
	c := s.liveClient()
	if c == nil {
		return State{}, ErrClosed
	}
	return c.ControllerState(ctx)
}

// setClient binds the ready child's connection.
func (s *Supervisor) setClient(c *Client) {
	s.mu.Lock()
	s.client = c
	s.mu.Unlock()
}

// clearClient unbinds c, leaving a newer child's connection in place: an old
// runOnce finishing after a restart must not unhook the new one.
func (s *Supervisor) clearClient(c *Client) {
	s.mu.Lock()
	if s.client == c {
		s.client = nil
	}
	s.mu.Unlock()
}

// liveClient is the ready child's connection, or nil while none is ready.
func (s *Supervisor) liveClient() *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *Supervisor) run(ctx context.Context, target Target) {
	failures := 0
	for attempt := 1; ctx.Err() == nil; attempt++ {
		s.report(ctx, Report{CoordinatorID: target.CoordinatorID, Connection: ConnectionConnecting})
		began := time.Now()
		err := s.runOnce(ctx, target, attempt)
		if ctx.Err() != nil {
			return
		}
		failures++
		last := &LastError{Code: errorCode(err), Message: errorMessage(err), At: time.Now()}
		var next *time.Time
		connection := ConnectionRetrying
		if failures >= MaxConsecutiveFailures {
			connection = ConnectionFaulted
		} else {
			t := time.Now().Add(s.opts.Backoff(failures))
			next = &t
		}
		if s.opts.Emitter != nil {
			s.opts.Emitter.Emit(ctx, &event.DriverFailed{
				Attempt: attempt, Reason: eventReason(err),
				DurationMS: time.Since(began).Milliseconds(), NextRetryAt: next,
			}, event.Correlation{CoordinatorID: target.CoordinatorID})
			if connection == ConnectionFaulted {
				s.opts.Emitter.Emit(ctx, &event.DriverRestartPaused{
					ConsecutiveFailures: failures, Since: time.Now(),
				}, event.Correlation{CoordinatorID: target.CoordinatorID})
			}
		}
		s.report(ctx, Report{CoordinatorID: target.CoordinatorID, Connection: connection, LastError: last, NextRetryAt: next})
		if connection == ConnectionFaulted {
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
	keySet, configFile, err := s.renderKeys(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = configFile.Remove() }()
	cmd := s.command(ctx, target, port, configFile.Path())
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
	// The client is bound before the ready report, so a caller that hears
	// "ready" can call HardReset and ControllerState straight away, and
	// unbound before the socket closes, so a call in flight fails as closed
	// rather than on a closing socket.
	s.setClient(client)
	defer s.clearClient(client)
	s.report(ctx, Report{
		CoordinatorID: target.CoordinatorID, Connection: ConnectionReady, State: &state,
		SchemaVersion: client.Schema, KeyRevision: keySet.Revision,
	})

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
	errKeys         = errors.New("driver_keys_unavailable")
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

// eventReason maps a failure to the closed driver.failed reason set of
// contracts/events.md §3.4. The API's last-error code is separate: it names the
// cause for an operator, this names the phase for a log reader.
func eventReason(err error) string {
	switch {
	case errors.Is(err, errProtocol):
		return "protocol_unsupported"
	case errors.Is(err, errStartTimeout):
		return "handshake_timeout"
	case errors.Is(err, errSpawn), errors.Is(err, errKeys):
		return "spawn_failed"
	case errors.Is(err, errSocketClosed), errors.Is(err, errExited):
		return "exited"
	default:
		return "start_listening_failed"
	}
}

// errorMessage is a fixed sentence per code: child output can name paths, and a
// last-error field must never carry a log line.
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
// exist: the real server refuses --config without securityKeys.S0_Legacy.
func (s *Supervisor) command(ctx context.Context, target Target, port int, configPath string) *exec.Cmd {
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
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
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
		// A line that looks like key material is dropped entirely rather
		// than masked: it is neither retained in the ring nor emitted, so
		// nothing downstream can recover it (events.md §3.4, R14 §8).
		if event.IsSecret(line) {
			continue
		}
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
