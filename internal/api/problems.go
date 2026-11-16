// Package api is the HTTP boundary: routing, middleware, problem responses and
// response presenters. It deals in contract types only and never in SQL.
package api

import (
	"context"
	"net/http"
	"strings"
)

// Code is the machine-readable error code of a problem response. The set is
// closed by contracts/openapi.yaml (Problem.code); a caller cannot invent one,
// which is what lets a client switch on it.
type Code string

// The problem codes of contracts/openapi.yaml.
const (
	CodeInvalidKeyMaterial         Code = "invalid_key_material"
	CodeDuplicateKeyMaterial       Code = "duplicate_key_material"
	CodeKeyInUse                   Code = "key_in_use"
	CodeCoordinatorNotFound        Code = "coordinator_not_found"
	CodeCoordinatorUnsupported     Code = "coordinator_unsupported"
	CodeCoordinatorNetworkMismatch Code = "coordinator_network_mismatch"
	CodeCoordinatorUnreachable     Code = "coordinator_unreachable"
	CodeNoActiveCoordinator        Code = "no_active_coordinator"
	CodeConfirmationInvalid        Code = "confirmation_invalid"
	CodeConfirmationExpired        Code = "confirmation_expired"
	CodeKeyStoreCorrupt            Code = "key_store_corrupt"
	CodeDestructiveOpsDisabled     Code = "destructive_ops_disabled"
	CodeCommandQueueFull           Code = "command_queue_full"
	CodeDriverNotReady             Code = "driver_not_ready"
	CodeInvalidRequest             Code = "invalid_request"
	CodeInternalError              Code = "internal_error"
)

// problemMediaType is the RFC 9457 content type. Every error response uses it,
// including the ones the router writes for an unknown route.
const problemMediaType = "application/problem+json"

// Problem is an RFC 9457 problem document. The required members are always
// present; Class and Classes carry the security classes a key or readiness
// error names (FR-011, FR-006), and RequestID ties the body to the log record
// and the audit row for the same request.
type Problem struct {
	Type      string   `json:"type"`
	Title     string   `json:"title"`
	Status    int      `json:"status"`
	Code      Code     `json:"code"`
	Detail    string   `json:"detail"`
	RequestID string   `json:"request_id,omitempty"`
	Class     string   `json:"class,omitempty"`
	Classes   []string `json:"classes,omitempty"`
}

// TypeURI is the problem type for a code: a URN derived from the code, so a
// client that only understands `type` still gets a stable identifier.
func (c Code) TypeURI() string {
	return "urn:zwave-hub:error:" + strings.ReplaceAll(string(c), "_", "-")
}

// Title is the short human-readable summary that belongs to the code. It is
// fixed per code so the same failure always reads the same way.
func (c Code) Title() string {
	switch c {
	case CodeInvalidKeyMaterial:
		return "Invalid key material"
	case CodeDuplicateKeyMaterial:
		return "Duplicate key material"
	case CodeKeyInUse:
		return "Key in use"
	case CodeCoordinatorNotFound:
		return "Coordinator not found"
	case CodeCoordinatorUnsupported:
		return "Coordinator unsupported"
	case CodeCoordinatorNetworkMismatch:
		return "Coordinator network mismatch"
	case CodeCoordinatorUnreachable:
		return "Coordinator unreachable"
	case CodeNoActiveCoordinator:
		return "No active coordinator"
	case CodeConfirmationInvalid:
		return "Confirmation invalid"
	case CodeConfirmationExpired:
		return "Confirmation expired"
	case CodeKeyStoreCorrupt:
		return "Key store corrupt"
	case CodeDestructiveOpsDisabled:
		return "Destructive operations disabled"
	case CodeCommandQueueFull:
		return "Command queue full"
	case CodeDriverNotReady:
		return "Driver not ready"
	case CodeInvalidRequest:
		return "Invalid request"
	case CodeInternalError:
		return "Internal error"
	default:
		return "Internal error"
	}
}

