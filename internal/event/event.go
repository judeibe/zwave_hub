package event

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// schemaVersion is the envelope's `schema` value (contracts/events.md §6). It
// changes only for a breaking field change.
const schemaVersion = 1

// maxLogLineBytes caps a driver.log line (contracts/events.md §3.4).
const maxLogLineBytes = 4096

const (
	redacted   = "[redacted]"
	hexRunMin  = 32
	timeLayout = "2006-01-02T15:04:05.000Z07:00"
)

// Field keys. Together with the envelope keys they are the allow-list of
// contracts/events.md §2: the handler drops every attribute whose key is not
// in allowed, so a field cannot reach the stream by being added at a call
// site.
const (
	keyTS      = "ts"
	keyLevel   = "level"
	keySchema  = "schema"
	keyEvent   = "event"
	keyMsg     = "msg"
	keyRequest = "request_id"
	keyActor   = "actor_addr"
	keyLabel   = "actor_label"
	keyCoord   = "coordinator_id"
	keyHome    = "home_id"
)

// allowList is built once at construction. It is the envelope keys plus every
// field name in contracts/events.md §2 and §3; an attribute whose key is not
// here is dropped by the handler, which is what makes the never-send list
// structural rather than a review rule.
func allowList() map[string]bool {
	keys := []string{
		// envelope (§1)
		keyTS, keyLevel, keySchema, keyEvent, keyMsg,
		// context (§1)
		keyRequest, keyActor, keyLabel, keyCoord, keyHome,
		// identifiers (§2)
		"vid", "pid", "serial", "node_id", "security_class",
		// verdicts and counts (§2)
		"outcome", "reason", "code", "node_count", "secure_node_count",
		"revision", "applied_revision", "attempt", "consecutive_failures",
		"duration_ms", "start_ms", "status",
		// hardware descriptors (§2)
		"manufacturer", "product", "firmware_version", "sdk_version", "chip_type",
		// paths and configuration (§2)
		"serial_path", "data_dir", "bind", "log_level", "destructive_ops",
		// catalogue fields (§3.1–§3.6)
		"version", "go_version", "db_schema", "uptime_s", "field",
		"from_version", "to_version", "detail", "method", "route",
		"panic_type", "identity_kind", "last_path", "from", "to",
		"previous_coordinator_id", "schema_version", "exit_code", "signal",
		"next_retry_at", "since", "line", "classes", "source", "detected_by",
		"operation", "expires_at", "home_id_before",
	}
	allow := make(map[string]bool, len(keys))
	for _, key := range keys {
		allow[key] = true
	}
	return allow
}

// Correlation is the optional §1 context. A field is emitted only when it is
// non-empty, so a zero Correlation adds nothing to the record.
type Correlation struct {
	// RequestID is the X-Request-Id of the HTTP request that caused the event.
	RequestID string
	// ActorAddr is the client host, port stripped.
	ActorAddr string
	// ActorLabel is the self-declared X-Actor-Label, at most 64 characters.
	ActorLabel string
	// CoordinatorID is usb:<vid>:<pid>:<serial> or usb:<vid>:<pid>:port:<topology>.
	CoordinatorID string
	// HomeID is 0x followed by 8 hex digits, never a decimal.
	HomeID string
}

func (c Correlation) attrs() []slog.Attr {
	var out []slog.Attr
	add := func(key, value string) {
		if value != "" {
			out = append(out, slog.String(key, value))
		}
	}
	add(keyRequest, c.RequestID)
	add(keyActor, c.ActorAddr)
	add(keyLabel, sanitizeLabel(c.ActorLabel))
	add(keyCoord, c.CoordinatorID)
	add(keyHome, c.HomeID)
	return out
}

