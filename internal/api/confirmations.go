package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/judeibe/zwave_hub/internal/hub"
)

// The operation names the two-step tokens are bound to
// (contracts/openapi.yaml ConfirmationChallenge.operation). A token is valid
// only for the operation it was issued for.
const (
	resetOperation  = "network.reset"
	exportOperation = "keys.export"
)

// maxConfirmationBody bounds either two-step body: one token.
const maxConfirmationBody = 4 << 10

// ResetService is the part of the hub the reset endpoint uses. The two steps are
// separate calls so the handler can answer 202 without performing anything: the
// first issues the confirmation token, the second redeems it and resets.
type ResetService interface {
	RequestReset(ctx context.Context, req hub.ResetRequest) (hub.ResetChallenge, error)
	ConfirmReset(ctx context.Context, req hub.ResetRequest, token string) (hub.ResetResult, error)
}

// KeyExportService is the part of the hub the export endpoint uses. It is two
// calls for the same reason: the first answers 202 and exports nothing.
type KeyExportService interface {
	RequestKeyExport(ctx context.Context, req hub.KeyExportRequest) (hub.KeyExportChallenge, error)
	ConfirmKeyExport(ctx context.Context, req hub.KeyExportRequest, token string) (hub.KeyExport, error)
}

// ConfirmationService is the part of the hub both two-step operations use.
type ConfirmationService interface {
	ResetService
	KeyExportService
}

// Confirmations serves the two-step network reset and key export.
type Confirmations struct {
	svc ConfirmationService
	// destructiveOps reports whether the operator left the reset enabled. When
	// it is false the reset refuses both steps, so an operator can close it
	// without a code change (ZWAVE_HUB_DESTRUCTIVE_OPS, FR-017). The export is
	// a read and stays available (INC-4).
	destructiveOps bool
}

// NewConfirmations returns the handler over svc. destructiveOps comes from the
// configuration; the hub itself never reads the environment.
func NewConfirmations(svc ConfirmationService, destructiveOps bool) *Confirmations {
	return &Confirmations{svc: svc, destructiveOps: destructiveOps}
}

// Mount registers the two-step routes.
func (c *Confirmations) Mount(rt *Router) {
	rt.Handle("POST /api/v1/network/reset", http.HandlerFunc(c.resetNetwork))
	rt.Handle("POST /api/v1/keys/export", http.HandlerFunc(c.exportKeys))
}

// confirmationRequest is the optional body of either two-step operation. A nil
// ConfirmToken means the first step; a token means the second. A token that is
// present but empty is a presented token the hub refuses, not a request for a
// new one.
type confirmationRequest struct {
	ConfirmToken *string `json:"confirm_token"`
}

// resetNetwork is resetNetwork of contracts/openapi.yaml, in two steps. The
// first answers 202 with a token and performs nothing; the second redeems the
// token, performs the reset and answers 200. The endpoint is destructive, so
// it is refused entirely when the operator disabled destructive operations.
func (c *Confirmations) resetNetwork(w http.ResponseWriter, r *http.Request) {
	if !c.destructiveOps {
		WriteProblem(w, r, NewProblem(CodeDestructiveOpsDisabled,
			"The network reset is disabled on this hub (ZWAVE_HUB_DESTRUCTIVE_OPS=disabled)."))
		return
	}
	req, err := readConfirmationRequest(r)
	if err != nil {
		WriteProblem(w, r, NewProblem(CodeInvalidRequest,
			"The body must be a JSON object whose only member is confirm_token."))
		return
	}

	actor := ActorFromContext(r.Context())
	reset := hub.ResetRequest{
		RequestID:  RequestIDFromContext(r.Context()),
		ActorAddr:  actor.Addr,
		ActorLabel: actor.Label,
	}
	if req.ConfirmToken == nil {
		challenge, err := c.svc.RequestReset(r.Context(), reset)
		if err != nil {
			writeResetProblem(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusAccepted, presentResetChallenge(challenge))
		return
	}

	result, err := c.svc.ConfirmReset(r.Context(), reset, *req.ConfirmToken)
	if err != nil {
		writeResetProblem(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, presentResetResult(result))
}

// readConfirmationRequest reads the optional body either two-step operation
// takes. An absent or empty body is the first step; anything else has to be an
// object with no member but confirm_token.
func readConfirmationRequest(r *http.Request) (confirmationRequest, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxConfirmationBody+1))
	if err != nil || len(body) > maxConfirmationBody {
		return confirmationRequest{}, errors.New("api: confirmation body is unreadable or too large")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return confirmationRequest{}, nil
	}
	var req confirmationRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return confirmationRequest{}, err
	}
	return req, nil
}

