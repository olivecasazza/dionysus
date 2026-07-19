// Package main is the dionysus manager entrypoint. It wires the
// HostedGameReconciler into a controller-runtime manager.
package main

import (
	"context"
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
	"github.com/olivecasazza/dionysus/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gamesv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Disabled by default since the operator runs as a single replica.")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New()
	ctrl.SetLogger(logger)
	setupLogger := logger.WithName("setup")

	cfg := ctrl.GetConfigOrDie()
	if _, err := rest.InClusterConfig(); err == nil {
		setupLogger.Info("running in-cluster")
	} else {
		setupLogger.Info("running with kubeconfig")
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "dionysus.casazza.io",
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{},
			},
		},
	})
	if err != nil {
		setupLogger.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.HostedGameReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLogger.Error(err, "unable to create controller")
		os.Exit(1)
	}

	// healthz / readyz: ping the Kubernetes API. If the API is reachable,
	// the controller is healthy.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLogger.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLogger.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLogger.Info("starting dionysus manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLogger.Error(err, "problem running manager")
		os.Exit(1)
	}

	_ = context.Background() // reserved for future graceful-shutdown hooks
}