// sanitizeLabel enforces §1's rule for actor_label: at most 64 characters with
// control characters stripped, so a self-declared header cannot break the
// line-oriented stream.
func sanitizeLabel(label string) string {
	var b strings.Builder
	b.Grow(len(label))
	count := 0
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if count == 64 {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// Event is one entry of the closed catalogue in contracts/events.md §3. The
// unexported attrs method means only this package can define an event, so the
// emitter cannot be handed an arbitrary map or an ad-hoc field set.
type Event interface {
	// Name is the `domain.action` event name.
	Name() string
	// Level is the fixed level from the catalogue.
	Level() slog.Level
	// Message is the short constant sentence for the `msg` field.
	Message() string
	attrs() []slog.Attr
}

// Emitter writes catalogue events as one JSON object per line. It is the hub's
// only logging path (constitution VI). The zero Emitter is not usable; build
// one with New, and share it: it is safe for concurrent use.
type Emitter struct {
	logger *slog.Logger
}

// New returns an Emitter that writes JSON to w at or above level. The handler
// stamps ts, renames slog's default keys to the §1 envelope, adds schema, and
// applies the allow-list and the hex-run redaction.
func New(w io.Writer, level slog.Level) *Emitter {
	allow := allowList()
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return slog.Attr{}
			}
			switch a.Key {
			case slog.TimeKey:
				return slog.String(keyTS, a.Value.Time().UTC().Format(timeLayout))
			case slog.LevelKey:
				if l, ok := a.Value.Any().(slog.Level); ok {
					// ReplaceAttr returns one attribute, so schema rides in an
					// inline group (an empty key inlines its members) and every
					// record carries it without a call site remembering to add
					// it (events.md §1, §6).
					return slog.Group("",
						slog.String(keyLevel, levelName(l)),
						slog.Int(keySchema, schemaVersion),
					)
				}
			case slog.MessageKey:
				return slog.String(keyMsg, a.Value.String())
			}
			if !allow[a.Key] {
				return slog.Attr{}
			}
			return redact(a)
		},
	})
	return &Emitter{logger: slog.New(handler)}
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}

// redact is the defence in depth of §2: a string carrying a run of 32 or more
// hex characters is replaced whole, whatever field it is in.
func redact(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindString && hasHexRun(a.Value.String()) {
		return slog.String(a.Key, redacted)
	}
	if a.Value.Kind() == slog.KindAny {
		if list, ok := a.Value.Any().([]string); ok {
			var clean []string
			for i, s := range list {
				if !hasHexRun(s) {
					continue
				}
				if clean == nil {
					clean = append([]string(nil), list...)
				}
				clean[i] = redacted
			}
			if clean != nil {
				return slog.Any(a.Key, clean)
			}
		}
	}
	return a
}

func hasHexRun(s string) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			run++
			if run >= hexRunMin {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// Emit writes ev with the correlation fields that are set. The timestamp is
// taken by the handler, never by the caller.
func (e *Emitter) Emit(ctx context.Context, ev Event, corr Correlation) {
	// A driver.log line that looks like security details is dropped entirely
	// rather than masked, so the record is never written (events.md §3.4).
	if log, ok := ev.(*DriverLog); ok && looksSecret(log.Line) {
		return
	}
	if !e.logger.Enabled(ctx, ev.Level()) {
		return
	}
	fields := ev.attrs()
	attrs := make([]slog.Attr, 0, len(fields)+6)
	attrs = append(attrs, slog.String(keyEvent, ev.Name()))
	for _, a := range corr.attrs() {
		if !hasKey(fields, a.Key) {
			attrs = append(attrs, a)
		}
	}
	attrs = append(attrs, fields...)
	e.logger.LogAttrs(ctx, ev.Level(), ev.Message(), attrs...)
}

func hasKey(attrs []slog.Attr, key string) bool {
	for _, a := range attrs {
		if a.Key == key {
			return true
		}
	}
	return false
}

// Optional-field rule used by every event below: an optional field is a
// pointer and is omitted when nil (Code, ExitCode, Signal, NextRetryAt,
// PreviousCoordinatorID); a required field is a value and is always written,
// even when it is the zero value. No event omits a required field.

func optString(out []slog.Attr, key, value string) []slog.Attr {
	if value == "" {
		return out
	}
	return append(out, slog.String(key, value))
}

func optInt(out []slog.Attr, key string, value *int) []slog.Attr {
	if value == nil {
		return out
	}
	return append(out, slog.Int(key, *value))
}

func optTime(out []slog.Attr, key string, value *time.Time) []slog.Attr {
	if value == nil {
		return out
	}
	return append(out, slog.String(key, value.UTC().Format(timeLayout)))
}

func timeAttr(key string, t time.Time) slog.Attr {
	return slog.String(key, t.UTC().Format(timeLayout))
}

// classesAttr writes the security class names as a JSON array. A nil slice
// becomes [], so a consumer can iterate classes without a null check.
func classesAttr(classes []string) slog.Attr {
	if classes == nil {
		classes = []string{}
	}
	return slog.Any("classes", classes)
}

// --- §3.1 Lifecycle ---

// HubStarted is emitted once the hub is serving.
type HubStarted struct {
	Version   string
	GoVersion string
	// DBSchema is the store's user_version after migration.
	DBSchema int
	Bind     string
	DataDir  string
	// DestructiveOps is enabled or disabled.
	DestructiveOps string
}

func (e *HubStarted) Name() string      { return "hub.started" }
func (e *HubStarted) Level() slog.Level { return slog.LevelInfo }
func (e *HubStarted) Message() string   { return "hub started" }
func (e *HubStarted) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("version", e.Version),
		slog.String("go_version", e.GoVersion),
		slog.Int("db_schema", e.DBSchema),
		slog.String("bind", e.Bind),
		slog.String("data_dir", e.DataDir),
		slog.String("destructive_ops", e.DestructiveOps),
	}
}

