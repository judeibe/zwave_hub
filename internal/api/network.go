package api

import (
	"net/http"

	"github.com/judeibe/zwave_hub/internal/hub"
)

// NetworkService is the part of the hub the network handler reads.
type NetworkService interface {
	Snapshot() hub.Snapshot
}

// Network serves the network status resource.
type Network struct {
	svc NetworkService
}

// NewNetwork returns the handler over svc.
func NewNetwork(svc NetworkService) *Network {
	return &Network{svc: svc}
}

// Mount registers the network routes.
func (n *Network) Mount(rt *Router) {
	rt.Handle("GET /api/v1/network", http.HandlerFunc(n.getNetworkStatus))
}

// getNetworkStatus reports state, the coordinator's connection and its
// membership for the US1 scope. Key fields report the empty state until the key
// phases land, so the shape is already the contract's.
func (n *Network) getNetworkStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, buildNetworkStatus(n.svc.Snapshot()))
}

// buildNetworkStatus derives the state of data-model.md §8.2 from a snapshot.
func buildNetworkStatus(snap hub.Snapshot) networkStatusBody {
	body := networkStatusBody{
		BlockingReasons: []blockingReasonBody{},
		Keys: keysBody{
			Revision:          snap.KeyRevision,
			ConfiguredClasses: []string{},
		},
		KeyStore: keyStoreBody{State: "ok", Classes: []string{}},
		Driver:   driverBody{State: "stopped"},
	}
	body.ActiveCoordinatorID = optional(snap.ActiveID)
	body.HomeID = optional(snap.HomeID)

	active, found := findCoordinator(snap, snap.ActiveID)
	switch {
	case snap.ActiveID == "" || !found:
		body.State = "unprovisioned"
		body.BlockingReasons = append(body.BlockingReasons, blockingReasonBody{
			Code: "no_active_coordinator", Message: "No coordinator is selected.",
		})
	default:
		body.Driver.State = driverState(active.Connection)
		if active.LastError != nil {
			body.Driver.LastError = &lastErrorBody{
				Code: active.LastError.Code, Message: active.LastError.Message, At: timestamp(active.LastError.At),
			}
		}
		if active.Network != nil && active.Connection == hub.ConnectionReady {
			body.NodeCount = active.Network.NodeCount
			body.SecureNodeCount = active.Network.SecureNodeCount
		}
		switch active.Connection {
		case hub.ConnectionReady:
			body.State = "ready"
		case hub.ConnectionFaulted:
			body.State = "faulted"
			body.BlockingReasons = append(body.BlockingReasons, blockingReasonBody{
				Code: "coordinator_faulted", Message: "The coordinator is present but the driver cannot start.",
			})
		case hub.ConnectionNotAttached:
			body.State = "awaiting_coordinator"
			body.BlockingReasons = append(body.BlockingReasons, blockingReasonBody{
				Code: "coordinator_not_attached", Message: "The selected coordinator is not attached.",
			})
		default:
			body.State = "awaiting_coordinator"
			body.BlockingReasons = append(body.BlockingReasons, blockingReasonBody{
				Code: "coordinator_not_attached", Message: "The selected coordinator is not ready yet.",
			})
		}
	}
	if snap.ActiveID != "" {
		body.BlockingReasons = append(body.BlockingReasons, blockingReasonBody{
			Code: "keys_missing", Message: "No security keys are configured yet.",
		})
	}
	return body
}

// driverState maps a coordinator connection to the driver states of the
// contract.
func driverState(c hub.Connection) string {
	switch c {
	case hub.ConnectionConnecting:
		return "starting"
	case hub.ConnectionReady:
		return "ready"
	case hub.ConnectionRetrying:
		return "retrying"
	case hub.ConnectionFaulted:
		return "faulted"
	default:
		return "stopped"
	}
}
