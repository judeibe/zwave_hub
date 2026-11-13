package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/judeibe/zwave_hub/internal/hub"
)

// KeyService is the part of the hub the key handlers use. Reads return metadata
// only; the submitted and generated values go in through the request structs and
// never come back out (FR-013).
type KeyService interface {
	Keys(ctx context.Context) (hub.KeyConfiguration, error)
	SubmitKeys(ctx context.Context, req hub.KeySubmission) (hub.KeyConfiguration, error)
	GenerateKeys(ctx context.Context, req hub.KeyGeneration) (hub.KeyConfiguration, error)
}

// maxKeysBody bounds the key bodies: six class/value pairs, or one list of six
// class names.
const maxKeysBody = 8 << 10

// Keys serves the key resources.
type Keys struct {
	svc KeyService
}

// NewKeys returns the handlers over svc.
func NewKeys(svc KeyService) *Keys {
	return &Keys{svc: svc}
}

// Mount registers the key routes.
func (k *Keys) Mount(rt *Router) {
	rt.Handle("GET /api/v1/keys", http.HandlerFunc(k.getKeyConfiguration))
	rt.Handle("PUT /api/v1/keys", http.HandlerFunc(k.submitKeys))
	rt.Handle("POST /api/v1/keys/generate", http.HandlerFunc(k.generateKeys))
}

// getKeyConfiguration is getKeyConfiguration of contracts/openapi.yaml: per
// class metadata for all six classes and no values.
func (k *Keys) getKeyConfiguration(w http.ResponseWriter, r *http.Request) {
	cfg, err := k.svc.Keys(r.Context())
	if err != nil {
		writeKeyProblem(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, presentKeyConfiguration(cfg))
}

// keySubmissionBody is the PUT body of contracts/openapi.yaml KeySubmission.
type keySubmissionBody struct {
	Mode string            `json:"mode"`
	Keys map[string]string `json:"keys"`
}

// submitKeys is submitKeys of contracts/openapi.yaml. The mode and the presence
// of at least one class are checked here, because neither is a key-material
// problem: they are a malformed request. Everything about the values themselves
// is the hub's validation, so the refusal carries one code for the whole
// submission.
func (k *Keys) submitKeys(w http.ResponseWriter, r *http.Request) {
	var body keySubmissionBody
	if err := readKeysBody(r, &body, false); err != nil {
		WriteProblem(w, r, NewProblem(CodeInvalidRequest,
			"The body must be a JSON object with mode and keys, and no other member."))
		return
	}
	mode := hub.KeyMode(body.Mode)
	switch mode {
	case "", hub.KeyModeMerge, hub.KeyModeReplace:
	default:
		WriteProblem(w, r, NewProblem(CodeInvalidRequest, "mode must be merge or replace."))
		return
	}
	if len(body.Keys) == 0 {
		WriteProblem(w, r, NewProblem(CodeInvalidRequest, "keys must name at least one security class."))
		return
	}

	actor := ActorFromContext(r.Context())
	cfg, err := k.svc.SubmitKeys(r.Context(), hub.KeySubmission{
		Mode:       mode,
		Keys:       body.Keys,
		RequestID:  RequestIDFromContext(r.Context()),
		ActorAddr:  actor.Addr,
		ActorLabel: actor.Label,
	})
	if err != nil {
		writeKeyProblem(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, presentKeyConfiguration(cfg))
}

// keyGenerationBody is the optional POST body of generateKeys. An absent body
// and an empty classes list both mean the four classic classes.
type keyGenerationBody struct {
	Classes []string `json:"classes"`
}

// generateKeys is generateKeys of contracts/openapi.yaml. Values are drawn by
// the hub and never returned; the response is the same metadata view a
// submission returns.
func (k *Keys) generateKeys(w http.ResponseWriter, r *http.Request) {
	var body keyGenerationBody
	if err := readKeysBody(r, &body, true); err != nil {
		WriteProblem(w, r, NewProblem(CodeInvalidRequest,
			"The body must be a JSON object whose only member is classes."))
		return
	}

	actor := ActorFromContext(r.Context())
	cfg, err := k.svc.GenerateKeys(r.Context(), hub.KeyGeneration{
		Classes:    body.Classes,
		RequestID:  RequestIDFromContext(r.Context()),
		ActorAddr:  actor.Addr,
		ActorLabel: actor.Label,
	})
	if err != nil {
		writeKeyProblem(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, presentKeyConfiguration(cfg))
}

// readKeysBody reads v from the request body with unknown members refused. An
// absent or empty body is accepted only when the body is optional; anything
// after the one object is refused, so a body is either the contract's shape or
// nothing.
func readKeysBody(r *http.Request, v any, optional bool) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxKeysBody+1))
	if err != nil {
		return errors.New("api: request body is unreadable")
	}
	if len(body) > maxKeysBody {
		return errors.New("api: request body is too large")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		if optional {
			return nil
		}
		return errors.New("api: request body is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("api: request body holds more than one JSON value")
	}
	return nil
}

// writeKeyProblem maps a key failure to its problem. A validation refusal is
// 422 naming the classes and never the values; a key in use is 409 with the
// reset path; a corrupt store is 503; a full queue is 503 with Retry-After, as
// the reset answers it; a stopping hub is 503 driver_not_ready.
func writeKeyProblem(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *hub.ValidationError
	var inUse *hub.KeyInUseError
	var corrupt *hub.KeyStoreCorruptError
	switch {
	case errors.As(err, &invalid):
		code := CodeInvalidKeyMaterial
		detail := "A key must be exactly 32 hexadecimal characters and name a known security class; nothing was changed."
		if invalid.Code == hub.CodeDuplicateKeyMaterial {
			code = CodeDuplicateKeyMaterial
			detail = "Two classes of one key set share a value, which the driver refuses; nothing was changed."
		}
		WriteProblem(w, r, NewProblem(code, detail).WithClasses(invalid.Classes...))
	case errors.As(err, &inUse):
		WriteProblem(w, r, NewProblem(CodeKeyInUse,
			"The change would replace the keys of "+strconv.Itoa(inUse.SecureNodeCount)+
				" securely included devices; reset the network (POST /api/v1/network/reset) before replacing them.").
			WithClasses(inUse.Classes...))
	case errors.As(err, &corrupt):
		WriteProblem(w, r, NewProblem(CodeKeyStoreCorrupt,
			"The stored keys failed authentication: restore the database from backup, re-supply the original keys, or reset the network.").
			WithClasses(corrupt.Classes...))
	case errors.Is(err, hub.ErrQueueFull):
		w.Header().Set("Retry-After", "1")
		WriteProblem(w, r, NewProblem(CodeCommandQueueFull, "The command queue is full; retry shortly."))
	case errors.Is(err, hub.ErrStopped):
		WriteProblem(w, r, NewProblem(CodeDriverNotReady, "The hub is shutting down."))
	default:
		WriteProblem(w, r, NewProblem(CodeInternalError, "The key request could not be completed."))
	}
}