// HubStopping is emitted once shutdown has begun.
type HubStopping struct {
	Reason string
	// UptimeS is the seconds since the hub started.
	UptimeS int64
}

func (e *HubStopping) Name() string      { return "hub.stopping" }
func (e *HubStopping) Level() slog.Level { return slog.LevelInfo }
func (e *HubStopping) Message() string   { return "hub stopping" }
func (e *HubStopping) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("reason", e.Reason),
		slog.Int64("uptime_s", e.UptimeS),
	}
}

// HubConfigInvalid is the only startup failure path; the process exits
// non-zero after it.
type HubConfigInvalid struct {
	Field  string
	Reason string
}

func (e *HubConfigInvalid) Name() string      { return "hub.config_invalid" }
func (e *HubConfigInvalid) Level() slog.Level { return slog.LevelError }
func (e *HubConfigInvalid) Message() string   { return "configuration invalid" }
func (e *HubConfigInvalid) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("field", e.Field),
		slog.String("reason", e.Reason),
	}
}

// StoreMigrated records a schema step.
type StoreMigrated struct {
	FromVersion int
	ToVersion   int
	DurationMS  int64
}

func (e *StoreMigrated) Name() string      { return "store.migrated" }
func (e *StoreMigrated) Level() slog.Level { return slog.LevelInfo }
func (e *StoreMigrated) Message() string   { return "store migrated" }
func (e *StoreMigrated) attrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("from_version", e.FromVersion),
		slog.Int("to_version", e.ToVersion),
		slog.Int64("duration_ms", e.DurationMS),
	}
}

// StoreIntegrityFailed carries a fixed detail code, never the SQLite message.
type StoreIntegrityFailed struct {
	Detail string
}

func (e *StoreIntegrityFailed) Name() string      { return "store.integrity_failed" }
func (e *StoreIntegrityFailed) Level() slog.Level { return slog.LevelError }
func (e *StoreIntegrityFailed) Message() string   { return "store integrity check failed" }
func (e *StoreIntegrityFailed) attrs() []slog.Attr {
	return []slog.Attr{slog.String("detail", e.Detail)}
}

// --- §3.2 HTTP boundary ---

// APIRequest is the access log: one event per request. Code is set only when
// Status is 400 or above.
type APIRequest struct {
	Method     string
	Route      string
	Status     int
	DurationMS int64
	Code       *string
}

func (e *APIRequest) Name() string      { return "api.request" }
func (e *APIRequest) Level() slog.Level { return slog.LevelInfo }
func (e *APIRequest) Message() string   { return "request served" }
func (e *APIRequest) attrs() []slog.Attr {
	attrs := []slog.Attr{
		slog.String("method", e.Method),
		slog.String("route", e.Route),
		slog.Int("status", e.Status),
		slog.Int64("duration_ms", e.DurationMS),
	}
	return optString(attrs, "code", deref(e.Code))
}

