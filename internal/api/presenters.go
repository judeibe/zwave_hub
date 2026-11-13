package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/judeibe/zwave_hub/internal/hub"
	"github.com/judeibe/zwave_hub/internal/store"
)

// KeyValueBody is the marker a response body implements when the contract lets
// it carry key values: the confirmed export of FR-013, and nothing else. The
// hex-run rule of Present does not apply to such a body, because the body is
// assembled only after a confirmation token was redeemed; the never-send member
// names and the []byte rule still do.
//
// No current type implements it. T052 adds the export response shape and is the
// only intended implementer, so an implementation of this method anywhere else
// is a review finding, not a convenience.
type KeyValueBody interface {
	KeyValuesPermitted()
}

const (
	// presentRoot is the JSON path of the body itself. Refusals read as a JSON
	// path, so a call site can see which member was refused.
	presentRoot = "$"

	// presentMaxDepth bounds the recursive walk. The bodies of this API nest
	// four or five levels; sixteen is generous, and a body deeper than that is
	// a mistake or a cycle. The walk does not otherwise chase cycles, so the
	// cap is what stops it.
	presentMaxDepth = 16
)

// Present returns the JSON encoding of v, refusing v when the body could carry
// key material (FR-013, SC-004). It is the only constructor of response bodies:
// a handler that marshals a body itself bypasses the refusal, so it must not
// (data-model.md §11).
//
// The check is a recursive walk over the value that follows encoding/json's
// treatment of `json` tags and embedded structs. A body is refused when
//
//   - a JSON member name is on the never-send list (see presentForbiddenName),
//   - a value is a []byte, which encoding/json renders as base64 — a key
//     encoding like any other, or
//   - a string carries a run of 32 or more hex characters, the shape of a
//     Z-Wave key and of any hash of one.
//
// A body whose dynamic type implements KeyValueBody is the one exemption: it
// may carry key-shaped strings. Nothing else is lifted for it, and the
// exemption is decided from the body's type, not from a member of the body.
//
// The returned error names the JSON path and the rule, and never the offending
// value: the message can be quoted in a problem body or a log line without
// leaking anything. A body that nests deeper than presentMaxDepth is refused
// rather than walked further.
func Present(v any) ([]byte, error) {
	permit := presentKeyValuesPermitted(v)
	if err := presentCheck(reflect.ValueOf(v), presentRoot, 0, permit); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// presentKeyValuesPermitted reports whether v is the one body shape allowed to
// carry key values. The marker is tested on the dynamic type rather than by
// calling the method, so a nil pointer cannot panic the check and the marker
// cannot do work during presentation.
func presentKeyValuesPermitted(v any) bool {
	t := reflect.TypeOf(v)
	if t == nil {
		return false
	}
	return t.Implements(reflect.TypeOf((*KeyValueBody)(nil)).Elem())
}

// presentForbiddenName reports whether a JSON member name is one the contract
// forbids in a response body. The list is built from contracts/events.md §2 (the
// never-allowed table) and the key responses of contracts/openapi.yaml, not from
// guesswork: it names key values in every encoding the contract uses, the
// material that protects them, the DSK, and the rendered driver config.
//
// Two names are deliberately absent. `keys` is legitimate: NetworkStatus carries
// a metadata object under it, and the export carries values under it only behind
// the KeyValueBody exemption. `token` and `confirm_token` are legitimate too: the
// export challenge returns a live token by design.
//
// Matching folds the name — lowercased and with underscores removed — because a
// Go field without a json tag is rendered with its Go name, and `MasterKey` and
// `KeyValue` leak exactly as `master_key` and `key_value` do. The table is a
// switch rather than a package-level map so this file holds no state at all, the
// same shape as Code.Title.
func presentForbiddenName(name string) bool {
	switch strings.ToLower(strings.ReplaceAll(name, "_", "")) {
	case "keyvalue", "keyvalues", "keyhex", "keymaterial",
		"keyblob", "keyblobs",
		"masterkey", "ciphertext", "nonce", "plaintext", "secret",
		"dsk",
		"config", "renderedconfig":
		return true
	default:
		return false
	}
}

// presentCheck walks one value and refuses it when it could carry a secret. path
// is the JSON path the value will be rendered at, depth is how many containers
// have been entered, and permit is true only for a KeyValueBody, where the
// hex-run rule does not apply.
func presentCheck(v reflect.Value, path string, depth int, permit bool) error {
	if depth > presentMaxDepth {
		return presentRefuse(path, fmt.Sprintf("the body nests deeper than %d levels", presentMaxDepth))
	}
	if !v.IsValid() {
		return nil
	}

	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return presentCheck(v.Elem(), path, depth+1, permit)

	case reflect.Struct:
		return presentStruct(v, path, depth, permit)

	case reflect.Map:
		return presentMap(v, path, depth, permit)

	case reflect.Slice, reflect.Array:
		// encoding/json renders a byte slice as base64. A fixed-size byte array
		// is rendered as a list of numbers, and legitimate bodies use one
		// (uuid.UUID is [16]byte), so only the slice is refused.
		if v.Kind() == reflect.Slice && presentByteSequence(v) {
			return presentRefuse(path, "a []byte value renders as base64, which is a key encoding")
		}
		for i := 0; i < v.Len(); i++ {
			if err := presentCheck(v.Index(i), fmt.Sprintf("%s[%d]", path, i), depth+1, permit); err != nil {
				return err
			}
		}
		return nil

	case reflect.String:
		if !permit && hasHexRun(v.String()) {
			return presentRefuse(path, "a string carries a run of 32 or more hex characters")
		}
		return nil

	default:
		return nil
	}
}

