//go:build integration

// Package integration runs the tenant-rbac-controller against a real Kubernetes
// cluster (Kind) so we exercise the production surface envtest can't reach:
// the admission chain, real RBAC enforcement, real namespace lifecycle, real
// CRD installation, and the real informer cache talking to a real API server.
//
// Run with:
//
//	make test-integration
//
// or directly:
//
//	go test -tags=integration -timeout 20m ./test/integration/...
//
// Requirements on the host:
//   - Docker daemon reachable (Kind needs it to create the node container).
//   - kubectl on PATH (used for `kubectl apply -f` of large CRD bundles —
//     loading the multi-doc YAML via the Go client is brittle and slow).
//   - Network access to fetch upstream CRDs at the pinned versions.
//
// What this suite does NOT cover:
//   - The container image itself (that's verified by the Docker build + Helm
//     chart's separate CI workflow). The operator runs in-process here, against
//     the Kind cluster's API, so admission/RBAC are real but the runtime is the
//     test binary. That's a deliberate trade — it shaves ~3 minutes off CI and
//     keeps the operator codepath identical (manager.New + Start).
package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/kind/pkg/cluster"
	kindcmd "sigs.k8s.io/kind/pkg/cmd"

	platformv1alpha1 "github.com/example/tenant-rbac-controller/api/v1alpha1"
	"github.com/example/tenant-rbac-controller/internal/controller"
)

// Pinned versions. Bumping these is a deliberate, reviewable change so the
// suite never silently picks up upstream breakage.
const (
	kindClusterName       = "tenant-rbac-it"
	kindNodeImage         = "kindest/node:v1.29.4"
	kubernetesVersion     = "v1.29.4"
	externalSecretsCRDURL = "https://raw.githubusercontent.com/external-secrets/external-secrets/v0.9.20/config/crds/bases/external-secrets.io_externalsecrets.yaml"
	argoAppProjectCRDURL  = "https://raw.githubusercontent.com/argoproj/argo-cd/v2.11.7/manifests/crds/appproject-crd.yaml"
	argoApplicationCRDURL = "https://raw.githubusercontent.com/argoproj/argo-cd/v2.11.7/manifests/crds/application-crd.yaml"
)

// Suite is the package-global test fixture. Initialised once in TestMain,
// torn down at the end of the package run.
type Suite struct {
	provider   *cluster.Provider
	kubeconfig string

	RESTConfig *rest.Config
	Client     client.Client
	Scheme     *runtime.Scheme

	// mgrCancel stops the in-process controller manager.
	mgrCancel context.CancelFunc
}

var suite *Suite