// APIPanic records the recovered panic's type and route only: no value and no
// stack, either of which can embed a key that was in scope.
type APIPanic struct {
	Route     string
	PanicType string
}

func (e *APIPanic) Name() string      { return "api.panic" }
func (e *APIPanic) Level() slog.Level { return slog.LevelError }
func (e *APIPanic) Message() string   { return "panic recovered" }
func (e *APIPanic) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("route", e.Route),
		slog.String("panic_type", e.PanicType),
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- §3.3 Discovery ---

// CoordinatorDiscovered is emitted for each device the USB poll finds. VID and
// PID are the lowercase hex strings used in coordinator_id, never decimal.
type CoordinatorDiscovered struct {
	IdentityKind string
	VID          string
	PID          string
	SerialPath   string
}

func (e *CoordinatorDiscovered) Name() string      { return "coordinator.discovered" }
func (e *CoordinatorDiscovered) Level() slog.Level { return slog.LevelInfo }
func (e *CoordinatorDiscovered) Message() string   { return "coordinator discovered" }
func (e *CoordinatorDiscovered) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("identity_kind", e.IdentityKind),
		slog.String("vid", e.VID),
		slog.String("pid", e.PID),
		slog.String("serial_path", e.SerialPath),
	}
}

// CoordinatorRemoved is emitted when a previously seen device disappears.
type CoordinatorRemoved struct {
	LastPath string
}

func (e *CoordinatorRemoved) Name() string      { return "coordinator.removed" }
func (e *CoordinatorRemoved) Level() slog.Level { return slog.LevelInfo }
func (e *CoordinatorRemoved) Message() string   { return "coordinator removed" }
func (e *CoordinatorRemoved) attrs() []slog.Attr {
	return []slog.Attr{slog.String("last_path", e.LastPath)}
}

// CoordinatorSupportChanged records a device moving between supported and
// unsupported.
type CoordinatorSupportChanged struct {
	From   string
	To     string
	Reason string
}

func (e *CoordinatorSupportChanged) Name() string      { return "coordinator.support_changed" }
func (e *CoordinatorSupportChanged) Level() slog.Level { return slog.LevelInfo }
func (e *CoordinatorSupportChanged) Message() string   { return "coordinator support changed" }
func (e *CoordinatorSupportChanged) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("from", e.From),
		slog.String("to", e.To),
		slog.String("reason", e.Reason),
	}
}

// CoordinatorSelected records a selection. PreviousCoordinatorID is nil when
// there was no previous selection; Reason is operator or restored.
type CoordinatorSelected struct {
	PreviousCoordinatorID *string
	Reason                string
}

func (e *CoordinatorSelected) Name() string      { return "coordinator.selected" }
func (e *CoordinatorSelected) Level() slog.Level { return slog.LevelInfo }
func (e *CoordinatorSelected) Message() string   { return "coordinator selected" }
func (e *CoordinatorSelected) attrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 2)
	attrs = optString(attrs, "previous_coordinator_id", deref(e.PreviousCoordinatorID))
	return append(attrs, slog.String("reason", e.Reason))
}

// CoordinatorRejected records a refused selection.
type CoordinatorRejected struct {
	Code string
}

func (e *CoordinatorRejected) Name() string      { return "coordinator.rejected" }
func (e *CoordinatorRejected) Level() slog.Level { return slog.LevelWarn }
func (e *CoordinatorRejected) Message() string   { return "coordinator rejected" }
func (e *CoordinatorRejected) attrs() []slog.Attr {
	return []slog.Attr{slog.String("code", e.Code)}
}

// --- §3.4 Driver ---

// DriverStarting is emitted after schema negotiation and before
// start_listening.
type DriverStarting struct {
	Attempt       int
	SerialPath    string
	SchemaVersion int
}

