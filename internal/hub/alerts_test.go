package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/store"
)

// eventsNamed returns the decoded records of the stream whose event is name, in
// the order they were written.
func eventsNamed(t *testing.T, buf *bytes.Buffer, name string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("event line is not JSON: %v\n%s", err, line)
		}
		if record["event"] == name {
			found = append(found, record)
		}
	}
	return found
}

// corruptKeyRow overwrites one stored blob with bytes that are long enough to
// be a blob and fail authentication, which is what the drill's
// `UPDATE security_keys SET key_blob = ...` does to a row.
func corruptKeyRow(t *testing.T, st *store.Store, class string) {
	t.Helper()
	err := st.WithTx(context.Background(), func(tx *store.Tx) error {
		row, err := tx.GetKey(context.Background(), class)
		if err != nil {
			return err
		}
		blob := append([]byte(nil), row.Blob...)
		blob[len(blob)-1] ^= 0xff
		return tx.PutKey(context.Background(), class, blob, row.Source, row.ChangedAt)
	})
	if err != nil {
		t.Fatalf("corrupt the %s row: %v", class, err)
	}
}

// TestNetworkStatusEmitsStateChangedOnceAndOnlyOnTransition proves the
// coordinator_unreachable trigger of events.md §5.1: the first derivation
// announces the state it found, an unchanged state is silent, and moving into
// faulted emits a record with to == faulted naming the cause.
func TestNetworkStatusEmitsStateChangedOnceAndOnlyOnTransition(t *testing.T) {
	h, _, buf, id := selectedHub(t)
	ctx := context.Background()

	if _, err := h.NetworkStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.NetworkStatus(ctx); err != nil {
		t.Fatal(err)
	}
	first := eventsNamed(t, buf, "network.state_changed")
	if len(first) != 1 {
		t.Fatalf("two derivations of one state emitted %d state_changed records, want 1", len(first))
	}
	if first[0]["from"] != "unknown" || first[0]["to"] != "unprovisioned" {
		t.Errorf("first record = %v, want unknown → unprovisioned", first[0])
	}

	mustSetConnection(t, h, id, ConnectionReady, &Network{HomeID: "0xdeadbeef", NodeCount: 1})
	// Keys exist, so the stored network is no longer an unknown one.
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("s0"), "req-keys")
	mustSetConnection(t, h, id, ConnectionFaulted, nil)

	status, err := h.NetworkStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != NetworkFaulted {
		t.Fatalf("state = %s, want faulted", status.State)
	}
	records := eventsNamed(t, buf, "network.state_changed")
	last := records[len(records)-1]
	if last["to"] != "faulted" {
		t.Fatalf("last record = %v, want to == faulted", last)
	}
	if last["coordinator_id"] != id {
		t.Errorf("coordinator_id = %v, want %s", last["coordinator_id"], id)
	}
	if reason, _ := last["reason"].(string); reason == "" {
		t.Errorf("record carries no reason: %v", last)
	}
	if level := last["level"]; level != "info" {
		t.Errorf("level = %v, want info", level)
	}

	before := len(records)
	if _, err := h.NetworkStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if after := len(eventsNamed(t, buf, "network.state_changed")); after != before {
		t.Errorf("a repeated faulted state emitted %d more records, want 0", after-before)
	}
}

// TestNetworkStatusReportsStartupFaultedState proves a hub that boots already
// faulted still emits the transition into faulted: the rule keys on to, so a
// first observation of faulted must not be swallowed as "no change".
func TestNetworkStatusReportsStartupFaultedState(t *testing.T) {
	h, st, buf, _ := selectedHub(t)
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("s0"), "req-keys")
	corruptKeyRow(t, st, SecurityClassS0Legacy)

	// A fresh hub over the same store stands in for a restart.
	restarted, err := New(st, h.emitter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.NetworkStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := eventsNamed(t, buf, "network.state_changed")
	last := records[len(records)-1]
	if last["from"] != "unknown" || last["to"] != "faulted" || last["reason"] != "key_store_corrupt" {
		t.Errorf("start-up record = %v, want unknown → faulted for key_store_corrupt", last)
	}
}

