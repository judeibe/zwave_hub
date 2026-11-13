package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/judeibe/zwave_hub/internal/store"
)

// AuditService is the part of the store the audit handler reads.
type AuditService interface {
	ListAudit(ctx context.Context, q store.AuditQuery) (store.AuditPage, error)
}

// Audit serves the audit trail.
type Audit struct {
	svc AuditService
}

// NewAudit returns the handler over svc.
func NewAudit(svc AuditService) *Audit {
	return &Audit{svc: svc}
}

// Mount registers the audit route.
func (a *Audit) Mount(rt *Router) {
	rt.Handle("GET /api/v1/audit", http.HandlerFunc(a.listAuditEntries))
}

// listAuditEntries pages the trail newest first. `before` is the cursor from the
// previous page's next_before; detail is the closed store.AuditDetail, so only
// allow-listed members can be rendered.
func (a *Audit) listAuditEntries(w http.ResponseWriter, r *http.Request) {
	query := store.AuditQuery{}
	values := r.URL.Query()
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			WriteProblem(w, r, NewProblem(CodeInvalidRequest, "limit must be an integer from 1 to 200."))
			return
		}
		query.Limit = limit
	}
	if raw := values.Get("before"); raw != "" {
		before, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || before < 1 {
			WriteProblem(w, r, NewProblem(CodeInvalidRequest, "before must be a positive integer."))
			return
		}
		query.Before = before
	}
	if raw := values.Get("action"); raw != "" {
		query.Action = store.AuditAction(raw)
	}
	page, err := a.svc.ListAudit(r.Context(), query)
	if err != nil {
		WriteProblem(w, r, NewProblem(CodeInvalidRequest, "The audit query was not accepted."))
		return
	}
	writeJSON(w, r, http.StatusOK, presentAuditPage(page))
}