func (e *DriverStarting) Name() string      { return "driver.starting" }
func (e *DriverStarting) Level() slog.Level { return slog.LevelInfo }
func (e *DriverStarting) Message() string   { return "driver starting" }
func (e *DriverStarting) attrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("attempt", e.Attempt),
		slog.String("serial_path", e.SerialPath),
		slog.Int("schema_version", e.SchemaVersion),
	}
}

// DriverReady is emitted once start_listening has answered.
type DriverReady struct {
	SchemaVersion int
	// StartMS is the milliseconds from spawn to ready.
	StartMS         int64
	NodeCount       int
	SecureNodeCount int
}

func (e *DriverReady) Name() string      { return "driver.ready" }
func (e *DriverReady) Level() slog.Level { return slog.LevelInfo }
func (e *DriverReady) Message() string   { return "driver ready" }
func (e *DriverReady) attrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("schema_version", e.SchemaVersion),
		slog.Int64("start_ms", e.StartMS),
		slog.Int("node_count", e.NodeCount),
		slog.Int("secure_node_count", e.SecureNodeCount),
	}
}

// DriverFailed records one failed start attempt. ExitCode, Signal and
// NextRetryAt are nil when they do not apply: a spawn that never produced a
// process has no exit code, and a failure that pauses retries has no next
// attempt.
type DriverFailed struct {
	Attempt     int
	Reason      string
	ExitCode    *int
	Signal      *string
	DurationMS  int64
	NextRetryAt *time.Time
}

func (e *DriverFailed) Name() string      { return "driver.failed" }
func (e *DriverFailed) Level() slog.Level { return slog.LevelWarn }
func (e *DriverFailed) Message() string   { return "driver failed" }
func (e *DriverFailed) attrs() []slog.Attr {
	attrs := []slog.Attr{
		slog.Int("attempt", e.Attempt),
		slog.String("reason", e.Reason),
	}
	attrs = optInt(attrs, "exit_code", e.ExitCode)
	attrs = optString(attrs, "signal", deref(e.Signal))
	attrs = append(attrs, slog.Int64("duration_ms", e.DurationMS))
	return optTime(attrs, "next_retry_at", e.NextRetryAt)
}

// DriverStopped records a clean stop.
type DriverStopped struct {
	Reason string
}

func (e *DriverStopped) Name() string      { return "driver.stopped" }
func (e *DriverStopped) Level() slog.Level { return slog.LevelInfo }
func (e *DriverStopped) Message() string   { return "driver stopped" }
func (e *DriverStopped) attrs() []slog.Attr {
	return []slog.Attr{slog.String("reason", e.Reason)}
}

// DriverRestartPaused is emitted when retrying stops until the device set
// changes.
type DriverRestartPaused struct {
	ConsecutiveFailures int
	Since               time.Time
}

func (e *DriverRestartPaused) Name() string      { return "driver.restart_paused" }
func (e *DriverRestartPaused) Level() slog.Level { return slog.LevelError }
func (e *DriverRestartPaused) Message() string   { return "driver restarts paused" }
func (e *DriverRestartPaused) attrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("consecutive_failures", e.ConsecutiveFailures),
		timeAttr("since", e.Since),
	}
}

// DriverLog forwards one line of the child's own output. Severity follows the
// child. The line is capped at maxLogLineBytes; a line that looks like security
// details (key material, a 32-hex run, an authorization header, the DSK) is
// dropped entirely rather than masked, so Emit writes no record for it.
type DriverLog struct {
	Severity slog.Level
	Line     string
}

func (e *DriverLog) Name() string      { return "driver.log" }
func (e *DriverLog) Level() slog.Level { return e.Severity }
func (e *DriverLog) Message() string   { return "driver log line" }
func (e *DriverLog) attrs() []slog.Attr {
	return []slog.Attr{slog.String("line", capLine(e.Line))}
}