// presentStruct walks the members encoding/json would render for a struct. A
// member json ignores — unexported, or tagged `json:"-"` — is not walked, so a
// key held in a field the body never shows is not a leak. Members of an embedded
// struct are walked at the outer path, which is where json renders them.
func presentStruct(v reflect.Value, path string, depth int, permit bool) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name, promoted, skip := presentMember(t.Field(i))
		if skip {
			continue
		}
		if name != "" && presentForbiddenName(name) {
			return presentRefuse(presentJoin(path, name), "the member name is on the never-send list")
		}
		fieldPath := path
		if !promoted {
			fieldPath = presentJoin(path, name)
		}
		if err := presentCheck(v.Field(i), fieldPath, depth+1, permit); err != nil {
			return err
		}
	}
	return nil
}

// presentMember reports the JSON member name of a struct field, whether the
// field is promoted from an embedded struct (json renders its members at the
// outer path), and whether json ignores the field entirely. The rules are the
// ones encoding/json applies: an unexported field is skipped unless it is an
// embedded struct, `json:"-"` is skipped, a tag name wins over the field name,
// and an untagged embedded struct is flattened.
func presentMember(f reflect.StructField) (name string, promoted, skip bool) {
	if f.Anonymous {
		t := f.Type
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		// json does not ignore an embedded field of an unexported struct type,
		// because its exported fields may still be rendered.
		if !f.IsExported() && t.Kind() != reflect.Struct {
			return "", false, true
		}
	} else if !f.IsExported() {
		return "", false, true
	}

	tag, tagged := f.Tag.Lookup("json")
	if tagged && tag == "-" {
		return "", false, true
	}
	if tagged {
		if member, _, _ := strings.Cut(tag, ","); member != "" {
			// `json:"-,"` names the member "-"; `json:"-"` was handled above.
			return member, false, false
		}
	}

	if f.Anonymous {
		t := f.Type
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() == reflect.Struct {
			return "", true, false
		}
	}
	return f.Name, false, false
}

// presentMap walks a map's members. A string key is a JSON member name, so it is
// held to the same rules as a member name and to the hex-run rule as well: a
// hex-run key would be rendered as the member name, which is still key material
// in the output. The iteration is not sorted; a map holds at most one offending
// member, so the refused path is the same whatever order it is found in.
func presentMap(v reflect.Value, path string, depth int, permit bool) error {
	if v.IsNil() {
		return nil
	}
	iter := v.MapRange()
	for iter.Next() {
		member := presentMapMember(iter.Key())
		if member != "" {
			if presentForbiddenName(member) {
				return presentRefuse(presentJoin(path, member), "the member name is on the never-send list")
			}
			if !permit && hasHexRun(member) {
				return presentRefuse(path, "a member name carries a run of 32 or more hex characters")
			}
		}
		if err := presentCheck(iter.Value(), presentJoin(path, member), depth+1, permit); err != nil {
			return err
		}
	}
	return nil
}

