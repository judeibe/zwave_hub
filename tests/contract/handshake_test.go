//go:build contract

// Package contract tests the zwave-server sidecar at the same boundary the hub
// uses: a real --mock-driver child for the handshake, and an in-test WebSocket
// server where the retry and schema rules have to be exact
// (specs/001-coordinator-key-provisioning/contracts/zwave-server-sidecar.md
// §3, §8). The build tag keeps node out of `go test ./...`.
package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/judeibe/zwave_hub/internal/driver"
)

const (
	// handshakeTimeout bounds one Dial, matching the supervisor's start
	// deadline (§7).
	handshakeTimeout = 30 * time.Second
	// retryInterval is the client's wait between 1013 retries (§3), repeated
	// here only so the raw-websocket dial can pace itself the same way.
	retryInterval = 500 * time.Millisecond
)

// fakeState is the smallest start_listening payload ParseState accepts.
var fakeState = json.RawMessage(`{"controller":{"homeId":1},"nodes":[]}`)

// wireFrame is the subset of a server frame these tests read.
type wireFrame struct {
	Type             string `json:"type"`
	MessageID        string `json:"messageId"`
	Success          bool   `json:"success"`
	ErrorCode        string `json:"errorCode"`
	DriverVersion    string `json:"driverVersion"`
	ServerVersion    string `json:"serverVersion"`
	MinSchemaVersion int    `json:"minSchemaVersion"`
	MaxSchemaVersion int    `json:"maxSchemaVersion"`
}

// wireCommand is the subset of a hub command these tests read and record.
type wireCommand struct {
	MessageID     string `json:"messageId"`
	Command       string `json:"command"`
	SchemaVersion int    `json:"schemaVersion"`
}

func TestHandshakeAgainstMockDriver(t *testing.T) {
	child := startMockDriver(t)

	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	client, err := driver.Dial(ctx, child.url)
	if err != nil {
		t.Fatalf("Dial %s: %v", child.url, err)
	}
	defer func() { _ = client.Close() }()

	t.Logf("server %s driver %s schema %d (server range %d..%d)",
		client.Version.ServerVersion, client.Version.DriverVersion, client.Schema,
		client.Version.MinSchemaVersion, client.Version.MaxSchemaVersion)

	if client.Version.ServerVersion == "" {
		t.Error("version message carried an empty serverVersion")
	}
	if client.Version.DriverVersion == "" {
		t.Error("version message carried an empty driverVersion")
	}
	wantSchema := min(driver.PreferredSchema, client.Version.MaxSchemaVersion)
	if client.Schema != wantSchema {
		t.Errorf("negotiated schema = %d, want %d = min(%d, server max %d)",
			client.Schema, wantSchema, driver.PreferredSchema, client.Version.MaxSchemaVersion)
	}
	if client.Schema < driver.MinimumSchema {
		t.Errorf("negotiated schema = %d, below the floor %d", client.Schema, driver.MinimumSchema)
	}

	state, err := driver.ParseState(client.State)
	if err != nil {
		t.Fatalf("ParseState: %v", err)
	}
	if state.HomeID != "0x00000001" {
		t.Errorf("home id = %q, want 0x00000001", state.HomeID)
	}
	if state.NodeCount != 0 {
		t.Errorf("node count = %d, want 0", state.NodeCount)
	}
}

