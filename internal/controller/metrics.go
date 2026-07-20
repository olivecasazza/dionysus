package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
)

// Custom operator metrics for the game state Dionysus owns. Registered on the
// controller-runtime metrics registry, so they ride the existing :8080/metrics
// endpoint the chart's ServiceMonitor already scrapes — no extra server.

var trackedPhases = []gamesv1alpha1.GamePhase{
	gamesv1alpha1.PhaseStopped,
	gamesv1alpha1.PhaseStarting,
	gamesv1alpha1.PhaseRunning,
	gamesv1alpha1.PhaseStopping,
	gamesv1alpha1.PhaseFailed,
}

var (
	hostedGamePhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dionysus_hostedgame_phase",
		Help: "HostedGame lifecycle phase: 1 for the active phase, 0 for the others.",
	}, []string{"game", "namespace", "phase"})

	hostedGamePlayersOnline = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dionysus_hostedgame_players_online",
		Help: "Players currently online per HostedGame (from the query protocol).",
	}, []string{"game", "namespace"})

	hostedGamePlayersMax = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dionysus_hostedgame_players_max",
		Help: "Max player slots per HostedGame.",
	}, []string{"game", "namespace"})

	hostedGameBackupLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dionysus_hostedgame_backup_last_success_timestamp_seconds",
		Help: "Unix time of the last successful backup per HostedGame.",
	}, []string{"game", "namespace"})
)

func init() {
	metrics.Registry.MustRegister(
		hostedGamePhase,
		hostedGamePlayersOnline,
		hostedGamePlayersMax,
		hostedGameBackupLastSuccess,
	)
}

// recordGameMetrics publishes one HostedGame's observed state. Phase is a
// one-hot set (1 for the active phase) so PromQL can sum/filter by phase.
func recordGameMetrics(game *gamesv1alpha1.HostedGame, status gamesv1alpha1.HostedGameStatus) {
	name, ns := game.Name, game.Namespace
	for _, p := range trackedPhases {
		v := 0.0
		if status.Phase == p {
			v = 1.0
		}
		hostedGamePhase.WithLabelValues(name, ns, string(p)).Set(v)
	}
	if status.Players != nil {
		hostedGamePlayersOnline.WithLabelValues(name, ns).Set(float64(status.Players.Online))
		if status.Players.Max > 0 {
			hostedGamePlayersMax.WithLabelValues(name, ns).Set(float64(status.Players.Max))
		}
	}
	if status.Backup != nil && status.Backup.LastResult == "Succeeded" && status.Backup.LastScheduleTime != nil {
		hostedGameBackupLastSuccess.WithLabelValues(name, ns).Set(float64(status.Backup.LastScheduleTime.Unix()))
	}
}

// deleteGameMetrics clears a deleted HostedGame's series so dashboards don't
// show stale games after a HostedGame is removed.
func deleteGameMetrics(name, namespace string) {
	for _, p := range trackedPhases {
		hostedGamePhase.DeleteLabelValues(name, namespace, string(p))
	}
	hostedGamePlayersOnline.DeleteLabelValues(name, namespace)
	hostedGamePlayersMax.DeleteLabelValues(name, namespace)
	hostedGameBackupLastSuccess.DeleteLabelValues(name, namespace)
}