// presentMapMember is the member name encoding/json renders for a map key, as far
// as the check needs it: a string key as itself, an integer key as its decimal
// text. Any other key kind is rendered from an encoding.TextMarshaler, which the
// check does not call — user code does not run during presentation — so the path
// stops at the map while the value is still walked.
func presentMapMember(key reflect.Value) string {
	switch key.Kind() {
	case reflect.String:
		return key.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(key.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(key.Uint(), 10)
	default:
		return ""
	}
}

// presentByteSequence reports whether v is a slice of bytes, including a named
// type with a byte-slice underlying type such as json.RawMessage. Such a value
// is refused whatever its element type is called, because the bytes are rendered
// as base64 and the check cannot see what they hold.
func presentByteSequence(v reflect.Value) bool {
	return v.Type().Elem().Kind() == reflect.Uint8
}

// presentJoin extends a JSON path with one member name. An empty name leaves the
// path alone, which is what a map key the check cannot name leaves it.
func presentJoin(path, member string) string {
	if member == "" {
		return path
	}
	return path + "." + member
}

// presentRefuse builds the refusal from the JSON path and the rule. Neither the
// path nor the rule is ever the offending value, and the value is never quoted:
// this error travels to the client and to a log line, so it must not become the
// leak it exists to prevent (FR-013).
func presentRefuse(path, rule string) error {
	return fmt.Errorf("api: refusing response body: %s: %s", path, rule)
}

// This section holds the response shapes of the coordinator, network and audit
// resources. Handlers hand a value built here to writeJSON, which runs it
// through Present; none constructs a body of its own.

// coordinatorBody is contracts/openapi.yaml Coordinator. Pointer members render
// as JSON null, which the contract names for the optional ones.
type coordinatorBody struct {
	ID           string           `json:"id"`
	IdentityKind string           `json:"identity_kind"`
	USB          usbBody          `json:"usb"`
	Model        modelBody        `json:"model"`
	ZWave        *zwaveBody       `json:"zwave"`
	Support      string           `json:"support"`
	SupportRsn   *string          `json:"support_reason"`
	Attached     bool             `json:"attached"`
	Path         *string          `json:"path"`
	PathKind     *string          `json:"path_kind,omitempty"`
	Selectable   bool             `json:"selectable"`
	Connection   string           `json:"connection"`
	NextRetryAt  *string          `json:"next_retry_at"`
	LastError    *lastErrorBody   `json:"last_error"`
	Network      *coordNetworkOut `json:"network"`
	FirstSeenAt  string           `json:"first_seen_at"`
	LastSeenAt   string           `json:"last_seen_at"`
}

type usbBody struct {
	VID    string  `json:"vid"`
	PID    string  `json:"pid"`
	Serial *string `json:"serial"`
}

type modelBody struct {
	Manufacturer *string `json:"manufacturer"`
	Product      *string `json:"product"`
}

type zwaveBody struct {
	ManufacturerID  *int   `json:"manufacturer_id,omitempty"`
	ProductType     *int   `json:"product_type,omitempty"`
	ProductID       *int   `json:"product_id,omitempty"`
	FirmwareVersion string `json:"firmware_version,omitempty"`
	SDKVersion      string `json:"sdk_version,omitempty"`
	ChipType        string `json:"chip_type,omitempty"`
}

type lastErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	At      string `json:"at"`
}

type coordNetworkOut struct {
	HomeID          string `json:"home_id"`
	NodeCount       int    `json:"node_count"`
	SecureNodeCount int    `json:"secure_node_count"`
}

