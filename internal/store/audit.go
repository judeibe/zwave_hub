package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// auditTimeLayout is the `at` format of data-model.md §7: RFC 3339 UTC with
	// milliseconds. Values are stored after .UTC(), so the offset always renders
	// as Z and the text sorts in the order the entries were taken.
	auditTimeLayout = "2006-01-02T15:04:05.000Z07:00"

	// auditDefaultPageSize is the page size when AuditQuery.Limit is zero, and
	// auditMaxPageSize is the ceiling (contracts/openapi.yaml GET /api/v1/audit).
	auditDefaultPageSize = 50
	auditMaxPageSize     = 200

	// auditMaxLabelRunes caps actor_label, a self-declared header recorded
	// unverified (data-model.md §7).
	auditMaxLabelRunes = 64
)

// auditInsertSQL writes one row. It is the only write this file performs: the
// audit trail is append-only, and no path here rewrites or removes a row.
const auditInsertSQL = `INSERT INTO audit_entries
	(at, action, actor_addr, actor_label, request_id, coordinator_id, security_class, outcome, detail)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// auditSelectSQL is the read side. ListAudit appends its WHERE, ORDER BY and
// LIMIT clauses so a filtered page still uses one statement.
const auditSelectSQL = `SELECT id, at, action, actor_addr, actor_label, request_id,
	coordinator_id, security_class, outcome, detail
