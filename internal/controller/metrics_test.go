package controller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The phase gauge must be one-hot: exactly the active phase is 1, the rest 0.
func TestRecordGameMetricsPhaseOneHot(t *testing.T) {
	game := &gamesv1alpha1.HostedGame{
		ObjectMeta: metav1.ObjectMeta{Name: "valheim", Namespace: "games"},
	}
	recordGameMetrics(game, gamesv1alpha1.HostedGameStatus{
		Phase:   gamesv1alpha1.PhaseRunning,
		Players: &gamesv1alpha1.PlayerStatus{Online: 3, Max: 10},
	})

	if got := testutil.ToFloat64(hostedGamePhase.WithLabelValues("valheim", "games", string(gamesv1alpha1.PhaseRunning))); got != 1 {
		t.Fatalf("active phase gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(hostedGamePhase.WithLabelValues("valheim", "games", string(gamesv1alpha1.PhaseStopped))); got != 0 {
		t.Fatalf("inactive phase gauge = %v, want 0", got)
	}
	if got := testutil.ToFloat64(hostedGamePlayersOnline.WithLabelValues("valheim", "games")); got != 3 {
		t.Fatalf("players_online = %v, want 3", got)
	}

	// After delete, the series is cleared (ToFloat64 on an absent series is 0).
	deleteGameMetrics("valheim", "games")
	if got := testutil.ToFloat64(hostedGamePhase.WithLabelValues("valheim", "games", string(gamesv1alpha1.PhaseRunning))); got != 0 {
		t.Fatalf("phase gauge after delete = %v, want 0 (cleared)", got)
	}
}
