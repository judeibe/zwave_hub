package driver

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Schema negotiation constants (contracts/zwave-server-sidecar.md §3). The hub
// asks for at most PreferredSchema and refuses a server whose maximum is below
// MinimumSchema, because schema 47 is where zwaveChipType and zwaveApiVersion
// appear.
const (
	PreferredSchema = 52
	MinimumSchema   = 47
)

const (
	// retryInterval is the wait between dials while the server answers 1013.
	retryInterval = 500 * time.Millisecond
	// readLimit bounds one frame. start_listening carries every node, which for
	// a large network is a few megabytes.
	readLimit = 32 << 20
)

var (
	// ErrProtocolUnsupported means the server's schema range cannot satisfy the
	// floor; the supervisor reports driver_protocol_unsupported.
	ErrProtocolUnsupported = errors.New("driver: server schema is below the supported floor")
	// ErrClosed means the connection ended.
	ErrClosed = errors.New("driver: connection closed")
)

// CallError is a success:false result. It does not close the socket.
type CallError struct {
	Code    string
	Message string
}

func (e *CallError) Error() string {
	return "driver: command failed: " + e.Code + ": " + e.Message
}

// ServerVersion is the `version` message the server sends first.
type ServerVersion struct {
	DriverVersion    string `json:"driverVersion"`
	ServerVersion    string `json:"serverVersion"`
	HomeID           int64  `json:"homeId"`
	MinSchemaVersion int    `json:"minSchemaVersion"`
	MaxSchemaVersion int    `json:"maxSchemaVersion"`
}

// Event is one `{type:"event"}` message, reduced to what the hub reads.
type Event struct {
	Source string
	Name   string
}

// Client is one WebSocket connection to the sidecar. Dial returns it only
// after the version → initialize → start_listening sequence has completed.
type Client struct {
	conn *websocket.Conn

	// Version is what the server advertised; Schema is what was negotiated.
	Version ServerVersion
	Schema  int
	// State is the start_listening state payload.
	State json.RawMessage

	mu      sync.Mutex
	pending map[string]chan result
	closed  bool
	err     error

	events chan Event
	done   chan struct{}
}

type result struct {
	raw     json.RawMessage
	failure *CallError
}

type frame struct {
	Type      string          `json:"type"`
	MessageID string          `json:"messageId"`
	Success   bool            `json:"success"`
	Result    json.RawMessage `json:"result"`
	ErrorCode string          `json:"errorCode"`
	Message   string          `json:"message"`
	Event     *struct {
		Source string `json:"source"`
		Event  string `json:"event"`
	} `json:"event"`
}

// Dial connects to url, retrying while the server is not ready, until ctx
// ends. "Not ready" is the 1013 close the server sends once the upgrade is
// accepted, and a refused connection while the child is still binding. It then
// performs the handshake and returns a client that routes results by messageId.
func Dial(ctx context.Context, url string) (*Client, error) {
	for {
		c, err := dialOnce(ctx, url)
		if err == nil {
			go c.readLoop()
			return c, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var callErr *CallError
		if errors.Is(err, ErrProtocolUnsupported) || errors.As(err, &callErr) {
			return nil, err
		}
		if !sleep(ctx, retryInterval) {
			return nil, ctx.Err()
		}
	}
}

// dialOnce makes one connection attempt and, when the socket opens, runs the
// handshake on it. The 1013 close surfaces from the first read inside the
// handshake, so it is returned like any other retryable failure.
func dialOnce(ctx context.Context, url string) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(readLimit)
	c := &Client{
		conn:    conn,
		pending: make(map[string]chan result),
		events:  make(chan Event, 64),
		done:    make(chan struct{}),
	}
	if err := c.handshake(ctx); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	return c, nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// handshake reads the version message, negotiates the schema, and runs
// initialize and start_listening synchronously before the read loop starts.
func (c *Client) handshake(ctx context.Context) error {
	var hello frame
	raw, err := c.read(ctx)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &hello); err != nil || hello.Type != "version" {
		return errors.New("driver: first message was not a version message")
	}
	if err := json.Unmarshal(raw, &c.Version); err != nil {
		return fmt.Errorf("driver: decode version message: %w", err)
	}
	schema := min(PreferredSchema, c.Version.MaxSchemaVersion)
	if schema < c.Version.MinSchemaVersion {
		schema = c.Version.MinSchemaVersion
	}
	if schema < MinimumSchema {
		return ErrProtocolUnsupported
	}
	c.Schema = schema

	if _, err := c.syncCall(ctx, "initialize", map[string]any{"schemaVersion": schema}); err != nil {
		return err
	}
	listening, err := c.syncCall(ctx, "start_listening", nil)
	if err != nil {
		return err
	}
	var payload struct {
		State json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(listening, &payload); err != nil || len(payload.State) == 0 {
		return errors.New("driver: start_listening returned no state")
	}
	c.State = payload.State
	return nil
}

// read returns the next frame, converting a 1013 close into a retryable error.
func (c *Client) read(ctx context.Context) ([]byte, error) {
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		if websocket.CloseStatus(err) == websocket.StatusTryAgainLater {
			return nil, errTryAgain
		}
		return nil, err
	}
	return data, nil
}

