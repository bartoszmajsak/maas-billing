package tenantreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var istioTelemetryGVK = schema.GroupVersionKind{Group: "telemetry.istio.io", Version: "v1", Kind: "Telemetry"}

func policyWithLabels(gvk schema.GroupVersionKind, labels map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace("openshift-ingress")
	u.SetName("maas-telemetry")
	_ = unstructured.SetNestedMap(u.Object, labels, telemetryLabelsPath...)
	return u
}

func pruneTestClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{GVKTelemetryPolicy, istioTelemetryGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func liveLabels(t *testing.T, c client.Client, gvk schema.GroupVersionKind) (map[string]any, string) {
	t.Helper()
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(gvk)
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: "openshift-ingress", Name: "maas-telemetry"}, live))
	labels, _, _ := unstructured.NestedMap(live.Object, telemetryLabelsPath...)
	return labels, live.GetResourceVersion()
}

func TestPruneUnrenderedTelemetryLabels(t *testing.T) {
	rendered := map[string]any{"subscription": "s", "organization_id": "o"}

	t.Run("removes labels the controller doesn't render", func(t *testing.T) {
		c := pruneTestClient(t, interceptor.Funcs{}, policyWithLabels(GVKTelemetryPolicy,
			map[string]any{"subscription": "s", "organization_id": "o", "user": "auth.identity.userid", "a/b~c": "x"}))

		pruneUnrenderedTelemetryLabels(t.Context(), c, policyWithLabels(GVKTelemetryPolicy, rendered), rendered)

		labels, _ := liveLabels(t, c, GVKTelemetryPolicy)
		assert.Equal(t, rendered, labels)
	})

	t.Run("leaves a policy with only rendered labels untouched", func(t *testing.T) {
		c := pruneTestClient(t, interceptor.Funcs{}, policyWithLabels(GVKTelemetryPolicy, rendered))
		_, before := liveLabels(t, c, GVKTelemetryPolicy)

		pruneUnrenderedTelemetryLabels(t.Context(), c, policyWithLabels(GVKTelemetryPolicy, rendered), rendered)

		labels, after := liveLabels(t, c, GVKTelemetryPolicy)
		assert.Equal(t, rendered, labels)
		assert.Equal(t, before, after, "no patch expected")
	})

	t.Run("only TelemetryPolicies are pruned", func(t *testing.T) {
		labels, ok := renderedTelemetryLabels(policyWithLabels(GVKTelemetryPolicy, rendered))
		assert.True(t, ok)
		assert.Equal(t, rendered, labels)
		_, ok = renderedTelemetryLabels(policyWithLabels(istioTelemetryGVK, rendered))
		assert.False(t, ok)
		_, ok = renderedTelemetryLabels(policyWithLabels(GVKTelemetryPolicy, map[string]any{}))
		assert.False(t, ok, "a policy that renders no labels must not prune every live label")
	})

	t.Run("does nothing when the live policy is missing", func(t *testing.T) {
		c := pruneTestClient(t, interceptor.Funcs{})

		pruneUnrenderedTelemetryLabels(t.Context(), c, policyWithLabels(GVKTelemetryPolicy, rendered), rendered)

		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(GVKTelemetryPolicy)
		err := c.Get(t.Context(), client.ObjectKey{Namespace: "openshift-ingress", Name: "maas-telemetry"}, live)
		assert.True(t, apierrors.IsNotFound(err), "prune must not create the policy: %v", err)
	})

	t.Run("survives a failed patch", func(t *testing.T) {
		withUser := map[string]any{"subscription": "s", "organization_id": "o", "user": "u"}
		c := pruneTestClient(t, interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return errors.New("patch refused")
			},
		}, policyWithLabels(GVKTelemetryPolicy, withUser))

		pruneUnrenderedTelemetryLabels(t.Context(), c, policyWithLabels(GVKTelemetryPolicy, rendered), rendered)

		labels, _ := liveLabels(t, c, GVKTelemetryPolicy)
		assert.Equal(t, withUser, labels)
	})
}