// coordinatorListBody is the body of listCoordinators.
type coordinatorListBody struct {
	Items               []coordinatorBody `json:"items"`
	ActiveCoordinatorID *string           `json:"active_coordinator_id"`
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	held := value
	return &held
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// PresentCoordinator maps the hub's view of one device to the contract shape.
// The network is shown only while the connection is ready, as the contract
// says; the hub keeps it longer for the FR-003 gate.
func presentCoordinator(c hub.Coordinator) coordinatorBody {
	body := coordinatorBody{
		ID:           c.ID,
		IdentityKind: string(c.IdentityKind),
		USB:          usbBody{VID: c.VID, PID: c.PID, Serial: optional(c.Serial)},
		Model:        modelBody{Manufacturer: optional(c.Manufacturer), Product: optional(c.Product)},
		Support:      string(c.Support),
		SupportRsn:   optional(string(c.SupportReason)),
		Attached:     c.Attached,
		Path:         optional(c.Path),
		Selectable:   c.Selectable,
		Connection:   string(c.Connection),
		FirstSeenAt:  timestamp(c.FirstSeenAt),
		LastSeenAt:   timestamp(c.LastSeenAt),
	}
	if c.PathKind != "" {
		kind := string(c.PathKind)
		body.PathKind = &kind
	}
	if c.Controller != nil {
		body.ZWave = &zwaveBody{
			ManufacturerID:  c.Controller.ManufacturerID,
			ProductType:     c.Controller.ProductType,
			ProductID:       c.Controller.ProductID,
			FirmwareVersion: c.Controller.FirmwareVersion,
			SDKVersion:      c.Controller.SDKVersion,
			ChipType:        c.Controller.ChipType,
		}
	}
	if c.NextRetryAt != nil {
		at := timestamp(*c.NextRetryAt)
		body.NextRetryAt = &at
	}
	if c.LastError != nil {
		body.LastError = &lastErrorBody{Code: c.LastError.Code, Message: c.LastError.Message, At: timestamp(c.LastError.At)}
	}
	if c.Connection == hub.ConnectionReady && c.Network != nil {
		body.Network = &coordNetworkOut{
			HomeID:          c.Network.HomeID,
			NodeCount:       c.Network.NodeCount,
			SecureNodeCount: c.Network.SecureNodeCount,
		}
	}
	return body
}

// presentCoordinatorList maps a snapshot to the list body. Items is never nil,
// so an empty hub renders [] rather than null (SC-008).
func presentCoordinatorList(snap hub.Snapshot) coordinatorListBody {
	items := make([]coordinatorBody, 0, len(snap.Coordinators))
	for _, c := range snap.Coordinators {
		items = append(items, presentCoordinator(c))
	}
	return coordinatorListBody{Items: items, ActiveCoordinatorID: optional(snap.ActiveID)}
}

// networkStatusBody is contracts/openapi.yaml NetworkStatus for the US1 scope:
// the key members report the empty state until the key phases land.
type networkStatusBody struct {
	State                   string               `json:"state"`
	ReadyForSecureInclusion bool                 `json:"ready_for_secure_inclusion"`
	BlockingReasons         []blockingReasonBody `json:"blocking_reasons"`
	ActiveCoordinatorID     *string              `json:"active_coordinator_id"`
	HomeID                  *string              `json:"home_id"`
	NodeCount               int                  `json:"node_count"`
	SecureNodeCount         int                  `json:"secure_node_count"`
	Keys                    keysBody             `json:"keys"`
	KeyStore                keyStoreBody         `json:"key_store"`
	Driver                  driverBody           `json:"driver"`
}

type blockingReasonBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type keysBody struct {
	Revision          int      `json:"revision"`
	AppliedRevision   *int     `json:"applied_revision"`
	InEffect          bool     `json:"in_effect"`
	ClassicComplete   bool     `json:"classic_complete"`
	ConfiguredClasses []string `json:"configured_classes"`
}

type keyStoreBody struct {
	State   string   `json:"state"`
	Classes []string `json:"classes"`
}

type driverBody struct {
	State               string         `json:"state"`
	Restarts            int            `json:"restarts"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	SchemaVersion       *int           `json:"schema_version"`
	LastError           *lastErrorBody `json:"last_error"`
}

// auditEntryBody is contracts/openapi.yaml AuditEntry. Detail is the closed
// store.AuditDetail struct, so only allow-listed keys can appear.
type auditEntryBody struct {
	ID            int64             `json:"id"`
	At            string            `json:"at"`
	Action        string            `json:"action"`
	ActorAddr     string            `json:"actor_addr"`
	ActorLabel    *string           `json:"actor_label"`
	RequestID     string            `json:"request_id"`
	CoordinatorID *string           `json:"coordinator_id"`
	SecurityClass *string           `json:"security_class"`
	Outcome       string            `json:"outcome"`
	Detail        store.AuditDetail `json:"detail"`
}

type auditPageBody struct {
	Items      []auditEntryBody `json:"items"`
	NextBefore *int64           `json:"next_before"`
}

func presentAuditPage(page store.AuditPage) auditPageBody {
	items := make([]auditEntryBody, 0, len(page.Items))
	for _, e := range page.Items {
		items = append(items, auditEntryBody{
			ID:            e.ID,
			At:            timestamp(e.At),
			Action:        string(e.Action),
			ActorAddr:     e.ActorAddr,
			ActorLabel:    optional(e.ActorLabel),
			RequestID:     e.RequestID,
			CoordinatorID: optional(e.CoordinatorID),
			SecurityClass: optional(e.SecurityClass),
			Outcome:       string(e.Outcome),
			Detail:        e.Detail,
		})
	}
	return auditPageBody{Items: items, NextBefore: page.NextBefore}
}
