package api

import (
	"net/http"
	"strings"

	"github.com/judeibe/zwave_hub/internal/event"
)

// Options configures the router.
type Options struct {
	// Emitter receives api.request and api.panic. It may be nil in tests.
	Emitter *event.Emitter

	// Ready reports why the API cannot serve requests: the database is not
	// open, migrations have not been applied, or the command worker is not
	// running. Nil means there is nothing to check and the API is ready. It
	// deliberately does not consult the coordinator, because SC-008 requires
	// readiness without a device.
	Ready func() error
}

// Router is the HTTP surface. Routes live under /api/v1; liveness and
// readiness sit outside the version prefix so a container health check does
// not depend on the API version. The router is safe for concurrent use once
// construction has finished.
type Router struct {
	mux     *http.ServeMux
	emitter *event.Emitter
	ready   func() error
}

// NewRouter returns the router with the health endpoints mounted. Resources
// are added with Handle, which keeps the route table in one place per phase.
func NewRouter(opts Options) *Router {
	rt := &Router{
		mux:     http.NewServeMux(),
		emitter: opts.Emitter,
		ready:   opts.Ready,
	}
	rt.Handle("GET /healthz", http.HandlerFunc(rt.getHealth))
	rt.Handle("GET /readyz", http.HandlerFunc(rt.getReady))
	return rt
}

// Handle mounts one handler. pattern is a net/http method-and-wildcard pattern
// ("GET /api/v1/coordinators/{coordinator_id}"); the access event reports the
// path half only, so an id in the URL never reaches the event stream.
func (rt *Router) Handle(pattern string, handler http.Handler) {
	rt.mux.Handle(pattern, Instrument(rt.emitter, routeOf(pattern), handler))
}

// ServeHTTP dispatches through the mux. A request no pattern matches is served
// by a handler of our own rather than net/http's text/plain page, so every
// response of this API is JSON or problem+json and every request still gets a
// request id and one access event.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler, pattern := rt.mux.Handler(r)
	if pattern != "" {
		handler.ServeHTTP(w, r)
		return
	}
	Instrument(rt.emitter, unmatchedRoute, http.HandlerFunc(rt.notFound)).ServeHTTP(w, r)
}

// notFound answers an unmatched request: 405 with Allow when the path exists
// under another method, 404 otherwise. Both carry the invalid_request code,
// the only generic one the contract defines.
func (rt *Router) notFound(w http.ResponseWriter, r *http.Request) {
	if allowed := rt.allowedMethods(r); len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		problem := NewProblem(CodeInvalidRequest, "This method is not supported for the requested path.")
		problem.Status = http.StatusMethodNotAllowed
		WriteProblem(w, r, problem)
		return
	}
	problem := NewProblem(CodeInvalidRequest, "No route matches the requested path.")
	problem.Status = http.StatusNotFound
	WriteProblem(w, r, problem)
}

// probeMethods are the methods a route may be registered with. HEAD is listed
// because a GET pattern serves it too, and the Allow header should say so.
var probeMethods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPut,
	http.MethodPost,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodOptions,
}

// allowedMethods reports which other methods match this path. It asks the mux
// itself rather than keeping a second route table that could drift from the
// real one.
func (rt *Router) allowedMethods(r *http.Request) []string {
	var allowed []string
	for _, method := range probeMethods {
		if method == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := rt.mux.Handler(probe); pattern != "" {
			allowed = append(allowed, method)
		}
	}
	return allowed
}

// routeOf keeps the path half of a mux pattern for the event stream.
func routeOf(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	return pattern
}

// healthResponse is the body of both health endpoints (contracts/openapi.yaml:
// {status: ok} and {status: ready}).
type healthResponse struct {
	Status string `json:"status"`
}

// getHealth is liveness only: it answers whenever the process can serve HTTP.
func (rt *Router) getHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, healthResponse{Status: "ok"})
}

// getReady reports API readiness: the database, its migrations and the command
// worker. A failure is 503 with a fixed detail, never the underlying message,
// which can name a file path.
func (rt *Router) getReady(w http.ResponseWriter, r *http.Request) {
	if rt.ready != nil {
		if err := rt.ready(); err != nil {
			problem := NewProblem(CodeInternalError, "The API is not ready to serve requests.")
			problem.Status = http.StatusServiceUnavailable
			WriteProblem(w, r, problem)
			return
		}
	}
	writeJSON(w, r, http.StatusOK, healthResponse{Status: "ready"})
}