FROM audit_entries`

// AuditAction names one kind of configuration change in the audit trail. The
// set is closed (data-model.md §7): driver lifecycle is an operational event,
// not a configuration change, so it is never audited, and a new action is a
// contract change rather than a call-site choice.
type AuditAction string

// The audited actions. Each carries exactly one outcome; auditOutcome is the
// authority on which, and an entry whose outcome disagrees is refused.
const (
	// AuditCoordinatorSelected records a committed coordinator selection.
	AuditCoordinatorSelected AuditAction = "coordinator.selected"
	// AuditCoordinatorRejected records a refused coordinator selection.
	AuditCoordinatorRejected AuditAction = "coordinator.rejected"
	// AuditKeysReplaced records a committed key submission.
	AuditKeysReplaced AuditAction = "keys.replaced"
	// AuditKeysGenerated records committed generated keys.
	AuditKeysGenerated AuditAction = "keys.generated"
	// AuditKeysApplyFailed records a revision the driver did not come back with.
	AuditKeysApplyFailed AuditAction = "keys.apply_failed"
	// AuditKeysExportRequested records an issued export confirmation token.
	AuditKeysExportRequested AuditAction = "keys.export_requested"
	// AuditKeysExported records a completed export.
	AuditKeysExported AuditAction = "keys.exported"
	// AuditKeysExportRejected records a bad, expired or consumed export token.
	AuditKeysExportRejected AuditAction = "keys.export_rejected"
	// AuditNetworkResetRequested records an issued reset confirmation token.
	AuditNetworkResetRequested AuditAction = "network.reset_requested"
	// AuditNetworkReset records a completed reset.
	AuditNetworkReset AuditAction = "network.reset"
	// AuditNetworkResetFailed records a reset the driver could not finish.
	AuditNetworkResetFailed AuditAction = "network.reset_failed"
)

// AuditOutcome is the result recorded for an audited action.
type AuditOutcome string

// The three outcomes of data-model.md §7.
const (
	// OutcomeSucceeded records an action that completed.
	OutcomeSucceeded AuditOutcome = "succeeded"
	// OutcomeFailed records an action that was attempted and did not complete.
	OutcomeFailed AuditOutcome = "failed"
	// OutcomeRejected records an action that was refused before it ran.
	OutcomeRejected AuditOutcome = "rejected"
)

// AuditDetail is the allow-list of `detail` keys (data-model.md §7). It is a
// struct rather than a map so a key outside the list cannot be written at all,
// which leaves key material nowhere to go even by mistake. The zero value
// marshals to `{}`, the column's default.
type AuditDetail struct {
	// Classes names the security classes the action concerned.
	Classes []string `json:"classes,omitempty"`
	// Source is supplied or generated.
	Source string `json:"source,omitempty"`
	// Reason is a short machine-readable cause.
	Reason string `json:"reason,omitempty"`
	// ExpectedHomeID is the network identifier the coordinator had to report.
	ExpectedHomeID string `json:"expected_home_id,omitempty"`
	// ActualHomeID is the network identifier it reported instead.
	ActualHomeID string `json:"actual_home_id,omitempty"`
	// Cleared reports which parts of a reset discarded data.
	Cleared *bool `json:"cleared,omitempty"`
}

// AuditEntry is one row of audit_entries: the durable record of a configuration
// change (FR-019). The table is append-only, and ID is monotonic, so it doubles
// as the pagination cursor.
type AuditEntry struct {
	// ID is the row id and the pagination cursor.
	ID int64
	// At is when the change was recorded. A zero At means now; the store
	// truncates it to milliseconds and writes it as RFC 3339 UTC.
	At time.Time
	// Action is one of the eleven defined actions.
	Action AuditAction
	// ActorAddr is the requesting client's network address, port stripped. It
	// is recorded, not authenticated.
	ActorAddr string
	// ActorLabel is the self-declared X-Actor-Label, at most 64 characters, and
	// unverified.
	ActorLabel string
	// RequestID correlates the row with the event stream and the API response.
	RequestID string
	// CoordinatorID is the coordinator the change bound to, if any.
	CoordinatorID string
	// SecurityClass is set for single-class actions, if any.
	SecurityClass string
	// Outcome is the result the action carries.
	Outcome AuditOutcome
	// Detail is the allow-listed JSON object; the zero value is stored as `{}`.
	Detail AuditDetail
}

// AuditQuery selects one page of the audit trail.
type AuditQuery struct {
	// Limit is the maximum page size. Zero means 50; any value is clamped into
	// 1..200.
	Limit int
	// Before returns entries with id less than this value. Zero or less starts
	// from the newest entry.
	Before int64
	// Action filters by action when non-empty. An undefined action is an error.
	Action AuditAction
}

// AuditPage is one page of the audit trail, newest first.
type AuditPage struct {
	// Items is the page itself, newest first. It is never nil, so an empty page
	// marshals as [].
	Items []AuditEntry
	// NextBefore is the cursor to pass as the next AuditQuery.Before, or nil
	// when this page reached the oldest entry.
	NextBefore *int64
}

// auditExecer is the one method insertAudit needs, satisfied by both *sql.DB
// and *sql.Tx. Keeping it this small is what lets a command write its audit row
// on the transaction that holds its state change (AppendAuditTx).
type auditExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// auditOutcome maps each defined action to the single outcome it may carry
// (data-model.md §7). A false second return means the action is not defined.
func auditOutcome(action AuditAction) (AuditOutcome, bool) {
	switch action {
	case AuditCoordinatorSelected, AuditKeysReplaced, AuditKeysGenerated,
		AuditKeysExportRequested, AuditKeysExported, AuditNetworkResetRequested,
		AuditNetworkReset:
		return OutcomeSucceeded, true
	case AuditCoordinatorRejected, AuditKeysExportRejected:
		return OutcomeRejected, true
	case AuditKeysApplyFailed, AuditNetworkResetFailed:
		return OutcomeFailed, true
	}
	return "", false
}

// auditValidate enforces the §7 constraints SQLite's CHECK clauses cannot
// express: the action is defined, the outcome is the one that action carries,
// the actor address and request id are present, and the label fits. Errors name
// the field and never its value, because the value is operator-supplied.
func auditValidate(entry AuditEntry) error {
	want, defined := auditOutcome(entry.Action)
	if !defined {
		return errors.New("store: audit action is not a defined action")
	}
	if entry.Outcome != want {
		return errors.New("store: audit outcome is not valid for the action")
	}
	if entry.ActorAddr == "" {
		return errors.New("store: audit actor_addr is empty")
	}
	if entry.RequestID == "" {
		return errors.New("store: audit request_id is empty")
	}
	if utf8.RuneCountInString(entry.ActorLabel) > auditMaxLabelRunes {
		return fmt.Errorf("store: audit actor_label is longer than %d characters", auditMaxLabelRunes)
	}
	return nil
}

// auditNull binds the empty string as SQL NULL. actor_label, coordinator_id and
// security_class are nullable columns, and an absent value must read as absent
// rather than as an empty string that looks supplied.
func auditNull(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

// auditPageSize applies the pagination bounds: zero is the default, and a value
// outside 1..200 is clamped into it.
func auditPageSize(limit int) int {
	if limit == 0 {
		return auditDefaultPageSize
	}
	if limit < 1 {
		return 1
	}
	if limit > auditMaxPageSize {
		return auditMaxPageSize
	}
	return limit
}

// auditScanRow reads one row of auditSelectSQL. The three nullable columns come
// back as sql.NullString; detail is NOT NULL and always holds a JSON object.
func auditScanRow(rows *sql.Rows) (AuditEntry, error) {
	var (
		entry         AuditEntry
		at            string
		action        string
		actorAddr     string
		actorLabel    sql.NullString
		requestID     string
		coordinatorID sql.NullString
		securityClass sql.NullString
		outcome       string
		detail        string
	)
	if err := rows.Scan(&entry.ID, &at, &action, &actorAddr, &actorLabel, &requestID,
		&coordinatorID, &securityClass, &outcome, &detail); err != nil {
		return AuditEntry{}, fmt.Errorf("store: scan audit entry: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: parse audit timestamp: %w", err)
	}
	entry.At = parsed
	entry.Action = AuditAction(action)
	entry.ActorAddr = actorAddr
	entry.ActorLabel = actorLabel.String
	entry.RequestID = requestID
	entry.CoordinatorID = coordinatorID.String
	entry.SecurityClass = securityClass.String
	entry.Outcome = AuditOutcome(outcome)
	if err := json.Unmarshal([]byte(detail), &entry.Detail); err != nil {
		return AuditEntry{}, fmt.Errorf("store: decode audit detail: %w", err)
	}
	return entry, nil
}

// insertAudit validates entry, applies its defaults and writes exactly one row.
// Every check runs before the statement, so an invalid entry costs no write. The
// new row id comes back from the database.
func insertAudit(ctx context.Context, ex auditExecer, entry AuditEntry) (int64, error) {
	if err := auditValidate(entry); err != nil {
		return 0, err
	}
	if entry.At.IsZero() {
		entry.At = time.Now().UTC()
	}
	detail, err := json.Marshal(entry.Detail)
	if err != nil {
		return 0, fmt.Errorf("store: encode audit detail: %w", err)
	}
	result, err := ex.ExecContext(ctx, auditInsertSQL,
		entry.At.UTC().Format(auditTimeLayout),
		string(entry.Action),
		entry.ActorAddr,
		auditNull(entry.ActorLabel),
		entry.RequestID,
		auditNull(entry.CoordinatorID),
		auditNull(entry.SecurityClass),
		string(entry.Outcome),
		string(detail),
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert audit entry: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: read audit entry id: %w", err)
	}
	return id, nil
}

// AppendAudit writes one audit row in a transaction of its own and returns its
// id. A command that changes state in the same breath should use WithTx with
// Tx.AppendAudit instead, so the change and its trail commit together.
func (s *Store) AppendAudit(ctx context.Context, entry AuditEntry) (int64, error) {
	var id int64
	err := s.WithTx(ctx, func(tx *Tx) error {
		var err error
		id, err = tx.AppendAudit(ctx, entry)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Tx is the store's transaction handle. It carries the audit writer and, as
// later phases land, the store operations a command needs; it deliberately does
// not expose *sql.Tx, so the hub and API packages never import database/sql or
// gain the ability to run arbitrary SQL (plan.md: internal/hub is "no SQL").
type Tx struct {
	tx *sql.Tx
}

// AppendAudit writes one audit row on this transaction. The row is durable only
// when the transaction commits, which is the point: a command's state change and
// its audit row succeed or fail as one.
func (t *Tx) AppendAudit(ctx context.Context, entry AuditEntry) (int64, error) {
	if t == nil || t.tx == nil {
		return 0, errors.New("store: transaction is nil")
	}
	return insertAudit(ctx, t.tx, entry)
}

// WithTx runs fn inside a transaction on the store's single connection: it
// commits when fn returns nil and rolls back when fn returns an error or panics.
// fn must use only the tx it is handed; calling a Store method inside fn would
// wait for the connection this transaction already holds, and the pool is capped
// at one, so it would block until ctx is cancelled.
func (s *Store) WithTx(ctx context.Context, fn func(tx *Tx) error) error {
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer raw.Rollback()
	if err := fn(&Tx{tx: raw}); err != nil {
		return err
	}
	if err := raw.Commit(); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	return nil
}

// ListAudit returns one page of the audit trail, newest first. Before is the
// cursor from the previous page's NextBefore (zero or less starts at the newest
// entry); Limit is read as 50 when zero and clamped into 1..200; Action filters
// when set, and an undefined action is an error. The page is filled one row past
// the limit so NextBefore can say whether another page exists without a second
// statement.
func (s *Store) ListAudit(ctx context.Context, q AuditQuery) (AuditPage, error) {
	if q.Action != "" {
		if _, defined := auditOutcome(q.Action); !defined {
			return AuditPage{}, errors.New("store: audit action filter is not a defined action")
		}
	}
	limit := auditPageSize(q.Limit)

	where := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if q.Before > 0 {
		where = append(where, "id < ?")
		args = append(args, q.Before)
	}
	if q.Action != "" {
		where = append(where, "action = ?")
		args = append(args, string(q.Action))
	}
	query := auditSelectSQL
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return AuditPage{}, fmt.Errorf("store: list audit entries: %w", err)
	}
	defer rows.Close()

	items := make([]AuditEntry, 0, limit)
	for rows.Next() {
		entry, err := auditScanRow(rows)
		if err != nil {
			return AuditPage{}, err
		}
		items = append(items, entry)
	}
	if err := rows.Err(); err != nil {
		return AuditPage{}, fmt.Errorf("store: read audit entries: %w", err)
	}

	page := AuditPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		next := page.Items[limit-1].ID
		page.NextBefore = &next
	}
	return page, nil
}
