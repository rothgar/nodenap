// Command nodenap powers homelab machines on and off to keep the
// cluster's electricity cost low while every workload that can run, runs.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/rothgar/nodenap/api/v1alpha1"
	"github.com/rothgar/nodenap/internal/scaler"
)

func main() {
	var metricsAddr, probeAddr, namespace, selfNode string
	var leaderElect bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":18080", "Prometheus metrics address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":18081", "Health probe address.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Run one active replica.")
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"), "Namespace for Secrets and the leader lease.")
	flag.StringVar(&selfNode, "node-name", os.Getenv("NODE_NAME"), "Node this runs on; never powered off.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")
	if namespace == "" {
		namespace = "nodenap"
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "nodenap",
		LeaderElectionNamespace: namespace,
	})
	if err != nil {
		log.Error(err, "creating manager")
		os.Exit(1)
	}
	if err := mgr.Add(&scaler.Scaler{
		Client:       mgr.GetClient(),
		SecretReader: mgr.GetAPIReader(), // uncached: no cluster-wide Secret watch
		Recorder:     mgr.GetEventRecorder("nodenap"),
		Namespace:    namespace,
		SelfNode:     selfNode,
	}); err != nil {
		log.Error(err, "adding scaler")
		os.Exit(1)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)
	log.Info("starting", "namespace", namespace, "node", selfNode)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}