// looksSecret reports whether a child line must not be emitted at all.
func looksSecret(line string) bool {
	if hasHexRun(line) {
		return true
	}
	lower := strings.ToLower(line)
	for _, marker := range []string{
		"network key", "security key", "security details", "master key",
		"s0 key", "s2 key", "auth key", "private key", "key material",
		"ciphertext", "nonce", "dsk", "authorization", "cookie", "bearer ",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func capLine(line string) string {
	if len(line) <= maxLogLineBytes {
		return line
	}
	cut := maxLogLineBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut]
}

// --- §3.5 Keys ---

// KeysStored records an accepted key change. Classes names the security
// classes supplied; values are never carried.
type KeysStored struct {
	Revision int
	Classes  []string
	// Source is supplied or generated.
	Source string
}

func (e *KeysStored) Name() string      { return "keys.stored" }
func (e *KeysStored) Level() slog.Level { return slog.LevelInfo }
func (e *KeysStored) Message() string   { return "keys stored" }
func (e *KeysStored) attrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("revision", e.Revision),
		classesAttr(e.Classes),
		slog.String("source", e.Source),
	}
}

// KeysApplied records a key revision reaching the running driver.
type KeysApplied struct {
	Revision        int
	AppliedRevision int
	DurationMS      int64
}

func (e *KeysApplied) Name() string      { return "keys.applied" }
func (e *KeysApplied) Level() slog.Level { return slog.LevelInfo }
func (e *KeysApplied) Message() string   { return "keys applied" }
func (e *KeysApplied) attrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("revision", e.Revision),
		slog.Int("applied_revision", e.AppliedRevision),
		slog.Int64("duration_ms", e.DurationMS),
	}
}

// KeysApplyFailed records a key revision that could not be applied.
// AppliedRevision is the revision still in effect; 0 means none was ever
// applied.
type KeysApplyFailed struct {
	Revision        int
	AppliedRevision int
	Reason          string
}

func (e *KeysApplyFailed) Name() string      { return "keys.apply_failed" }
func (e *KeysApplyFailed) Level() slog.Level { return slog.LevelWarn }
func (e *KeysApplyFailed) Message() string   { return "keys apply failed" }
func (e *KeysApplyFailed) attrs() []slog.Attr {
	return []slog.Attr{
		slog.Int("revision", e.Revision),
		slog.Int("applied_revision", e.AppliedRevision),
		slog.String("reason", e.Reason),
	}
}

// KeysInUseRejected records a key change refused because a secure node is on
// the network.
type KeysInUseRejected struct {
	Classes         []string
	SecureNodeCount int
}

func (e *KeysInUseRejected) Name() string      { return "keys.in_use_rejected" }
func (e *KeysInUseRejected) Level() slog.Level { return slog.LevelInfo }
func (e *KeysInUseRejected) Message() string   { return "key change rejected while in use" }
func (e *KeysInUseRejected) attrs() []slog.Attr {
	return []slog.Attr{
		classesAttr(e.Classes),
		slog.Int("secure_node_count", e.SecureNodeCount),
	}
}

// KeysCorrupt is the key_store_corrupt alert trigger. Classes names the rows
// that failed, DetectedBy is aead or integrity_check.
type KeysCorrupt struct {
	Classes    []string
	DetectedBy string
}

func (e *KeysCorrupt) Name() string      { return "keys.corrupt" }
func (e *KeysCorrupt) Level() slog.Level { return slog.LevelError }
func (e *KeysCorrupt) Message() string   { return "key store corrupt" }
func (e *KeysCorrupt) attrs() []slog.Attr {
	return []slog.Attr{
		classesAttr(e.Classes),
		slog.String("detected_by", e.DetectedBy),
	}
}

// KeysExportRequested records an issued confirmation token. The token itself
// is never a field.
type KeysExportRequested struct {
	// Operation is keys.export or network.reset.
	Operation string
	ExpiresAt time.Time
}

func (e *KeysExportRequested) Name() string      { return "keys.export_requested" }
func (e *KeysExportRequested) Level() slog.Level { return slog.LevelInfo }
func (e *KeysExportRequested) Message() string   { return "key export confirmation issued" }
func (e *KeysExportRequested) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("operation", e.Operation),
		timeAttr("expires_at", e.ExpiresAt),
	}
}

// KeysExported records a completed export. HomeID is the event's own field
// because §3.5 names it; Emit keeps this copy and drops a duplicate from
// Correlation.
type KeysExported struct {
	Classes []string
	HomeID  string
}

