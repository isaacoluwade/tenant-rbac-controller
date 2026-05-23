// Package main is the entry point for the tenant-rbac-controller binary.
//
// Responsibilities:
//   - Build the controller-runtime Manager with leader election + metrics + health probes.
//   - Register the API types (Tenant) into the scheme.
//   - Wire the Tenant reconciler and start the manager.
//
// Configuration is read from CLI flags so the binary can be driven the same
// way by Kubernetes, by a developer running locally with `make run`, or by a
// CI harness.
package main

import (
	"flag"
	"os"

	// +kubebuilder:scaffold:imports

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	platformv1alpha1 "github.com/example/tenant-rbac-controller/api/v1alpha1"
	"github.com/example/tenant-rbac-controller/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(platformv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		platformNamespace    string
		argocdNamespace      string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election so only one instance reconciles at a time.")
	flag.StringVar(&platformNamespace, "platform-namespace", "mtkp-platform",
		"Namespace allowed to ingress into tenant namespaces via the baseline NetworkPolicy.")
	flag.StringVar(&argocdNamespace, "argocd-namespace", "argocd",
		"Namespace where AppProject and Application resources are managed.")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "tenant-rbac-controller.mtkp.platform",
		WebhookServer:          webhook.NewServer(webhook.Options{Port: 9443}),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.Reconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		PlatformNamespace: platformNamespace,
		ArgoCDNamespace:   argocdNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create tenant controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
