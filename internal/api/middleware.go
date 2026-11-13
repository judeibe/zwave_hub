package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/judeibe/zwave_hub/internal/event"
)

const (
	// requestIDHeader is echoed on every response and appears in the audit row
	// and the events for the same request (contracts/openapi.yaml).
	requestIDHeader = "X-Request-Id"

	// actorLabelHeader is the optional, self-declared client label. It is
	// recorded as unverified and never used for authorization.
	actorLabelHeader = "X-Actor-Label"

	// maxRequestIDLength bounds a client-supplied X-Request-Id and
	// maxActorLabelRunes bounds the label, so neither can flood a log line.
	maxRequestIDLength = 128
	maxActorLabelRunes = 64

	// unmatchedRoute is the `route` value for requests no pattern matched.
	// Reporting the raw path would put client-supplied text, including ids,
	// into the event stream.
	unmatchedRoute = "unmatched"
)

// Actor is the unauthenticated caller of a request: the network address the
// connection came from and an optional self-declared label. The hub has no
// authentication (contracts/openapi.yaml), so the label is a note, not an
// identity, and is recorded as unverified (FR-019).
type Actor struct {
	Addr  string
	Label string
}

// actorKey is the context key for the caller.
type actorKey struct{}

// ActorFromContext returns the caller the middleware captured.
func ActorFromContext(ctx context.Context) Actor {
	actor, _ := ctx.Value(actorKey{}).(Actor)
	return actor
}

// withActor stores the caller for handlers and command submissions.
func withActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// recorder captures the status and whether anything was written, which the
// access event and the panic path both need.
type recorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// WriteHeader records the first status. A second call is dropped, matching
// net/http's own behaviour.
func (rec *recorder) WriteHeader(status int) {
	if rec.wrote {
		return
	}
	rec.status = status
	rec.wrote = true
	rec.ResponseWriter.WriteHeader(status)
}

// Write implies a 200 when the handler never called WriteHeader.
func (rec *recorder) Write(body []byte) (int, error) {
	if !rec.wrote {
		rec.WriteHeader(http.StatusOK)
	}
	return rec.ResponseWriter.Write(body)
}

// statusCode is the status the client received, 200 when the handler wrote
// nothing at all.
func (rec *recorder) statusCode() int {
	if !rec.wrote {
		return http.StatusOK
	}
	return rec.status
}

// Unwrap lets http.ResponseController reach the underlying writer, so
// streaming and deadlines still work through the recorder.
func (rec *recorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

// Instrument wraps one route's handler with the per-request work: request id,
// actor capture, panic recovery, and exactly one api.request event. route is
// the path pattern, never the raw path, so an id in the URL cannot reach the
// event stream. Every route the router mounts goes through here.
func Instrument(emitter *event.Emitter, route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		id := requestIDFrom(r)
		actor := actorFrom(r)

		// WriteProblem records the code it wrote here, so the access event can
		// carry it without re-reading the response.
		var problemCode Code
		ctx := withRequestID(r.Context(), id)
		ctx = withActor(ctx, actor)
		ctx = context.WithValue(ctx, problemCodeKey{}, &problemCode)
		r = r.WithContext(ctx)

		w.Header().Set(requestIDHeader, id)
		rec := &recorder{ResponseWriter: w}
		corr := event.Correlation{
			RequestID:  id,
			ActorAddr:  actor.Addr,
			ActorLabel: actor.Label,
		}

		defer func() {
			if recovered := recover(); recovered != nil {
				// Only the type is reported: the value can hold a key or a
				// rendered config, and a stack can hold both.
				if !rec.wrote {
					WriteProblem(rec, r, NewProblem(CodeInternalError, "The server failed to complete the request."))
				}
				emit(ctx, emitter, &event.APIPanic{Route: route, PanicType: panicType(recovered)}, corr)
			}
			status := rec.statusCode()
			var code *string
			if status >= http.StatusBadRequest && problemCode != "" {
				value := string(problemCode)
				code = &value
			}
			emit(ctx, emitter, &event.APIRequest{
				Method:     r.Method,
				Route:      route,
				Status:     status,
				DurationMS: time.Since(started).Milliseconds(),
				Code:       code,
			}, corr)
		}()

		next.ServeHTTP(rec, r)
	})
}

// emit writes one catalogue event and tolerates a nil emitter, which tests and
// the router's zero value rely on.
func emit(ctx context.Context, emitter *event.Emitter, ev event.Event, corr event.Correlation) {
	if emitter == nil {
		return
	}
	emitter.Emit(ctx, ev, corr)
}

// panicType names the recovered value's type. A value with no type information
// (a bare panic(nil), which Go reports as a runtime.PanicNilError) still gets a
// name rather than an empty field.
func panicType(recovered any) string {
	if recovered == nil {
		return "runtime.PanicNilError"
	}
	t := reflect.TypeOf(recovered)
	if t == nil {
		return "unknown"
	}
	return t.String()
}

// requestIDFrom reuses a well-formed client X-Request-Id and generates one
// otherwise. A supplied value is constrained to visible ASCII, and one that
// carries a 32+ hex run is refused rather than echoed: that is the shape of a
// Z-Wave key, and internal/event redacts any such string, so accepting it would
// either log a key or break correlation. The response then carries a generated
// id instead.
func requestIDFrom(r *http.Request) string {
	if supplied := r.Header.Get(requestIDHeader); validRequestID(supplied) {
		return supplied
	}
	return newRequestID()
}

// newRequestID returns a dashed, UUID-shaped 128-bit id. The dashes keep every
// hex run shorter than the 32-character redaction threshold in internal/event,
// so a generated id survives into the event stream instead of being masked.
func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// validRequestID reports whether a supplied id is usable: 1 to 128 visible
// ASCII characters with no key-shaped run.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return !hasHexRun(id)
}

// hasHexRun mirrors the redaction threshold of internal/event: a run of 32 or
// more hex characters anywhere in the string. It is duplicated rather than
// exported because the two uses differ — event redacts, this refuses.
func hasHexRun(s string) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			run++
			if run >= 32 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// actorFrom captures the caller's address with the port stripped, and the
// label sanitized to at most 64 printable characters.
func actorFrom(r *http.Request) Actor {
	addr := r.RemoteAddr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	return Actor{Addr: addr, Label: sanitizeActorLabel(r.Header.Get(actorLabelHeader))}
}

// sanitizeActorLabel drops control characters and stops at 64 characters, the
// same rule the audit row and the event stream enforce, so a malformed header
// is recorded in reduced form rather than rejected.
func sanitizeActorLabel(label string) string {
	var b strings.Builder
	b.Grow(len(label))
	written := 0
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if written == maxActorLabelRunes {
			break
		}
		b.WriteRune(r)
		written++
	}
	return b.String()
}
