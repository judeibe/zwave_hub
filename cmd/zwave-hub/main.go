// Command zwave-hub discovers USB Z-Wave coordinators, provisions their network
// keys and serves the control API.
//
// The object graph is built here and nowhere else. This is the graph Phase 02
// needs to serve its health endpoints: configuration, the event emitter, the
// store and the HTTP boundary. T034 adds the command worker, discovery, the
// supervisor and PID 1 behaviour.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/judeibe/zwave_hub/internal/api"
	"github.com/judeibe/zwave_hub/internal/config"
	"github.com/judeibe/zwave_hub/internal/event"
	"github.com/judeibe/zwave_hub/internal/store"
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

	router := api.NewRouter(api.Options{
		Emitter: emitter,
		Ready:   ready(st),
	})

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
