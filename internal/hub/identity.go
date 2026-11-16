package hub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/judeibe/zwave_hub/internal/coordinator"
	"github.com/judeibe/zwave_hub/internal/store"
)

// controllerOf returns the Z-Wave identity a row holds, or nil when the driver
// has never identified the device.
func controllerOf(row store.CoordinatorRow) *store.ControllerIdentity {
	if row.FirmwareVersion == "" && row.SDKVersion == "" && row.ZWaveChipType == "" &&
		row.ZWaveManufacturerID == nil && row.ZWaveProductType == nil && row.ZWaveProductID == nil {
		return nil
	}
	return &store.ControllerIdentity{
		FirmwareVersion: row.FirmwareVersion,
		SDKVersion:      row.SDKVersion,
		ManufacturerID:  row.ZWaveManufacturerID,
		ProductType:     row.ZWaveProductType,
		ProductID:       row.ZWaveProductID,
		ChipType:        row.ZWaveChipType,
	}
}

// Identified is what the driver learned about a coordinator when it answered as
// a Z-Wave controller: the controller identity and the network it reported.
type Identified struct {
	Controller store.ControllerIdentity
	Network    Network
}

// Adopt records a successful identification: it stores the controller identity,
// moves the verdict unverified → supported, and marks the coordinator ready
// with its network. The order matters: the identity and the verdict are durable
// and land before the connection reads ready, so a client never sees ready for
// a coordinator the hub still calls unverified.
func (h *Hub) Adopt(ctx context.Context, id string, identified Identified) error {
	if err := h.store.SetControllerIdentity(ctx, id, identified.Controller); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("hub: adopt unknown coordinator %s: %w", id, err)
		}
		return fmt.Errorf("hub: record controller identity of %s: %w", id, err)
	}
	h.mu.Lock()
	if known, ok := h.known[id]; ok {
		controller := identified.Controller
		known.row.FirmwareVersion = controller.FirmwareVersion
		known.row.SDKVersion = controller.SDKVersion
		known.row.ZWaveManufacturerID = controller.ManufacturerID
		known.row.ZWaveProductType = controller.ProductType
		known.row.ZWaveProductID = controller.ProductID
		known.row.ZWaveChipType = controller.ChipType
	}
	h.mu.Unlock()

	if err := h.RecordVerdict(ctx, id, Verdict{Supported: true}); err != nil {
		return err
	}
	h.ClearDriverStatus(id)
	return h.SetConnection(ctx, id, ConnectionReady, &identified.Network)
}

// Reject records that identification found the device is not a Z-Wave
// controller. The verdict is final (FR-006) and detaches the selection if the
// device was active.
func (h *Hub) Reject(ctx context.Context, id string, reason coordinator.Reason) error {
	if err := h.RecordVerdict(ctx, id, Verdict{Supported: false, Reason: reason}); err != nil {
		return err
	}
	return h.SetConnection(ctx, id, ConnectionNotAttached, nil)
}

// SetDriverStatus records a non-ready driver state for a coordinator together
// with the retry time and last error the API presents. A ready report goes
// through Adopt, which clears both.
func (h *Hub) SetDriverStatus(ctx context.Context, id string, conn Connection, nextRetryAt *time.Time, lastError *LastError) error {
	if err := h.SetConnection(ctx, id, conn, nil); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	known, ok := h.known[id]
	if !ok {
		return fmt.Errorf("hub: record driver status for unknown coordinator %s: %w", id, store.ErrNotFound)
	}
	known.overlay.nextRetryAt = nextRetryAt
	if lastError != nil {
		held := *lastError
		known.overlay.lastError = &held
	}
	return nil
}

// ClearDriverStatus drops the retry time and last error once a start succeeds.
func (h *Hub) ClearDriverStatus(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if known, ok := h.known[id]; ok {
		known.overlay.nextRetryAt = nil
		known.overlay.lastError = nil
	}
}