// TestCorruptKeyRowEmitsKeysCorruptOnceThroughEveryReader proves the
// key_store_corrupt trigger of events.md §5.2: a row that fails authentication
// emits keys.corrupt naming the class, whichever reader finds it, and the same
// corruption is not re-announced by the next read.
func TestCorruptKeyRowEmitsKeysCorruptOnceThroughEveryReader(t *testing.T) {
	h, st, buf, _ := selectedHub(t)
	ctx := context.Background()
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("s0"), "req-keys")
	corruptKeyRow(t, st, SecurityClassS0Legacy)

	if _, err := h.Keys(ctx); err == nil {
		t.Fatal("Keys read a corrupt store without refusing")
	}
	records := eventsNamed(t, buf, "keys.corrupt")
	if len(records) != 1 {
		t.Fatalf("keys.corrupt records = %d, want 1", len(records))
	}
	record := records[0]
	if record["level"] != "error" {
		t.Errorf("level = %v, want error", record["level"])
	}
	if record["detected_by"] != "aead" {
		t.Errorf("detected_by = %v, want aead", record["detected_by"])
	}
	classes, _ := record["classes"].([]any)
	if len(classes) != 1 || classes[0] != SecurityClassS0Legacy {
		t.Errorf("classes = %v, want [%s]", record["classes"], SecurityClassS0Legacy)
	}

	if _, err := h.NetworkStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.KeyMaterial(ctx); err == nil {
		t.Fatal("KeyMaterial opened a corrupt store")
	}
	if got := len(eventsNamed(t, buf, "keys.corrupt")); got != 1 {
		t.Errorf("the same corruption found by three readers emitted %d records, want 1", got)
	}
}

// TestKeysCorruptAlertsAgainAfterRecovery proves the memory is not permanent: a
// store that is repaired and then corrupted again is a new incident and alerts
// again, which is what lets the operator trust a restored backup.
func TestKeysCorruptAlertsAgainAfterRecovery(t *testing.T) {
	h, st, buf, _ := selectedHub(t)
	ctx := context.Background()
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("s0"), "req-keys")

	if _, err := h.NetworkStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(eventsNamed(t, buf, "keys.corrupt")); got != 0 {
		t.Fatalf("a healthy store emitted %d keys.corrupt records", got)
	}

	corruptKeyRow(t, st, SecurityClassS0Legacy)
	_, _ = h.NetworkStatus(ctx)
	if got := len(eventsNamed(t, buf, "keys.corrupt")); got != 1 {
		t.Fatalf("first corruption emitted %d records, want 1", got)
	}

	// Restore the row, as a backup restore would, and check the store is clean.
	restore := testKeyValue("s0")
	if _, err := h.SubmitKeys(ctx, KeySubmission{
		Keys: map[string]string{SecurityClassS0Legacy: restore}, RequestID: "req-restore", ActorAddr: testActorAddr,
	}); err != nil {
		t.Fatalf("restore the row: %v", err)
	}
	if _, err := h.NetworkStatus(ctx); err != nil {
		t.Fatal(err)
	}

	corruptKeyRow(t, st, SecurityClassS0Legacy)
	_, _ = h.NetworkStatus(ctx)
	if got := len(eventsNamed(t, buf, "keys.corrupt")); got != 2 {
		t.Errorf("a second corruption after recovery emitted %d records in total, want 2", got)
	}
}

// TestKeysCorruptRecordCarriesNoKeyMaterial proves the alert trigger names
// classes only: no blob, no value and no hex run reaches the stream.
func TestKeysCorruptRecordCarriesNoKeyMaterial(t *testing.T) {
	h, st, buf, _ := selectedHub(t)
	value := testKeyValue("s0")
	mustConfigureKey(t, h, SecurityClassS0Legacy, value, "req-keys")
	corruptKeyRow(t, st, SecurityClassS0Legacy)
	_, _ = h.NetworkStatus(context.Background())

	if strings.Contains(buf.String(), value) {
		t.Fatal("the submitted key value appears on the event stream")
	}
}

// TestMonitorEmitsWithoutAClientRead proves the alerts do not depend on someone
// calling GET /network: Monitor derives the status at once and on every tick.
func TestMonitorEmitsWithoutAClientRead(t *testing.T) {
	sink := &lockedBuffer{}
	h, st := newTestHubWith(t, event.New(sink, slog.LevelInfo))
	dev := testDevice("ttyUSB0", "aabbcc", "usb-1")
	mustSync(t, h, dev)
	mustSelect(t, h, dev.Identity.ID, "req-select")
	mustConfigureKey(t, h, SecurityClassS0Legacy, testKeyValue("s0"), "req-keys")
	corruptKeyRow(t, st, SecurityClassS0Legacy)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Monitor(ctx, 10*time.Millisecond) }()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(testTimeout)
	for len(eventsNamed(t, sink.snapshot(), "keys.corrupt")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Monitor never emitted keys.corrupt for a corrupt row")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(eventsNamed(t, sink.snapshot(), "network.state_changed")) == 0 {
		t.Error("Monitor never emitted network.state_changed")
	}
}

// lockedBuffer is a bytes.Buffer safe for the emitter to write from Monitor's
// goroutine while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) snapshot() *bytes.Buffer {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.NewBuffer(append([]byte(nil), b.buf.Bytes()...))
}
