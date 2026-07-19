// Package query speaks the player-count protocols game servers expose:
// Valve A2S (Valheim, Zomboid, Rust, CS) and Minecraft Server List Ping.
// The package is pure stdlib — no k8s client — so it can be unit-tested
// in isolation and reused outside the operator.
package query

import (
	"context"
	"fmt"
	"net"
	"time"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
)

// Result is the normalized player-count observation.
type Result struct {
	// Online players right now.
	Online int32
	// Max players the server will accept.
	Max int32
	// Names of online players, if the protocol exposes them.
	Names []string
}

// Client queries one game server for its current player count.
type Client interface {
	Query(ctx context.Context) (Result, error)
}

// defaultTimeout caps each protocol's I/O. Game-query endpoints are
// occasionally wedged (CPU-starved host, broken mod, misconfigured
// query port) and a hung dial would otherwise stall the reconcile loop.
const defaultTimeout = 5 * time.Second

// For returns the appropriate Client for the given QuerySpec. It does
// not resolve the host:port — the caller (controller) must populate
// spec.Host with the in-cluster service DNS name before calling For(),
// because this package has no knowledge of how Services are named.
func For(spec gamesv1alpha1.QuerySpec) (Client, error) {
	switch spec.Type {
	case gamesv1alpha1.QueryTypeA2S:
		return NewA2S(spec), nil
	case gamesv1alpha1.QueryTypeMinecraft:
		return NewMinecraft(spec), nil
	default:
		return nil, fmt.Errorf("unsupported query type %q", spec.Type)
	}
}

// addrFromSpec builds host:port for the configured query endpoint.
// spec.Host is normally filled by the controller before constructing a
// client (defaults to "<game>.<ns>.svc.cluster.local"); spec.Port is
// required and is the protocol-specific query port.
func addrFromSpec(spec gamesv1alpha1.QuerySpec) string {
	return net.JoinHostPort(spec.Host, fmt.Sprintf("%d", spec.Port))
}
