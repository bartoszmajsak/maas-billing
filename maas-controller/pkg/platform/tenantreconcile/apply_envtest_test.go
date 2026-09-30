package tenantreconcile

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/internal/testenv"
)

// The tenant reconciler watches what it applies, so an object the API server rewrites
// on every apply of unchanged content re-triggers the reconcile in a loop. Kinds whose
// CRDs envtest does not serve (Istio, Kuadrant, Gateway API, ...) are skipped: custom
// resources are stored as sent, so an unchanged apply leaves them alone.
func TestApplyRenderedIsIdempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, maasv1alpha1.AddToScheme(scheme))
	c := testenv.Start(t, scheme)

	config := &maasv1alpha1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.ConfigInstanceName, UID: "envtest-config"},
	}

	for _, bundledPostgres := range []bool{true, false} {
		t.Run(fmt.Sprintf("bundledPostgres=%t", bundledPostgres), func(t *testing.T) {
			appNamespace := fmt.Sprintf("maas-infra-%t", bundledPostgres)
			tenant := &maasv1alpha1.MaasTenantConfig{
				ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "models-as-a-service"},
			}
			objs := servedResources(t, c, renderDefaultTenant(t, appNamespace, bundledPostgres))
			createNamespaces(t, c, objs)

			require.NoError(t, ApplyRendered(t.Context(), c, scheme, tenant, appNamespace, config, objs))
			applied := resourceVersions(t, c, objs)

			require.NoError(t, ApplyRendered(t.Context(), c, scheme, tenant, appNamespace, config, objs))
			reapplied := resourceVersions(t, c, objs)

			var changed []string
			for key, rv := range applied {
				if reapplied[key] != rv {
					changed = append(changed, key)
				}
			}
			slices.Sort(changed)
			assert.Empty(t, changed, "re-applying unchanged manifests changed these objects")
		})
	}
}

func servedResources(t *testing.T, c client.Client, resources []unstructured.Unstructured) []unstructured.Unstructured {
	t.Helper()

	var served []unstructured.Unstructured
	for _, r := range resources {
		gvk := r.GroupVersionKind()
		if _, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			require.True(t, meta.IsNoMatchError(err), "map %s: %v", gvk, err)
			continue
		}
		served = append(served, r)
	}
	require.NotEmpty(t, served)
	return served
}

func createNamespaces(t *testing.T, c client.Client, objs []unstructured.Unstructured) {
	t.Helper()

	for _, obj := range objs {
		if obj.GetNamespace() == "" {
			continue
		}
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: obj.GetNamespace()}}
		if err := c.Create(t.Context(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
			require.NoError(t, err)
		}
	}
}

func resourceVersions(t *testing.T, c client.Client, objs []unstructured.Unstructured) map[string]string {
	t.Helper()

	versions := make(map[string]string, len(objs))
	for _, obj := range objs {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(&obj), live))
		versions[fmt.Sprintf("%s %s/%s", obj.GetKind(), obj.GetNamespace(), obj.GetName())] = live.GetResourceVersion()
	}
	return versions
}
