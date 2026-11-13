package api

import (
	"context"
	"net/http"

	"github.com/judeibe/zwave_hub/internal/hub"
)

// NetworkService is the part of the hub the network handler reads.
type NetworkService interface {
	NetworkStatus(ctx context.Context) (hub.NetworkStatus, error)
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

// getNetworkStatus reports the derived network status: the state of
// data-model.md §8.2, the readiness derivation behind it with its blocking
// reasons (data-model.md §8.3), and the key metadata both are read from.
func (n *Network) getNetworkStatus(w http.ResponseWriter, r *http.Request) {
	status, err := n.svc.NetworkStatus(r.Context())
	if err != nil {
		WriteProblem(w, r, NewProblem(CodeInternalError, "The network status could not be read."))
		return
	}
	writeJSON(w, r, http.StatusOK, buildNetworkStatus(status))
}

// buildNetworkStatus maps the hub's derived status to the contract shape. The
// state and the readiness come from the same derivation, so a state of ready
// always carries ready_for_secure_inclusion true and the reverse.
func buildNetworkStatus(status hub.NetworkStatus) networkStatusBody {
	body := networkStatusBody{
		State:                   string(status.State),
		ReadyForSecureInclusion: status.Readiness.Ready,
		BlockingReasons:         make([]blockingReasonBody, 0, len(status.Readiness.BlockingReasons)),
		ActiveCoordinatorID:     optional(status.ActiveID),
		HomeID:                  optional(status.HomeID),
		NodeCount:               status.NodeCount,
		SecureNodeCount:         status.SecureNodeCount,
		Keys: keysBody{
			Revision:          status.Keys.Revision,
			AppliedRevision:   status.Keys.AppliedRevision,
			InEffect:          status.Keys.InEffect,
			ClassicComplete:   status.Keys.ClassicComplete,
			ConfiguredClasses: configuredClasses(status.Keys),
		},
		KeyStore: keyStoreBody{State: string(status.KeyStore), Classes: emptyClasses(status.CorruptClasses)},
		Driver:   driverBody{State: driverState(status.Connection)},
	}
	for _, reason := range status.Readiness.BlockingReasons {
		body.BlockingReasons = append(body.BlockingReasons, presentBlockingReason(reason))
	}
	if status.LastError != nil {
		body.Driver.LastError = &lastErrorBody{
			Code: status.LastError.Code, Message: status.LastError.Message, At: timestamp(status.LastError.At),
		}
	}
	return body
}

// configuredClasses lists the classes that hold a key, in canonical order, as an
// empty list rather than null so a client can iterate it unconditionally.
func configuredClasses(keys hub.KeyConfiguration) []string {
	classes := make([]string, 0, len(keys.Classes))
	for _, class := range keys.Classes {
		if class.Configured {
			classes = append(classes, class.Class)
		}
	}
	return classes
}

// emptyClasses renders a nil list of classes as the empty list.
func emptyClasses(classes []string) []string {
	if classes == nil {
		return []string{}
	}
	return classes
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