// Status is the HTTP status the code is defined with. Where the contract names
// a status (409 for a conflicting coordinator, 410 for an expired confirmation,
// 503 for a full queue or a disabled reset) this table is the one authority.
func (c Code) Status() int {
	switch c {
	case CodeInvalidKeyMaterial, CodeDuplicateKeyMaterial:
		return http.StatusUnprocessableEntity // 422
	case CodeKeyInUse, CodeCoordinatorUnsupported, CodeCoordinatorNetworkMismatch,
		CodeNoActiveCoordinator, CodeCoordinatorUnreachable:
		return http.StatusConflict // 409
	case CodeCoordinatorNotFound, CodeConfirmationInvalid:
		return http.StatusNotFound // 404
	case CodeConfirmationExpired:
		return http.StatusGone // 410
	case CodeKeyStoreCorrupt, CodeDestructiveOpsDisabled,
		CodeCommandQueueFull, CodeDriverNotReady:
		return http.StatusServiceUnavailable // 503
	case CodeInvalidRequest:
		return http.StatusBadRequest // 400
	default:
		return http.StatusInternalServerError // 500
	}
}

// NewProblem builds a problem document from a code and a human explanation.
// Title, status and type come from the code; the detail must never contain key
// material (FR-013).
func NewProblem(code Code, detail string) *Problem {
	return &Problem{
		Type:   code.TypeURI(),
		Title:  code.Title(),
		Status: code.Status(),
		Code:   code,
		Detail: detail,
	}
}

// WithClass names the security class the problem is about. It returns the same
// document so a call site can build one in a single expression.
func (p *Problem) WithClass(class string) *Problem {
	p.Class = class
	return p
}

// WithClasses names several security classes, as readiness and duplicate-key
// errors do. A single class is named in the singular member as well, because
// the contract has one and a client reading a one-class refusal should not have
// to know that the plural form exists.
func (p *Problem) WithClasses(classes ...string) *Problem {
	p.Classes = append([]string(nil), classes...)
	if len(p.Classes) == 1 {
		p.Class = p.Classes[0]
	}
	return p
}

// WriteProblem writes p as application/problem+json. The request id is stamped
// from the context when p does not already carry one, so every error body is
// correlatable even when the handler forgot. The code is also recorded for the
// access log, which would otherwise have to parse the body. Marshal cannot fail
// for this struct; a failure would mean a bug, so it falls back to a bare 500
// that still carries the request id.
func WriteProblem(w http.ResponseWriter, r *http.Request, p *Problem) {
	if p.RequestID == "" {
		p.RequestID = RequestIDFromContext(r.Context())
	}
	noteProblemCode(r, p.Code)
	body, err := Present(p)
	if err != nil {
		w.Header().Set("Content-Type", problemMediaType)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", problemMediaType)
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}

// writeJSON writes a success body with its content type and status. It is the
// one writer the handlers share, so no response can quietly be text/plain. The
// request is only used to stamp a request id on the unlikely encoding failure.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	encoded, err := Present(body)
	if err != nil {
		WriteProblem(w, r, NewProblem(CodeInternalError, "The response could not be encoded."))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// requestIDKey is the context key for the per-request identifier. The type is
// unexported so nothing outside this package can collide with it.
type requestIDKey struct{}

// problemCodeKey is the context key under which the instrumentation middleware
// installs the access-log code slot. It holds a *Code, so WriteProblem can
// record what it wrote without the middleware and this file sharing a mutex or
// a second context lookup.
type problemCodeKey struct{}

// noteProblemCode records the code of the problem just written, when the
// instrumentation middleware is in the chain.
func noteProblemCode(r *http.Request, code Code) {
	if slot, ok := r.Context().Value(problemCodeKey{}).(*Code); ok {
		*slot = code
	}
}

// RequestIDFromContext returns the request id the middleware assigned, or the
// empty string outside a request.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// withRequestID stores the request id for the handlers and problem writer.
func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}
