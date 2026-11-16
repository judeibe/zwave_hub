package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

// This file records and measures the metadata-read budget of plan.md
// §Performance Goals: p95 < 100 ms for GET /api/v1/coordinators, GET /api/v1/keys
// and GET /api/v1/network (T075). Every request goes through the real router of
// handlers_test.go in process, so what is measured is the handler's own cost —
// routing, presentation and the store reads behind them — and not loopback TCP.

const (
	// metadataSerial is the coordinator the fixtures select: the example
	// identity of contracts/openapi.yaml.
	metadataSerial = "00a1b2c3"
	// metadataHomeID is the network the fixture's ready report carries, as the
	// restart tests' reports do.
	metadataHomeID = "0x00000001"
	// metadataSamples is the sample count TestMetadataReadP95 records p95 over.
	metadataSamples = 200
	// metadataWarmup is how many unrecorded requests run before the samples, so
	// route matching, the first store page and the allocator's growth are not
	// counted as steady-state latency.
	metadataWarmup = 5
	// metadataTarget is the p95 bound of plan.md §Performance Goals.
	metadataTarget = 100 * time.Millisecond
)

// metadataKeys returns the full four-class classic set the fixtures submit
// through PUT /api/v1/keys, with the values restart_test.go submits: one
// distinct value per class, because a set that shares a value between two
// classes is refused (FR-011).
func metadataKeys() map[string]string {
	return map[string]string{
		hub.SecurityClassS2AccessControl:   "00112233445566778899aabbccddeeff",
		hub.SecurityClassS2Authenticated:   "102132435465768798a9bacbdcedfe0f",
		hub.SecurityClassS2Unauthenticated: "2031425364758697a8b9cadbecfd0e1f",
		hub.SecurityClassS0Legacy:          "30415263748596a7b8c9daebfc0d1e2f",
	}
}

// metadataSubmissionBody encodes the PUT /api/v1/keys body in the shape
// keys_test.go's keysTestSubmissionBody submits: the keys member alone, which
// the contract reads as merge. It returns an error rather than failing a test
// because the benchmarks build the body too, and a benchmark holds no
// *testing.T.
func metadataSubmissionBody(keys map[string]string) (string, error) {
	body, err := json.Marshal(struct {
		Keys map[string]string `json:"keys"`
	}{Keys: keys})
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// newBenchmarkFixture builds for a benchmark what newHandlerFixture builds for
// a test: a real SQLite store in a temporary directory, a hub running over it,
// and the same five routers on the same route table. The construction is
// repeated here because newHandlerFixture takes a *testing.T and a benchmark
// holds only a *testing.B; the fixture type and its request helper are the ones
// every other API test drives.
func newBenchmarkFixture(b *testing.B) *handlerFixture {
	b.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DataDir:   b.TempDir(),
		MasterKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		b.Fatal(err)
	}
	h, err := hub.New(st, nil)
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	b.Cleanup(func() {
		cancel()
		<-done
		_ = st.Close()
	})
	rt := NewRouter(Options{})
	NewCoordinators(h).Mount(rt)
	NewNetwork(h).Mount(rt)
	NewKeys(h).Mount(rt)
	NewConfirmations(h, true).Mount(rt)
	NewAudit(st).Mount(rt)
	return &handlerFixture{hub: h, store: st, router: rt}
}

// prepareMetadataState drives a fixture into the state the metadata budget is
// recorded against: one selected coordinator that has reported ready, and the
// complete classic key set supplied, so no endpoint under measurement renders
// an empty body. It returns an error rather than failing a test because
// TestMetadataReadP95 and the benchmarks share it.
func prepareMetadataState(f *handlerFixture) error {
	ctx := context.Background()
	dev := device(metadataSerial)
	if err := f.hub.SyncDiscovery(ctx, []coordinator.Device{dev}); err != nil {
		return fmt.Errorf("sync discovery: %w", err)
	}
	selectBody := `{"coordinator_id":"` + dev.Identity.ID + `"}`
	if rec := f.do("PUT", "/api/v1/active-coordinator", selectBody); rec.Code != http.StatusOK {
		return fmt.Errorf("select coordinator: status %d: %s", rec.Code, rec.Body)
	}
	// The fixture binds no driver supervisor, so the ready report is made
	// directly through Adopt, exactly as the fixtures of handlers_test.go make
	// it: without the report GET /network renders an unprovisioned body.
	if err := f.hub.Adopt(ctx, dev.Identity.ID, hub.Identified{
		Network: hub.Network{HomeID: metadataHomeID},
	}); err != nil {
		return fmt.Errorf("adopt coordinator: %w", err)
	}
	body, err := metadataSubmissionBody(metadataKeys())
	if err != nil {
		return fmt.Errorf("encode key submission: %w", err)
	}
	if rec := f.do("PUT", "/api/v1/keys", body); rec.Code != http.StatusOK {
		return fmt.Errorf("submit keys: status %d: %s", rec.Code, rec.Body)
	}
	return checkMetadataState(ctx, f, dev.Identity.ID)
}

// checkMetadataState verifies the values the three endpoints present, so a
// benchmark never measures an empty render: the coordinator is listed and
// selected, and the four classic classes are configured.
func checkMetadataState(ctx context.Context, f *handlerFixture, id string) error {
	snap := f.hub.Snapshot()
	if len(snap.Coordinators) != 1 || snap.ActiveID != id {
		return fmt.Errorf("fixture holds %d coordinators with %q active, want the one device %q",
			len(snap.Coordinators), snap.ActiveID, id)
	}
	cfg, err := f.hub.Keys(ctx)
	if err != nil {
		return fmt.Errorf("read the key configuration: %w", err)
	}
	configured := 0
	for _, class := range cfg.Classes {
		if class.Configured {
			configured++
		}
	}
	if want := len(hub.ClassicClasses()); configured != want || !cfg.ClassicComplete {
		return fmt.Errorf("fixture holds %d configured classes (classic complete %t), want %d",
			configured, cfg.ClassicComplete, want)
	}
	return nil
}