func TestDialRetriesWhileNotReady(t *testing.T) {
	fake := &fakeServer{refuse: 2, maxSchema: driver.PreferredSchema}
	url := newFakeServer(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	began := time.Now()
	client, err := driver.Dial(ctx, url)
	elapsed := time.Since(began)
	if err != nil {
		t.Fatalf("Dial after two 1013 closes: %v", err)
	}
	defer func() { _ = client.Close() }()

	attempts, _, _ := fake.snapshot()
	if attempts != 3 {
		t.Errorf("connection attempts = %d, want 3 (two 1013 closes, then one served)", attempts)
	}
	// Two retries cost at least two 500 ms waits; a Dial that gave up early or
	// reused a closed socket would come back sooner.
	if elapsed < time.Second {
		t.Errorf("Dial returned after %v, want at least 1 s of retry waits", elapsed)
	}
	if client.Schema != driver.PreferredSchema {
		t.Errorf("negotiated schema = %d, want %d", client.Schema, driver.PreferredSchema)
	}
}

func TestSchemaNegotiation(t *testing.T) {
	cases := []struct {
		name       string
		minSchema  int
		maxSchema  int
		wantSchema int
		wantErr    bool
	}{
		{name: "above preferred", minSchema: 0, maxSchema: 60, wantSchema: 52},
		{name: "at preferred", minSchema: 0, maxSchema: 52, wantSchema: 52},
		{name: "at floor", minSchema: 0, maxSchema: driver.MinimumSchema, wantSchema: driver.MinimumSchema},
		{name: "below floor", minSchema: 0, maxSchema: driver.MinimumSchema - 1, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeServer{minSchema: tc.minSchema, maxSchema: tc.maxSchema}
			url := newFakeServer(t, fake)

			ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
			defer cancel()

			began := time.Now()
			client, err := driver.Dial(ctx, url)
			elapsed := time.Since(began)

			attempts, commands, initializeSchema := fake.snapshot()

			if tc.wantErr {
				if !errors.Is(err, driver.ErrProtocolUnsupported) {
					t.Fatalf("Dial err = %v, want driver.ErrProtocolUnsupported", err)
				}
				if elapsed >= 5*time.Second {
					t.Errorf("Dial took %v to reject the floor; it must not retry", elapsed)
				}
				if attempts != 1 {
					t.Errorf("connection attempts = %d, want 1: a schema below the floor is terminal", attempts)
				}
				if slices.Contains(commands, "initialize") {
					t.Errorf("commands = %v, want no initialize below the floor", commands)
				}
				return
			}

			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer func() { _ = client.Close() }()

			if client.Schema != tc.wantSchema {
				t.Errorf("negotiated schema = %d, want %d", client.Schema, tc.wantSchema)
			}
			if initializeSchema != tc.wantSchema {
				t.Errorf("initialize sent schemaVersion %d, want the negotiated %d", initializeSchema, tc.wantSchema)
			}
			if want := []string{"initialize", "start_listening"}; !slices.Equal(commands, want) {
				t.Errorf("handshake commands = %v, want %v", commands, want)
			}
			// §4: driver.shutdown puts the Z-Wave API into shutdown and is
			// never sent.
			if slices.Contains(commands, "driver.shutdown") {
				t.Errorf("handshake sent driver.shutdown: %v", commands)
			}
		})
	}
}

func TestInitializeOutsideRangeIsRefused(t *testing.T) {
	child := startMockDriver(t)

	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	conn, hello, err := dialRaw(ctx, child.url)
	if err != nil {
		t.Fatalf("raw dial %s: %v", child.url, err)
	}
	defer func() { _ = conn.CloseNow() }()

	if hello.MaxSchemaVersion < driver.MinimumSchema {
		t.Fatalf("server max schema = %d, below the floor %d", hello.MaxSchemaVersion, driver.MinimumSchema)
	}

	sendCommand(t, ctx, conn, "outside-range", "initialize", hello.MaxSchemaVersion+1)
	refused := readResult(t, ctx, conn, "outside-range")
	if refused.Success {
		t.Fatalf("initialize with schemaVersion %d succeeded, want a failed result",
			hello.MaxSchemaVersion+1)
	}
	if refused.ErrorCode == "" {
		t.Fatal("failed result carried an empty errorCode")
	}

	// §3: a failed result does not close the socket, so the same connection
	// still serves a valid initialize. This is what makes the refusal terminal
	// for the hub without also being terminal for the connection.
	sendCommand(t, ctx, conn, "in-range", "initialize", driver.PreferredSchema)
	accepted := readResult(t, ctx, conn, "in-range")
	if !accepted.Success {
		t.Fatalf("initialize with schemaVersion %d failed on the same connection: %s",
			driver.PreferredSchema, accepted.ErrorCode)
	}
}

// mockDriver is a real zwave-server child running --mock-driver.
type mockDriver struct {
	url  string
	cmd  *exec.Cmd
	out  *lockedBuffer
	wait chan struct{}
	err  error
}

// startMockDriver spawns the sidecar the way the supervisor does (§1) and
// returns it once its WebSocket is listening. It skips, naming the missing
// piece, when node or sidecar/node_modules is absent.
func startMockDriver(t *testing.T) *mockDriver {
	t.Helper()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	entry := filepath.Join(root, "sidecar", "node_modules", "@zwave-js", "server", "dist-esm", "bin", "server.js")
	if _, err := os.Stat(entry); err != nil {
		t.Skipf("zwave-server is not installed (%v); run npm ci in sidecar/", err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the contract suite runs the real sidecar")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}

	cmd := exec.Command(node, entry,
		"--mock-driver",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--disable-dns-sd",
	)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"NODE_ENV=production",
		"TZ=UTC",
		"HOME=/nonexistent",
	}
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start zwave-server: %v", err)
	}

	d := &mockDriver{
		url:  fmt.Sprintf("ws://127.0.0.1:%d", port),
		cmd:  cmd,
		out:  out,
		wait: make(chan struct{}),
	}
	go func() {
		d.err = cmd.Wait()
		close(d.wait)
	}()
	t.Cleanup(func() { d.stop(t) })
	d.awaitListening(t, handshakeTimeout)
	return d
}

