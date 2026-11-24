package tenantreconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/oteljson"
)

const ssaFieldOwner = "maas-controller"

// ApplyRendered server-side-applies rendered objects with Config as controller owner.
//
// The cluster-scoped Config is a valid owner for namespaced resources in any namespace
// and for cluster-scoped operands. Tenant tracking labels are always applied so the
// reconciler can correlate resources with the namespace-local config object for status and debugging.
//
// Objects matched by skipConfigControllerOwnerRef (see configOwnerRefSkips) do not receive a
// Config controller ownerReference; they still receive tenant tracking labels. Add predicates
// there for future exceptions (e.g. shared config that must outlive Config GC).
func ApplyRendered(ctx context.Context, c client.Client, scheme *runtime.Scheme, tenant client.Object, appNs string, mcfg *maasv1alpha1.Config, objs []unstructured.Unstructured) error {
	if mcfg == nil || mcfg.UID == "" {
		return errors.New("config with UID is required for platform apply")
	}

	for i := range objs {
		u := objs[i].DeepCopy()

		// Skip resources whose live cluster copy has opendatahub.io/managed=false,
		// allowing operators to opt specific resources out of reconciliation.
		// The payload-processing-plugins ConfigMap is stamped with managed=false on
		// bootstrap/migrate (see preparePayloadProcessingPluginsConfigMapApply) so
		// subsequent reconciles leave user plugin edits alone unless they set
		// opendatahub.io/managed=true to opt back into continuous management.
		if isLiveResourceUnmanaged(ctx, c, u) {
			oteljson.FromContext(ctx).V(1).Info("Skipping SSA for resource with opendatahub.io/managed=false on cluster",
				"kind", u.GetKind(), "name", u.GetName(), "namespace", u.GetNamespace())
			continue
		}

		// Skip resources whose live cluster copy is already owned by a different
		// controller (e.g. ODH operator's ModelsAsService component). SSA-applying
		// over them would fail on immutable fields like spec.selector and produce
		// conflicting controller:true ownerReferences.
		if isOwnedByExternalController(ctx, c, u, mcfg.UID) {
			oteljson.FromContext(ctx).Info("Skipping SSA: resource owned by external controller",
				"kind", u.GetKind(), "namespace", u.GetNamespace(), "name", u.GetName())
			continue
		}

		if skipConfigControllerOwnerRef(u, appNs) {
			setTenantTrackingLabels(u, tenant)
		} else {
			if err := controllerutil.SetControllerReference(mcfg, u, scheme); err != nil {
				var already *controllerutil.AlreadyOwnedError
				if errors.As(err, &already) {
					oteljson.FromContext(ctx).Info("skipping Config controller reference: object already owned by another controller",
						"kind", u.GetKind(), "namespace", u.GetNamespace(), "name", u.GetName(),
						"existingOwner", already)
				} else {
					return fmt.Errorf("set controller reference (Config) on %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
				}
			}
			setTenantTrackingLabels(u, tenant)
		}
		preparePayloadProcessingPluginsConfigMapApply(ctx, c, u)
		unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
		unstructured.RemoveNestedField(u.Object, "metadata", "resourceVersion")
		unstructured.RemoveNestedField(u.Object, "status")
		// Read before the apply: Apply writes the live object back into u.
		telemetryLabels, isTelemetryPolicy := renderedTelemetryLabels(u)
		// ForceOwnership is intentional: maas-controller is the sole manager for
		// Tenant platform resources, ensuring a clean field-manager handoff.
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(ssaFieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
		}
		if isTelemetryPolicy {
			pruneUnrenderedTelemetryLabels(ctx, c, u, telemetryLabels)
		}
	}
	return nil
}

// telemetryLabelsPath is where a TelemetryPolicy keeps its metric labels.
var telemetryLabelsPath = []string{"spec", "metrics", "default", "labels"}

// renderedTelemetryLabels returns a copy of the metric labels of a rendered TelemetryPolicy.
// It returns false for any other kind and for a policy that renders no labels, which would
// otherwise read as every live label being stray.
func renderedTelemetryLabels(u *unstructured.Unstructured) (map[string]any, bool) {
	if u.GroupVersionKind().GroupKind() != GVKTelemetryPolicy.GroupKind() {
		return nil, false
	}
	labels, found, err := unstructured.NestedMap(u.Object, telemetryLabelsPath...)
	return labels, found && err == nil && len(labels) > 0
}

// pruneUnrenderedTelemetryLabels removes TelemetryPolicy metric labels the controller doesn't
// render. Server-side apply only removes fields its own field manager set, so a label added
// by another writer survives every apply. The static policy that install-observability.sh
// used to apply over the default tenant's left an unguarded user label behind that way.
// It runs after the apply, so the rendered labels are already present and the map can't be
// left empty. A failure is logged and retried on the next reconcile rather than blocking the
// remaining operands.
func pruneUnrenderedTelemetryLabels(ctx context.Context, c client.Client, policy *unstructured.Unstructured, renderedLabels map[string]any) {
	log := oteljson.FromContext(ctx)
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(policy.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(policy), live); err != nil {
		log.V(1).Info("Skipping TelemetryPolicy label prune: cannot read the live policy",
			"namespace", policy.GetNamespace(), "name", policy.GetName(), "error", err.Error())
		return
	}
	liveLabels, _, _ := unstructured.NestedMap(live.Object, telemetryLabelsPath...)

	removed := make(map[string]any)
	for key := range liveLabels {
		if _, ok := renderedLabels[key]; !ok {
			removed[key] = nil
		}
	}
	if len(removed) == 0 {
		return
	}
	keys := slices.Sorted(maps.Keys(removed))

	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"metrics": map[string]any{"default": map[string]any{"labels": removed}}},
	})
	if err != nil {
		log.Error(err, "Cannot build the TelemetryPolicy label prune patch", "labels", keys)
		return
	}
	if err := c.Patch(ctx, live, client.RawPatch(types.MergePatchType, patch)); err != nil {
		log.Error(err, "Cannot remove TelemetryPolicy labels the controller doesn't render",
			"namespace", live.GetNamespace(), "name", live.GetName(), "labels", keys)
		return
	}
	log.Info("Removed TelemetryPolicy labels the controller doesn't render",
		"namespace", live.GetNamespace(), "name", live.GetName(), "labels", keys)
}

