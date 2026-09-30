// Package testenv runs a bare API server and etcd for tests that depend on real API
// server behaviour, such as defaulting, update strategies and server-side apply. No
// controllers run, so nothing reconciles behind the test.
package testenv

import (
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Start runs envtest until the test ends and returns a client for it. The test is
// skipped when KUBEBUILDER_ASSETS is unset; `make test` sets it through setup-envtest.
func Start(t *testing.T, scheme *runtime.Scheme) client.Client { //nolint:ireturn // client.New only returns the interface.
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run through make test")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create envtest client: %v", err)
	}
	return c
}