var errTryAgain = errors.New("driver: server is not ready")

// syncCall is a call made before the read loop runs: it writes the command and
// reads frames until the matching result arrives.
func (c *Client) syncCall(ctx context.Context, command string, args map[string]any) (json.RawMessage, error) {
	id, err := newMessageID()
	if err != nil {
		return nil, err
	}
	if err := c.write(ctx, id, command, args); err != nil {
		return nil, err
	}
	for {
		data, err := c.read(ctx)
		if err != nil {
			return nil, err
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("driver: decode frame: %w", err)
		}
		if f.Type != "result" || f.MessageID != id {
			continue
		}
		if !f.Success {
			return nil, &CallError{Code: f.ErrorCode, Message: f.Message}
		}
		return f.Result, nil
	}
}

func (c *Client) write(ctx context.Context, id, command string, args map[string]any) error {
	message := map[string]any{"messageId": id, "command": command}
	for k, v := range args {
		message[k] = v
	}
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("driver: encode command: %w", err)
	}
	return c.conn.Write(ctx, websocket.MessageText, body)
}

func newMessageID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("driver: generate message id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// Call sends one command and waits for its result. A success:false result is
// a *CallError and leaves the connection open.
func (c *Client) Call(ctx context.Context, command string, args map[string]any) (json.RawMessage, error) {
	id, err := newMessageID()
	if err != nil {
		return nil, err
	}
	ch := make(chan result, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(ctx, id, command, args); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.failure != nil {
			return nil, r.failure
		}
		return r.raw, nil
	case <-c.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Events delivers consumed events (node/added, node/removed, node/ready). It
// is closed when the connection ends; a slow reader drops events rather than
// stalling the read loop, which is safe because the supervisor re-reads state
// on its watchdog.
func (c *Client) Events() <-chan Event { return c.events }

// Done is closed when the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the connection ended, once Done is closed.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) readLoop() {
	defer close(c.events)
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.finish(err)
			return
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.Type {
		case "result":
			c.mu.Lock()
			ch := c.pending[f.MessageID]
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			if f.Success {
				ch <- result{raw: f.Result}
			} else {
				ch <- result{failure: &CallError{Code: f.ErrorCode, Message: f.Message}}
			}
		case "event":
			if f.Event == nil {
				continue
			}
			select {
			case c.events <- Event{Source: f.Event.Source, Name: f.Event.Event}:
			default:
			}
		}
	}
}

func (c *Client) finish(err error) {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.err = err
		close(c.done)
	}
	c.mu.Unlock()
}

// Close ends the connection with a normal close. It is idempotent and does not
// send driver.shutdown, which would put the Z-Wave API into shutdown.
func (c *Client) Close() error {
	err := c.conn.Close(websocket.StatusNormalClosure, "")
	c.finish(ErrClosed)
	return err
}