// configOwnerRefSkip matches rendered objects that must not get Config as controller owner
// (invalid self-reference, or operands that should not cascade-delete with Config).
type configOwnerRefSkip func(u *unstructured.Unstructured, appNs string) bool

// configOwnerRefSkips is evaluated in order; add new predicates here for additional exceptions.
var configOwnerRefSkips = []configOwnerRefSkip{
	isMaaSControllerDeployment,
}

func skipConfigControllerOwnerRef(u *unstructured.Unstructured, appNs string) bool {
	for _, fn := range configOwnerRefSkips {
		if fn(u, appNs) {
			return true
		}
	}
	return false
}

func isMaaSControllerDeployment(u *unstructured.Unstructured, appNs string) bool {
	if appNs == "" || u.GetNamespace() != appNs {
		return false
	}
	return strings.EqualFold(u.GetKind(), "Deployment") && u.GetName() == MaaSControllerDeploymentName
}

func isLiveResourceUnmanaged(ctx context.Context, c client.Client, rendered *unstructured.Unstructured) bool {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(rendered.GroupVersionKind())
	key := client.ObjectKeyFromObject(rendered)
	if key.Name == "" {
		return false
	}
	if err := c.Get(ctx, key, live); err != nil {
		return false
	}
	ann := live.GetAnnotations()
	return ann != nil && ann[AnnotationManaged] == "false"
}

// isPayloadProcessingPluginsConfigMap reports whether u is the IPP plugins ConfigMap
// (default name or per-tenant suffix).
func isPayloadProcessingPluginsConfigMap(u *unstructured.Unstructured) bool {
	if u == nil || u.GetKind() != "ConfigMap" {
		return false
	}
	name := u.GetName()
	if name == PayloadProcessingPluginsConfigMapName {
		return true
	}
	return strings.HasPrefix(name, PayloadProcessingPluginsConfigMapName+"-")
}

// preparePayloadProcessingPluginsConfigMapApply stamps opendatahub.io/managed on the
// plugins ConfigMap about to be SSA'd:
//   - managed=false (default): next reconcile skips via isLiveResourceUnmanaged so
//     operators can edit response plugins (e.g. re-enable api-translation) without
//     the controller overwriting the ConfigMap.
//   - managed=true preserved when the live object already opted into continuous
//     reconciler management.
//
// Do not put managed=false in the source YAML: PostRender drops resources that
// already carry that annotation, which would prevent first-time creation.
func preparePayloadProcessingPluginsConfigMapApply(ctx context.Context, c client.Client, u *unstructured.Unstructured) {
	if !isPayloadProcessingPluginsConfigMap(u) {
		return
	}
	managedValue := "false"
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(u.GroupVersionKind())
	key := client.ObjectKeyFromObject(u)
	if key.Name != "" {
		if err := c.Get(ctx, key, live); err == nil {
			if ann := live.GetAnnotations(); ann != nil && ann[AnnotationManaged] == "true" {
				managedValue = "true"
			}
		}
	}
	ann := u.GetAnnotations()
	if ann == nil {
		ann = make(map[string]string)
	}
	ann[AnnotationManaged] = managedValue
	u.SetAnnotations(ann)
}

// isOwnedByExternalController returns true when the live cluster copy of the
// rendered resource has a controller:true ownerReference whose UID differs from
// the given Config UID. This prevents the tenant config reconciler from SSA-applying
// over resources the ODH operator's ModelsAsService component already owns,
// which would fail on immutable fields (spec.selector) and produce conflicting
// controller ownerReferences.
func isOwnedByExternalController(ctx context.Context, c client.Client, rendered *unstructured.Unstructured, configUID types.UID) bool {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(rendered.GroupVersionKind())
	key := client.ObjectKeyFromObject(rendered)
	if key.Name == "" {
		return false
	}
	if err := c.Get(ctx, key, live); err != nil {
		return false
	}
	for _, ref := range live.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.UID != configUID {
			return true
		}
	}
	return false
}

func setTenantTrackingLabels(obj *unstructured.Unstructured, tenant client.Object) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[LabelTenantName] = tenantTrackingName(tenant)
	labels[LabelTenantNamespace] = tenant.GetNamespace()
	obj.SetLabels(labels)
}

func tenantTrackingName(tenant client.Object) string {
	tenantLabels := tenant.GetLabels()
	if tenantLabels != nil && tenantLabels[LabelTenantName] != "" {
		return tenantLabels[LabelTenantName]
	}
	return tenant.GetName()
}
