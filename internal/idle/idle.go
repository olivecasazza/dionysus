// Package idle implements scale-to-zero for HostedGames whose player
// count has been zero for longer than spec.idle.timeoutMinutes. It is
// a pure-policy helper invoked from the main reconcile loop — there is
// no separate controller — so behavior stays coherent with workload
// rendering and status observation.
//
// State machine (per game, only when spec.idle.enabled):
//
//	Running, players > 0     → record last-non-zero timestamp
//	Running, players == 0    → if elapsed > timeout → scale Deployment to 0
//	Stopped (replicas == 0)  → no action; wake-on-connect or manual /start wakes it
//
// The last-non-zero timestamp lives in an annotation on the HostedGame
// (dionysus.io/last-non-zero-players, RFC3339). It cannot live in
// status because status fields represent observed state, not
// policy-evaluation bookkeeping. Using an annotation also keeps the
// idle package decoupled from any future status-shape changes.
package idle

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
	"github.com/olivecasazza/dionysus/internal/query"
)

// LastNonZeroAnnotation records the last time the player count was > 0
// for an idle-aware HostedGame. RFC3339 format. Controller-owned: the
// operator both reads and writes it.
const LastNonZeroAnnotation = "dionysus.io/last-non-zero-players"

// defaultTimeout matches the +kubebuilder:default=30 on IdlePolicy.
// Used when the field is explicitly zero (rare; CRD default usually
// fills it).
const defaultTimeout = 30 * time.Minute

// Decision is what Evaluate tells the controller to do.
type Decision struct {
	// Players is the freshly-observed player count to write into status.
	// Nil when no query was performed (idle disabled, game stopped).
	Players *gamesv1alpha1.PlayerStatus

	// ScaleToZero is true when the Deployment should be scaled to 0
	// replicas. The controller owns the actual patch; this is just the
	// recommendation.
	ScaleToZero bool

	// LastNonZeroAt is the timestamp the controller should persist into
	// the LastNonZeroAnnotation after this evaluation. Zero value means
	// "don't touch the annotation." Evaluate sets this to:
	//   - now                if players > 0
	//   - unchanged          if players == 0 (caller leaves annotation alone)
	//   - creationTimestamp  if players == 0 and annotation is missing
	//                        (bootstraps the timer so we don't immediately
	//                        scale down a freshly-created game)
	LastNonZeroAt *time.Time

	// RequeueAfter tells the controller when to check again. Idle-aware
	// games need tighter cadence than the controller's default 30s when
	// the check interval is configured shorter.
	RequeueAfter time.Duration
}

// Evaluate queries the game server for its player count and returns a
// Decision based on the idle policy. Returns a no-op Decision (nil
// players, no scale-down) when:
//   - spec.idle is nil or disabled
//   - the Deployment is already scaled to zero (someone else's decision)
//   - the Deployment is missing
//
// The caller is expected to:
//  1. Write Decision.Players into status.players (if non-nil)
//  2. Patch the LastNonZeroAnnotation if Decision.LastNonZeroAt is non-nil
//  3. Scale the Deployment to 0 if Decision.ScaleToZero
//  4. Requeue after Decision.RequeueAfter
func Evaluate(
	ctx context.Context,
	game *gamesv1alpha1.HostedGame,
	dep *appsv1.Deployment,
) (Decision, error) {
	out := Decision{}

	// Idle policy not configured → no-op.
	if game.Spec.Idle == nil || !game.Spec.Idle.Enabled {
		return out, nil
	}
	// No Deployment or Deployment already at 0 replicas → no-op.
	// (Wake-on-connect or a manual /start is what bumps replicas back
	// to 1; that's not this package's job.)
	if dep == nil || dep.Spec.Replicas == nil || *dep.Spec.Replicas == 0 {
		return out, nil
	}

	interval := intervalOrDefault(game)
	out.RequeueAfter = interval

	// Build the query spec with the in-cluster DNS name filled in if
	// the user left host empty. The query package expects the caller
	// to resolve this; we do it here so the controller stays simple.
	qspec := game.Spec.Idle.Query
	if qspec.Host == "" {
		qspec.Host = fmt.Sprintf("%s.%s.svc.cluster.local", game.Name, game.Namespace)
	}

	qclient, err := query.For(qspec)
	if err != nil {
		return out, fmt.Errorf("build query client: %w", err)
	}

	result, err := qclient.Query(ctx)
	if err != nil {
		// Query failure is recoverable. Record a sentinel player status
		// (online=-1) so dashboards can flag it, but don't scale down
		// based on a failed read — that would punish transient network
		// blips with an unnecessary game stop.
		out.Players = &gamesv1alpha1.PlayerStatus{
			Online:     -1,
			Max:        0,
			ObservedAt: metav1.Now(),
		}
		return out, fmt.Errorf("query %s/%s: %w", game.Namespace, game.Name, err)
	}

	now := time.Now()
	out.Players = &gamesv1alpha1.PlayerStatus{
		Online:     result.Online,
		Max:        result.Max,
		Names:      result.Names,
		ObservedAt: metav1.NewTime(now),
	}

	if result.Online > 0 {
		// Players present — reset the timer.
		out.LastNonZeroAt = &now
		return out, nil
	}

	// 0 players. Decide whether to scale down.
	lastNonZero, ok := readLastNonZero(game)
	if !ok {
		// Annotation missing. Bootstrap from the game's creation
		// timestamp so we don't immediately scale down a game that
		// has never been queried before — give it one full timeout
		// window to attract players.
		created := game.CreationTimestamp.Time
		out.LastNonZeroAt = &created
		lastNonZero = created
	}

	timeout := timeoutOrDefault(game)
	if now.Sub(lastNonZero) >= timeout {
		out.ScaleToZero = true
	}
	return out, nil
}

// intervalOrDefault returns spec.idle.intervalSeconds or the CRD default
// (120s) if zero. Negative values are treated as zero (use default).
func intervalOrDefault(game *gamesv1alpha1.HostedGame) time.Duration {
	if game.Spec.Idle.IntervalSeconds > 0 {
		return time.Duration(game.Spec.Idle.IntervalSeconds) * time.Second
	}
	return 120 * time.Second
}

// timeoutOrDefault returns spec.idle.timeoutMinutes or 30m if zero.
func timeoutOrDefault(game *gamesv1alpha1.HostedGame) time.Duration {
	if game.Spec.Idle.TimeoutMinutes > 0 {
		return time.Duration(game.Spec.Idle.TimeoutMinutes) * time.Minute
	}
	return defaultTimeout
}

// readLastNonZero parses the annotation. Returns ok=false if the
// annotation is missing or unparseable; the caller treats that as
// "never seen players" and bootstraps.
func readLastNonZero(game *gamesv1alpha1.HostedGame) (time.Time, bool) {
	if game.Annotations == nil {
		return time.Time{}, false
	}
	v, ok := game.Annotations[LastNonZeroAnnotation]
	if !ok || v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