// benchmarkMetadataFixture is newBenchmarkFixture in the state
// TestMetadataReadP95 records, so a benchmark measures the same populated
// renders the recorded p95 does.
func benchmarkMetadataFixture(b *testing.B) *handlerFixture {
	b.Helper()
	f := newBenchmarkFixture(b)
	if err := prepareMetadataState(f); err != nil {
		b.Fatal(err)
	}
	return f
}

// BenchmarkListCoordinators measures GET /api/v1/coordinators, whose list
// presentation plan.md §Performance Goals bounds at 100 ms p95 (T075). The
// fixture lists and selects one ready coordinator.
func BenchmarkListCoordinators(b *testing.B) {
	f := benchmarkMetadataFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if rec := f.do("GET", "/api/v1/coordinators", ""); rec.Code != http.StatusOK {
			b.Fatalf("GET /api/v1/coordinators = %d: %s", rec.Code, rec.Body)
		}
	}
}

// BenchmarkGetKeyConfiguration measures GET /api/v1/keys, the per-class
// metadata of the six classes, against the same 100 ms p95 target (T075). The
// fixture holds the four classic classes as supplied keys.
func BenchmarkGetKeyConfiguration(b *testing.B) {
	f := benchmarkMetadataFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if rec := f.do("GET", "/api/v1/keys", ""); rec.Code != http.StatusOK {
			b.Fatalf("GET /api/v1/keys = %d: %s", rec.Code, rec.Body)
		}
	}
}

// BenchmarkGetNetworkStatus measures GET /api/v1/network, the derived state,
// its blocking reasons and the key metadata block, against the same 100 ms p95
// target (T075).
func BenchmarkGetNetworkStatus(b *testing.B) {
	f := benchmarkMetadataFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if rec := f.do("GET", "/api/v1/network", ""); rec.Code != http.StatusOK {
			b.Fatalf("GET /api/v1/network = %d: %s", rec.Code, rec.Body)
		}
	}
}

// metadataP95 returns the 95th percentile of samples by the nearest-rank
// definition: sort ascending and take index ceil(0.95*n)-1. The nearest-rank
// percentile is used rather than an interpolated one because the recorded claim
// is "95% of requests were at or under this duration", which is a statement
// about an observed sample, not about a distribution between two samples. The
// index is computed in integers as (95*n+99)/100-1 so no floating-point
// rounding picks the rank, and clamped to the last element so a short or empty
// sample set still returns a value.
func metadataP95(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	index := (95*len(sorted)+99)/100 - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

// metadataMillis renders a duration as fractional milliseconds for the
// recorded line.
func metadataMillis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// TestMetadataReadP95 is the recorded proof of T075 and plan.md §Performance
// Goals: with one selected, ready coordinator and the four classic keys
// supplied, GET /api/v1/coordinators, GET /api/v1/keys and GET /api/v1/network
// each answer within 100 ms at p95, measured in process through the real
// router. The fixture asserts the populated bodies before sampling, so a
// short-circuited render cannot pass as a fast one.
func TestMetadataReadP95(t *testing.T) {
	f := newHandlerFixture(t)
	if err := prepareMetadataState(f); err != nil {
		t.Fatalf("prepare the metadata fixture: %v", err)
	}

	id := device(metadataSerial).Identity.ID
	list := decode(t, f.do("GET", "/api/v1/coordinators", ""))
	if items, _ := list["items"].([]any); len(items) != 1 {
		t.Fatalf("GET /api/v1/coordinators: items = %v, want the one selected coordinator",
			list["items"])
	}
	if list["active_coordinator_id"] != id {
		t.Fatalf("GET /api/v1/coordinators: active_coordinator_id = %v, want %q",
			list["active_coordinator_id"], id)
	}
	keysTestAssertClasses(t, "GET /api/v1/keys",
		decode(t, f.do("GET", "/api/v1/keys", "")), hub.ClassicClasses())
	network := decode(t, f.do("GET", "/api/v1/network", ""))
	keysBlock, _ := network["keys"].(map[string]any)
	if keysBlock["classic_complete"] != true {
		t.Fatalf("GET /api/v1/network: keys = %v, want the complete classic set", network["keys"])
	}
	configured, _ := keysBlock["configured_classes"].([]any)
	if len(configured) != len(hub.ClassicClasses()) {
		t.Fatalf("GET /api/v1/network: configured_classes = %v, want the classic four",
			keysBlock["configured_classes"])
	}

	for _, path := range []string{"/api/v1/coordinators", "/api/v1/keys", "/api/v1/network"} {
		for i := 0; i < metadataWarmup; i++ {
			if rec := f.do("GET", path, ""); rec.Code != http.StatusOK {
				t.Fatalf("warm-up GET %s = %d: %s", path, rec.Code, rec.Body)
			}
		}
		samples := make([]time.Duration, metadataSamples)
		for i := range samples {
			start := time.Now()
			rec := f.do("GET", path, "")
			samples[i] = time.Since(start)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body)
			}
		}
		p95 := metadataP95(samples)
		t.Logf("GET %s: p95 %.3fms over %d requests (target %s)",
			path, metadataMillis(p95), len(samples), metadataTarget)
		if p95 > metadataTarget {
			t.Errorf("GET %s: p95 %.3fms exceeds the %s target of plan.md §Performance Goals",
				path, metadataMillis(p95), metadataTarget)
		}
	}
}