func (e *KeysExported) Name() string      { return "keys.exported" }
func (e *KeysExported) Level() slog.Level { return slog.LevelWarn }
func (e *KeysExported) Message() string   { return "keys exported" }
func (e *KeysExported) attrs() []slog.Attr {
	return []slog.Attr{
		classesAttr(e.Classes),
		slog.String("home_id", e.HomeID),
	}
}

// KeysExportRejected records a refused redemption of an export token.
type KeysExportRejected struct {
	Reason string
}

func (e *KeysExportRejected) Name() string      { return "keys.export_rejected" }
func (e *KeysExportRejected) Level() slog.Level { return slog.LevelWarn }
func (e *KeysExportRejected) Message() string   { return "key export rejected" }
func (e *KeysExportRejected) attrs() []slog.Attr {
	return []slog.Attr{slog.String("reason", e.Reason)}
}

// --- §3.6 Network ---

// NetworkStateChanged records a state transition derived in data-model.md
// §8.2.
type NetworkStateChanged struct {
	From   string
	To     string
	Reason string
}

func (e *NetworkStateChanged) Name() string      { return "network.state_changed" }
func (e *NetworkStateChanged) Level() slog.Level { return slog.LevelInfo }
func (e *NetworkStateChanged) Message() string   { return "network state changed" }
func (e *NetworkStateChanged) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("from", e.From),
		slog.String("to", e.To),
		slog.String("reason", e.Reason),
	}
}

// NetworkExistingDetected records a stick carrying an unknown network, which
// blocks auto-generation (FR-018).
type NetworkExistingDetected struct {
	NodeCount int
}

func (e *NetworkExistingDetected) Name() string      { return "network.existing_detected" }
func (e *NetworkExistingDetected) Level() slog.Level { return slog.LevelWarn }
func (e *NetworkExistingDetected) Message() string   { return "existing network detected" }
func (e *NetworkExistingDetected) attrs() []slog.Attr {
	return []slog.Attr{slog.Int("node_count", e.NodeCount)}
}

// NetworkResetRequested records an issued reset token; nothing has been reset.
type NetworkResetRequested struct {
	ExpiresAt time.Time
}

func (e *NetworkResetRequested) Name() string      { return "network.reset_requested" }
func (e *NetworkResetRequested) Level() slog.Level { return slog.LevelWarn }
func (e *NetworkResetRequested) Message() string   { return "network reset confirmation issued" }
func (e *NetworkResetRequested) attrs() []slog.Attr {
	return []slog.Attr{timeAttr("expires_at", e.ExpiresAt)}
}

// NetworkResetStarted records a redeemed token and the reset now under way.
type NetworkResetStarted struct {
	HomeIDBefore string
}

func (e *NetworkResetStarted) Name() string      { return "network.reset_started" }
func (e *NetworkResetStarted) Level() slog.Level { return slog.LevelWarn }
func (e *NetworkResetStarted) Message() string   { return "network reset started" }
func (e *NetworkResetStarted) attrs() []slog.Attr {
	return []slog.Attr{slog.String("home_id_before", e.HomeIDBefore)}
}

// NetworkResetCompleted records a finished reset.
type NetworkResetCompleted struct {
	HomeIDBefore string
	DurationMS   int64
}

func (e *NetworkResetCompleted) Name() string      { return "network.reset_completed" }
func (e *NetworkResetCompleted) Level() slog.Level { return slog.LevelWarn }
func (e *NetworkResetCompleted) Message() string   { return "network reset completed" }
func (e *NetworkResetCompleted) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("home_id_before", e.HomeIDBefore),
		slog.Int64("duration_ms", e.DurationMS),
	}
}

// NetworkResetFailed records a reset that did not finish.
type NetworkResetFailed struct {
	Reason string
}

func (e *NetworkResetFailed) Name() string      { return "network.reset_failed" }
func (e *NetworkResetFailed) Level() slog.Level { return slog.LevelError }
func (e *NetworkResetFailed) Message() string   { return "network reset failed" }
func (e *NetworkResetFailed) attrs() []slog.Attr {
	return []slog.Attr{slog.String("reason", e.Reason)}
}
