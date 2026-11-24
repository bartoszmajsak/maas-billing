package tenantreconcile_test

import (
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile/fixture"
	pkgtest "github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Tenant TelemetryPolicy", func() {
	var (
		appNamespace string
		policy       unstructured.Unstructured
	)

	BeforeEach(func(ctx SpecContext) {
		appNamespace = pkgtest.NewTestNamespace(ctx, envTest).Name
		gatewayNamespace := pkgtest.NewTestNamespace(ctx, envTest, pkgtest.WithNameSuffix("gateway")).Name
		operands := fixture.RenderDefaultTenant(ctx, fixture.InNamespaces(appNamespace, gatewayNamespace))
		i := slices.IndexFunc(operands, func(obj unstructured.Unstructured) bool {
			return obj.GroupVersionKind() == tenantreconcile.GVKTelemetryPolicy
		})
		Expect(i).NotTo(Equal(-1), "the default tenant renders a TelemetryPolicy")
		policy = operands[i]
	})

	apply := func(ctx SpecContext) {
		GinkgoHelper()
		Expect(tenantreconcile.ApplyRendered(ctx, envTest.Client, envTest.Environment.Scheme,
			fixture.DefaultTenantConfig(), appNamespace, fixture.ConfigAnchor(), []unstructured.Unstructured{policy})).To(Succeed())
	}
	live := func(ctx SpecContext) *unstructured.Unstructured {
		GinkgoHelper()
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(tenantreconcile.GVKTelemetryPolicy)
		Expect(envTest.Client.Get(ctx, client.ObjectKeyFromObject(&policy), obj)).To(Succeed())
		return obj
	}
	labelsOf := func(obj *unstructured.Unstructured) map[string]any {
		labels, _, _ := unstructured.NestedMap(obj.Object, "spec", "metrics", "default", "labels")
		return labels
	}
	// addUserLabel does what a client-side apply of the old static policy in
	// deployment/base/observability did to the controller's object.
	addUserLabel := func(ctx SpecContext) {
		GinkgoHelper()
		patch := []byte(`{"spec":{"metrics":{"default":{"labels":{"user":"auth.identity.userid"}}}}}`)
		Expect(envTest.Client.Patch(ctx, live(ctx), client.RawPatch(types.MergePatchType, patch),
			client.FieldOwner("kubectl-client-side-apply"))).To(Succeed())
		Expect(labelsOf(live(ctx))).To(HaveKey("user"))
	}

	It("removes a label another writer added", func(ctx SpecContext) {
		apply(ctx)
		addUserLabel(ctx)

		apply(ctx)

		Expect(labelsOf(live(ctx))).To(Equal(labelsOf(&policy)))
	})

	It("leaves a policy marked managed=false alone", func(ctx SpecContext) {
		apply(ctx)
		addUserLabel(ctx)
		obj := live(ctx)
		obj.SetAnnotations(map[string]string{tenantreconcile.AnnotationManaged: "false"})
		Expect(envTest.Client.Update(ctx, obj)).To(Succeed())

		apply(ctx)

		Expect(labelsOf(live(ctx))).To(HaveKey("user"))
	})
})
