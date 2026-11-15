// Command zwave-hub discovers USB Z-Wave coordinators, provisions their network
// keys and serves the control API.
//
// The object graph is built here and nowhere else: configuration, the event
// emitter, the store, the hub and its command worker, discovery, the driver
// supervisor and the HTTP boundary. The Go binary is PID 1 in the image and the
// sidecar its only child, so shutdown stops the child before the process exits.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/judeibe/zwave_hub/internal/api"
	"github.com/judeibe/zwave_hub/internal/config"
	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/driver"
	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

// Host paths. They are constants of the deployment, not configuration: the
// image mounts tmpfs at /run/zwave-hub, and sysfs and dev are where Linux puts
// them. Discovery takes them as injected values, so tests use fixture trees.
const (
	sysRoot = "/sys"
	devRoot = "/dev"
	runDir  = "/run/zwave-hub"
)

// shutdownTimeout bounds the drain of in-flight requests. The store is closed
// after the server has stopped, so no handler can write to a closed database.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "zwave-hub: %v\n", err)
		os.Exit(1)
	}
}

// run builds the graph and serves until SIGINT or SIGTERM.
func run() error {
	cfg, err := config.Load(os.Args[1:], config.Environ(), os.Stderr)
	if err != nil {
		return err
	}
	emitter := event.New(os.Stdout, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, store.Options{
		DataDir:   cfg.DataDir,
		MasterKey: cfg.MasterKey,
		Emitter:   emitter,
	})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	h, err := hub.New(st, emitter)
	if err != nil {
		return err
	}
	var background sync.WaitGroup
	workerCtx, stopWorker := context.WithCancel(context.Background())
	background.Add(1)
	go func() { defer background.Done(); h.Run(workerCtx) }()

	bridge := newDriverBridge(h)
	// One cleanup for every return below: the server has already stopped or
	// never started, so stop the sidecar, then the workers, then wait. It runs
	// before the store closes (deferred calls run last-in-first-out).
	defer func() {
		stop()
		bridge.stop()
		stopWorker()
		background.Wait()
	}()

	sup, err := driver.NewSupervisor(driver.Options{
		SidecarDir: cfg.SidecarDir,
		Mock:       cfg.DriverMode == config.DriverModeMock,
		RunDir:     runDir,
		CacheDir:   filepath.Join(cfg.DataDir, "zwave-cache"),
		LogLevel:   "info",
		Emitter:    emitter,
		Report:     bridge.report,
	})
	if err != nil {
		return err
	}
	bridge.attach(sup)
	// The reset orchestration runs through the same live child the reports
	// come from. The binding is set here because the supervisor needs the
	// bridge as its report callback, so the two cannot be built in one
	// expression (constitution III: everything is injected, nothing global).
	h.SetNetworkDriver(bridge)

	background.Add(1)
	go func() { defer background.Done(); bridge.run(ctx) }()

	if cfg.DriverMode == config.DriverModeMock {
		if err := h.SyncDiscovery(ctx, []coordinator.Device{mockDevice()}); err != nil {
			return err
		}
	} else {
		discovery, err := coordinator.NewDiscovery(coordinator.DiscoveryOptions{
			SysRoot:  sysRoot,
			DevRoot:  devRoot,
			Interval: cfg.DiscoveryInterval,
			Debounce: coordinator.DefaultDebounce,
			Publish: func(ctx context.Context, devices []coordinator.Device) error {
				if err := h.SyncDiscovery(ctx, devices); err != nil {
					return err
				}
				// A settled change set is what resumes a paused driver
				// (R14 §7); tell the bridge at once rather than waiting for
				// its next tick.
				bridge.deviceSetChanged()
				return nil
			},
		})
		if err != nil {
			return err
		}
		background.Add(1)
		go func() { defer background.Done(); discovery.Run(ctx) }()
	}

	router := api.NewRouter(api.Options{
		Emitter: emitter,
		Ready:   ready(st),
	})
	api.NewCoordinators(h).Mount(router)
	api.NewNetwork(h).Mount(router)
	api.NewAudit(st).Mount(router)

	// Listen before serving so a port already in use fails here, with a
	// message naming the address, rather than inside the serving goroutine.
	listener, err := net.Listen("tcp", cfg.Bind)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Bind, err)
	}

	server := &http.Server{
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	select {
	case err := <-served:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// ready reports why the API cannot answer. Until the command worker exists,
// the question is whether the database still answers at all: a closed or
// deleted file fails the query that SchemaVersion makes. It deliberately does
// not consult a coordinator (SC-008).
func ready(st *store.Store) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := st.SchemaVersion(ctx)
		return err
	}
}
