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

// CoordinatorService is the part of the hub the coordinator handlers use.
// *hub.Hub implements it; the interface keeps the handlers testable and keeps
// this package from reaching into hub internals.
type CoordinatorService interface {
	Snapshot() hub.Snapshot
	Select(ctx context.Context, req hub.SelectionRequest) (hub.Snapshot, error)
}

// maxSelectBody bounds the request body: one identity string.
const maxSelectBody = 4 << 10

// Coordinators serves the coordinator resources.
type Coordinators struct {
	svc CoordinatorService
}

// NewCoordinators returns the handlers over svc.
func NewCoordinators(svc CoordinatorService) *Coordinators {
	return &Coordinators{svc: svc}
}

// Mount registers the coordinator routes.
func (c *Coordinators) Mount(rt *Router) {
	rt.Handle("GET /api/v1/coordinators", http.HandlerFunc(c.listCoordinators))
	rt.Handle("GET /api/v1/coordinators/{coordinator_id}", http.HandlerFunc(c.getCoordinator))
	rt.Handle("GET /api/v1/active-coordinator", http.HandlerFunc(c.getActiveCoordinator))
	rt.Handle("PUT /api/v1/active-coordinator", http.HandlerFunc(c.selectCoordinator))
}

func (c *Coordinators) listCoordinators(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, presentCoordinatorList(c.svc.Snapshot()))
}

func (c *Coordinators) getCoordinator(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("coordinator_id")
	found, ok := findCoordinator(c.svc.Snapshot(), id)
	if !ok {
		WriteProblem(w, r, NewProblem(CodeCoordinatorNotFound, "No coordinator has that identity."))
		return
	}
	writeJSON(w, r, http.StatusOK, presentCoordinator(found))
}

func (c *Coordinators) getActiveCoordinator(w http.ResponseWriter, r *http.Request) {
	snap := c.svc.Snapshot()
	active, ok := findCoordinator(snap, snap.ActiveID)
	if snap.ActiveID == "" || !ok {
		// The contract answers 404 here; the code's default 409 is for the
		// operations that need an active coordinator.
		problem := NewProblem(CodeNoActiveCoordinator, "No coordinator is selected.")
		problem.Status = http.StatusNotFound
		WriteProblem(w, r, problem)
		return
	}
	writeJSON(w, r, http.StatusOK, presentCoordinator(active))
}

func findCoordinator(snap hub.Snapshot, id string) (hub.Coordinator, bool) {
	return snap.Coordinator(id)
}

// selectRequest is the PUT body; unknown members are refused.
type selectRequest struct {
	CoordinatorID string `json:"coordinator_id"`
}

func (c *Coordinators) selectCoordinator(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSelectBody+1))
	if err != nil || len(body) > maxSelectBody {
		WriteProblem(w, r, NewProblem(CodeInvalidRequest, "The request body could not be read."))
		return
	}
	var req selectRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || req.CoordinatorID == "" {
		WriteProblem(w, r, NewProblem(CodeInvalidRequest, "The body must be a JSON object with a coordinator_id."))
		return
	}

	actor := ActorFromContext(r.Context())
	snap, err := c.svc.Select(r.Context(), hub.SelectionRequest{
		CoordinatorID: req.CoordinatorID,
		RequestID:     RequestIDFromContext(r.Context()),
		ActorAddr:     actor.Addr,
		ActorLabel:    actor.Label,
	})
	if err != nil {
		WriteProblem(w, r, selectionProblem(err))
		return
	}
	selected, ok := findCoordinator(snap, req.CoordinatorID)
	if !ok {
		WriteProblem(w, r, NewProblem(CodeInternalError, "The selected coordinator is no longer known."))
		return
	}
	writeJSON(w, r, http.StatusOK, presentCoordinator(selected))
}

// selectionProblem maps a Select failure to its problem. A refusal is the
// hub's *SelectionError; a full queue is 503 with the codes the contract
// defines. ambiguous_identity has no problem code of its own in
// contracts/openapi.yaml, so it is reported as coordinator_unsupported with a
// detail that names the cause.
func selectionProblem(err error) *Problem {
	var refusal *hub.SelectionError
	switch {
	case errors.As(err, &refusal):
		switch refusal.Code {
		case hub.CodeCoordinatorNotFound:
			return NewProblem(CodeCoordinatorNotFound, "No attached coordinator has that identity.")
		case hub.CodeCoordinatorUnsupported:
			return NewProblem(CodeCoordinatorUnsupported, "That device is not a supported Z-Wave coordinator.")
		case hub.CodeCoordinatorNetworkMismatch:
			return NewProblem(CodeCoordinatorNetworkMismatch,
				"The replacement reports a different network than the one holding secure devices. "+
					"Reset the network with DELETE /api/v1/network to start over, or attach the original coordinator.")
		case hub.CodeAmbiguousIdentity:
			return NewProblem(CodeCoordinatorUnsupported,
				"More than one attached device claims that identity, so it cannot be selected safely.")
		}
	case errors.Is(err, hub.ErrQueueFull):
		return NewProblem(CodeCommandQueueFull, "The command queue is full; retry shortly.")
	case errors.Is(err, hub.ErrStopped):
		return NewProblem(CodeDriverNotReady, "The hub is shutting down.")
	}
	return NewProblem(CodeInternalError, "The selection could not be completed.")
}