// awaitListening waits for the line the server prints once its WebSocket is
// bound, so a child that died fails the test with its output instead of
// dialling a dead port for the whole handshake timeout.
func (d *mockDriver) awaitListening(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if strings.Contains(d.out.String(), "listening on") {
			return
		}
		select {
		case <-d.wait:
			t.Fatalf("zwave-server exited before it listened: %v\n%s", d.err, d.out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("zwave-server did not report listening within %v\n%s", within, d.out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop sends SIGTERM and escalates to SIGKILL after driver.StopGrace, then
// waits for the child (§1 exit handling). It never sends driver.shutdown.
func (d *mockDriver) stop(t *testing.T) {
	t.Helper()
	select {
	case <-d.wait:
	default:
		_ = d.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-d.wait:
		case <-time.After(driver.StopGrace):
			_ = d.cmd.Process.Kill()
			<-d.wait
		}
	}
	if t.Failed() {
		t.Logf("zwave-server output:\n%s", d.out.String())
	}
}

// fakeServer is an in-test zwave-server. It closes the first refuse
// connections with 1013, then serves the handshake and records what the client
// sent, so retry and schema behaviour are asserted without a Node process.
type fakeServer struct {
	refuse    int
	minSchema int
	maxSchema int

	mu               sync.Mutex
	attempts         int
	commands         []string
	initializeSchema int
}

func newFakeServer(t *testing.T, f *fakeServer) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	f.mu.Lock()
	f.attempts++
	attempt := f.attempts
	f.mu.Unlock()

	if attempt <= f.refuse {
		_ = conn.Close(websocket.StatusTryAgainLater, "Driver is not ready")
		return
	}

	// The request context ends when the handler returns, so the connection
	// outlives it only through a context of its own.
	ctx := context.Background()
	hello := map[string]any{
		"type":             "version",
		"homeId":           1,
		"driverVersion":    "15.32.3",
		"serverVersion":    "3.12.0",
		"minSchemaVersion": f.minSchema,
		"maxSchemaVersion": f.maxSchema,
	}
	if err := writeJSON(ctx, conn, hello); err != nil {
		return
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var cmd wireCommand
		if err := json.Unmarshal(data, &cmd); err != nil {
			continue
		}
		f.mu.Lock()
		f.commands = append(f.commands, cmd.Command)
		if cmd.Command == "initialize" {
			f.initializeSchema = cmd.SchemaVersion
		}
		f.mu.Unlock()

		switch cmd.Command {
		case "initialize":
			err = writeJSON(ctx, conn, resultFrame(cmd.MessageID, map[string]any{}))
		case "start_listening":
			err = writeJSON(ctx, conn, resultFrame(cmd.MessageID, map[string]any{"state": fakeState}))
		}
		if err != nil {
			return
		}
	}
}

func (f *fakeServer) snapshot() (attempts int, commands []string, initializeSchema int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts, slices.Clone(f.commands), f.initializeSchema
}

func resultFrame(messageID string, result any) map[string]any {
	return map[string]any{
		"type":      "result",
		"success":   true,
		"messageId": messageID,
		"result":    result,
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, body)
}

// dialRaw connects to a real child, retrying the 1013 close and a refused
// connection the way driver.Dial does, and returns the connection with the
// version frame already read.
func dialRaw(ctx context.Context, url string) (*websocket.Conn, wireFrame, error) {
	for {
		conn, _, err := websocket.Dial(ctx, url, nil)
		if err == nil {
			hello, readErr := readVersion(ctx, conn)
			if readErr == nil {
				return conn, hello, nil
			}
			_ = conn.CloseNow()
			if websocket.CloseStatus(readErr) != websocket.StatusTryAgainLater {
				return nil, wireFrame{}, readErr
			}
		} else if ctx.Err() != nil {
			return nil, wireFrame{}, ctx.Err()
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, wireFrame{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func readVersion(ctx context.Context, conn *websocket.Conn) (wireFrame, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return wireFrame{}, err
	}
	var f wireFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return wireFrame{}, fmt.Errorf("decode first frame: %w", err)
	}
	if f.Type != "version" {
		return wireFrame{}, fmt.Errorf("first frame type = %q, want version", f.Type)
	}
	return f, nil
}

func sendCommand(t *testing.T, ctx context.Context, conn *websocket.Conn, messageID, command string, schemaVersion int) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"messageId":     messageID,
		"command":       command,
		"schemaVersion": schemaVersion,
	})
	if err != nil {
		t.Fatalf("encode %s: %v", command, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatalf("send %s: %v", command, err)
	}
}

func readResult(t *testing.T, ctx context.Context, conn *websocket.Conn, messageID string) wireFrame {
	t.Helper()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read result for %s: %v", messageID, err)
		}
		var f wireFrame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("decode frame while waiting for %s: %v", messageID, err)
		}
		if f.Type == "result" && f.MessageID == messageID {
			return f
		}
	}
}

// lockedBuffer collects child output; the child writes while the test may read
// it for a failure message.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