// exportKeys is exportKeys of contracts/openapi.yaml, in two steps. The first
// answers 202 with a token and exports nothing; the second redeems the token,
// returns the values and answers 200. It is not gated by destructive
// operations: an export reads the keys and changes nothing (INC-4).
func (c *Confirmations) exportKeys(w http.ResponseWriter, r *http.Request) {
	req, err := readConfirmationRequest(r)
	if err != nil {
		WriteProblem(w, r, NewProblem(CodeInvalidRequest,
			"The body must be a JSON object whose only member is confirm_token."))
		return
	}

	actor := ActorFromContext(r.Context())
	export := hub.KeyExportRequest{
		RequestID:  RequestIDFromContext(r.Context()),
		ActorAddr:  actor.Addr,
		ActorLabel: actor.Label,
	}
	if req.ConfirmToken == nil {
		challenge, err := c.svc.RequestKeyExport(r.Context(), export)
		if err != nil {
			writeKeyExportProblem(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusAccepted, presentKeyExportChallenge(challenge))
		return
	}

	result, err := c.svc.ConfirmKeyExport(r.Context(), export, *req.ConfirmToken)
	if err != nil {
		writeKeyExportProblem(w, r, err)
		return
	}
	// The values live in the body only until it is encoded (FR-013).
	defer result.Wipe()
	writeJSON(w, r, http.StatusOK, presentKeyExport(result))
}

// writeKeyExportProblem maps an export failure to its problem. The confirmation
// errors are the same pair as the reset's: a token that was issued and has
// expired or been used is 410, one that is unknown, another client's or another
// operation's is 404. A store that cannot authenticate its keys is 503.
func writeKeyExportProblem(w http.ResponseWriter, r *http.Request, err error) {
	var confirmation *hub.KeyExportConfirmationError
	var corrupt *hub.KeyStoreCorruptError
	switch {
	case errors.As(err, &confirmation):
		if confirmation.Unusable {
			WriteProblem(w, r, NewProblem(CodeConfirmationExpired,
				"The confirmation token has expired or was already used; request a new one."))
			return
		}
		WriteProblem(w, r, NewProblem(CodeConfirmationInvalid,
			"The confirmation token is not valid for this client and operation."))
	case errors.As(err, &corrupt):
		WriteProblem(w, r, NewProblem(CodeKeyStoreCorrupt,
			"The stored keys failed authentication, so they cannot be exported: restore the database from backup or re-supply the original keys.").
			WithClasses(corrupt.Classes...))
	case errors.Is(err, hub.ErrQueueFull):
		w.Header().Set("Retry-After", "1")
		WriteProblem(w, r, NewProblem(CodeCommandQueueFull, "The command queue is full; retry shortly."))
	case errors.Is(err, hub.ErrStopped):
		WriteProblem(w, r, NewProblem(CodeDriverNotReady, "The hub is shutting down."))
	default:
		WriteProblem(w, r, NewProblem(CodeInternalError, "The key export could not be completed."))
	}
}

// writeResetProblem maps a reset failure to its problem. The confirmation errors
// are the two cases FR-013 separates: a token that was issued and has expired or
// been used is 410, one that is unknown or another client's is 404. A driver
// that did not confirm is 409 with coordinator_unreachable, as
// contracts/zwave-server-sidecar.md §6 defines, because the reset may still have
// landed and the client has to re-read GET /api/v1/network before retrying.
func writeResetProblem(w http.ResponseWriter, r *http.Request, err error) {
	var confirmation *hub.ResetConfirmationError
	var failure *hub.ResetDriverFailure
	switch {
	case errors.As(err, &confirmation):
		if confirmation.Unusable {
			WriteProblem(w, r, NewProblem(CodeConfirmationExpired,
				"The confirmation token has expired or was already used; request a new one."))
			return
		}
		WriteProblem(w, r, NewProblem(CodeConfirmationInvalid,
			"The confirmation token is not valid for this client and operation."))
	case errors.As(err, &failure):
		if errors.Is(err, hub.ErrFirmwareUpdateInProgress) {
			WriteProblem(w, r, NewProblem(CodeCoordinatorUnreachable,
				"The coordinator is updating its firmware, so the reset was not attempted; retry once the update finishes."))
			return
		}
		WriteProblem(w, r, NewProblem(CodeCoordinatorUnreachable,
			"The reset did not complete; re-read GET /api/v1/network before retrying."))
	case errors.Is(err, hub.ErrNoActiveCoordinator):
		WriteProblem(w, r, NewProblem(CodeNoActiveCoordinator,
			"No coordinator is selected, so there is no network to reset."))
	case errors.Is(err, hub.ErrNetworkDriverUnavailable):
		WriteProblem(w, r, NewProblem(CodeDriverNotReady,
			"The hub has no driver to run the reset through."))
	case errors.Is(err, hub.ErrQueueFull):
		w.Header().Set("Retry-After", "1")
		WriteProblem(w, r, NewProblem(CodeCommandQueueFull, "The command queue is full; retry shortly."))
	case errors.Is(err, hub.ErrStopped):
		WriteProblem(w, r, NewProblem(CodeDriverNotReady, "The hub is shutting down."))
	default:
		WriteProblem(w, r, NewProblem(CodeInternalError, "The reset could not be completed."))
	}
}