// TestMain is the lifecycle hook for the integration suite. We do everything
// in here so all *_test.go files in the package share the same Kind cluster.
func TestMain(m *testing.M) {
	if os.Getenv("SKIP_INTEGRATION") == "1" {
		fmt.Fprintln(os.Stderr, "SKIP_INTEGRATION=1 set; skipping suite")
		os.Exit(0)
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	s, err := bootstrap()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap failed: %v\n", err)
		_ = teardown(s)
		os.Exit(1)
	}
	suite = s

	code := m.Run()

	if err := teardown(s); err != nil {
		fmt.Fprintf(os.Stderr, "teardown failed: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// bootstrap builds the full test environment:
//  1. Create the Kind cluster (with a pinned 1.29 node image).
//  2. Export its kubeconfig so we can talk to it.
//  3. Install the operator's own CRDs from config/crd/bases.
//  4. Install upstream CRDs (external-secrets + ArgoCD).
//  5. Build the typed client and the scheme.
//  6. Start the controller-runtime manager in-process, in a goroutine.
func bootstrap() (*Suite, error) {
	s := &Suite{}

	logger := kindcmd.NewLogger()
	s.provider = cluster.NewProvider(cluster.ProviderWithLogger(logger))

	fmt.Fprintf(os.Stderr, "[suite] creating Kind cluster %q with node image %s\n", kindClusterName, kindNodeImage)
	if err := s.provider.Create(
		kindClusterName,
		cluster.CreateWithNodeImage(kindNodeImage),
		cluster.CreateWithWaitForReady(3*time.Minute),
	); err != nil {
		return s, fmt.Errorf("kind create: %w", err)
	}

	kubeconfig, err := s.provider.KubeConfig(kindClusterName, false)
	if err != nil {
		return s, fmt.Errorf("kind kubeconfig: %w", err)
	}
	tmp, err := os.CreateTemp("", "tenant-rbac-it-*.kubeconfig")
	if err != nil {
		return s, fmt.Errorf("temp kubeconfig: %w", err)
	}
	if _, err := tmp.WriteString(kubeconfig); err != nil {
		_ = tmp.Close()
		return s, fmt.Errorf("write kubeconfig: %w", err)
	}
	_ = tmp.Close()
	s.kubeconfig = tmp.Name()

	cfg, err := clientcmd.BuildConfigFromFlags("", s.kubeconfig)
	if err != nil {
		return s, fmt.Errorf("rest config: %w", err)
	}
	s.RESTConfig = cfg

	s.Scheme = runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s.Scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(s.Scheme))
	utilruntime.Must(platformv1alpha1.AddToScheme(s.Scheme))

	cl, err := client.New(cfg, client.Options{Scheme: s.Scheme})
	if err != nil {
		return s, fmt.Errorf("typed client: %w", err)
	}
	s.Client = cl

	if err := installOperatorCRDs(s); err != nil {
		return s, fmt.Errorf("operator CRDs: %w", err)
	}
	if err := installUpstreamCRDs(s); err != nil {
		return s, fmt.Errorf("upstream CRDs: %w", err)
	}

	if err := ensureNamespace(context.Background(), s.Client, "argocd"); err != nil {
		return s, fmt.Errorf("argocd namespace: %w", err)
	}
	if err := ensureNamespace(context.Background(), s.Client, "mtkp-platform"); err != nil {
		return s, fmt.Errorf("platform namespace: %w", err)
	}

	if err := startManager(s); err != nil {
		return s, fmt.Errorf("start manager: %w", err)
	}

	fmt.Fprintln(os.Stderr, "[suite] bootstrap complete")
	return s, nil
}

// teardown best-effort cleans up everything bootstrap created. It tolerates
// nil fields so we can call it even on partial bootstrap failure.
func teardown(s *Suite) error {
	if s == nil {
		return nil
	}
	if s.mgrCancel != nil {
		s.mgrCancel()
	}
	if s.kubeconfig != "" {
		_ = os.Remove(s.kubeconfig)
	}
	if s.provider != nil {
		fmt.Fprintf(os.Stderr, "[suite] deleting Kind cluster %q\n", kindClusterName)
		if err := s.provider.Delete(kindClusterName, ""); err != nil {
			return fmt.Errorf("kind delete: %w", err)
		}
	}
	return nil
}

// installOperatorCRDs applies the CRDs in config/crd/bases. We use
// `kubectl apply -f` because the directory may grow over time and we want
// glob semantics without reimplementing multi-doc YAML splitting.
func installOperatorCRDs(s *Suite) error {
	crdDir, err := repoPath("config", "crd", "bases")
	if err != nil {
		return err
	}
	return kubectlApply(s.kubeconfig, crdDir)
}

// installUpstreamCRDs downloads the pinned external-secrets and ArgoCD CRDs
// and applies them. We cache the downloads under testdata/ so subsequent runs
// don't hit GitHub (handy when iterating locally).
func installUpstreamCRDs(s *Suite) error {
	cacheDir, err := testdataPath()
	if err != nil {
		return err
	}
	downloads := []struct {
		url  string
		file string
	}{
		{externalSecretsCRDURL, filepath.Join(cacheDir, "external-secrets-crd.yaml")},
		{argoAppProjectCRDURL, filepath.Join(cacheDir, "argocd-appproject-crd.yaml")},
		{argoApplicationCRDURL, filepath.Join(cacheDir, "argocd-application-crd.yaml")},
	}

	for _, d := range downloads {
		if _, err := os.Stat(d.file); os.IsNotExist(err) {
			if err := fetchToFile(d.url, d.file); err != nil {
				return fmt.Errorf("download %s: %w", d.url, err)
			}
		}
		if err := kubectlApply(s.kubeconfig, d.file); err != nil {
			return fmt.Errorf("apply %s: %w", d.file, err)
		}
	}

	// Wait for the CRDs to be Established so the RESTMapper can resolve them
	// before the controller starts up — otherwise the first reconcile races
	// with CRD registration and fails on a "no matches for kind" error.
	wantedKinds := []string{
		"externalsecrets.external-secrets.io",
		"appprojects.argoproj.io",
		"applications.argoproj.io",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range wantedKinds {
		if err := waitForCRDEstablished(ctx, s.Client, name); err != nil {
			return fmt.Errorf("wait CRD %s: %w", name, err)
		}
	}
	return nil
}

// startManager wires the same Reconciler the production main.go uses and
// runs it against the Kind cluster. We disable leader election (single
// instance) and bind metrics to a random port to avoid clashes when the
// suite is run in parallel.
func startManager(s *Suite) error {
	mgr, err := ctrl.NewManager(s.RESTConfig, ctrl.Options{
		Scheme:                 s.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		return err
	}

	if err := (&controller.Reconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		PlatformNamespace: "mtkp-platform",
		ArgoCDNamespace:   "argocd",
	}).SetupWithManager(mgr); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.mgrCancel = cancel

	started := make(chan error, 1)
	go func() {
		started <- mgr.Start(ctx)
	}()

	// Wait until the manager's cache is synced before returning so tests
	// don't fire requests at a controller that hasn't started watching yet.
	syncCtx, syncCancel := context.WithTimeout(ctx, 60*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		return fmt.Errorf("manager cache failed to sync within 60s")
	}

	// Replace the client with the manager's cached client so reads go through
	// the same informer the controller uses. Writes still go to the API server.
	s.Client = mgr.GetClient()

	// If the manager exits early (panic, bad config) surface it now.
	select {
	case err := <-started:
		return fmt.Errorf("manager exited during startup: %w", err)
	case <-time.After(500 * time.Millisecond):
	}
	return nil
}

// ----- helpers -----

func repoPath(parts ...string) (string, error) {
	// The integration tests live at test/integration/, so two directories up
	// from the test file is the repo root.
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	return filepath.Join(append([]string{root}, parts...)...), nil
}

func testdataPath() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(wd, "testdata")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func fetchToFile(url, dst string) error {
	resp, err := http.Get(url) //nolint:gosec // URLs are constants pinned by version.
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, body, 0o644)
}

// kubectlApply shells out to `kubectl apply -f <path>`. We could re-implement
// multi-doc YAML splitting in Go, but `kubectl` is already on every CI runner
// that has a Kubernetes test, and this keeps the suite short.
func kubectlApply(kubeconfig, path string) error {
	cmd := exec.Command("kubectl", "--kubeconfig", kubeconfig, "apply", "-f", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func ensureNamespace(ctx context.Context, cl client.Client, name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := cl.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// waitForCRDEstablished polls a CRD until its Established condition flips True.
// This is the standard guard before doing any unstructured operations against
// a freshly-applied CRD.
func waitForCRDEstablished(ctx context.Context, cl client.Client, name string) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for CRD %s", name)
		case <-tick.C:
			var crd apiextensionsv1.CustomResourceDefinition
			if err := cl.Get(ctx, client.ObjectKey{Name: name}, &crd); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return err
			}
			for _, c := range crd.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					return nil
				}
			}
		}
	}
}
