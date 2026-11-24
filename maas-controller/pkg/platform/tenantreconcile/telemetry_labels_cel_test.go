package tenantreconcile

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// TestTelemetryIdentityLabelsResolveForEveryIdentityShape evaluates each auth.* label the way
// Authorino does (auth bound as google.protobuf.Struct, as in its pkg/expressions/cel)
// against the shapes auth.identity takes at the metric call sites. Authorino logs every label
// that errors, at every call site; the wasm-shim fails the request on one it can't resolve.
func TestTelemetryIdentityLabelsResolveForEveryIdentityShape(t *testing.T) {
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: maasv1alpha1.MaasTenantConfigInstanceName, Namespace: "models-as-a-service"},
	}
	labels := buildTelemetryLabels(logr.Discard(), &maasv1alpha1.TenantTelemetryConfig{
		Metrics: &maasv1alpha1.TenantMetricsConfig{CaptureUser: boolPtr(true), CaptureGroup: boolPtr(true)},
	}, tenant)

	resolved := map[string]string{
		"subscription":    "premium",
		"organization_id": "org-1",
		"cost_center":     "cc-1",
		"user":            "alice",
		"group":           "team-a,team-b",
	}
	shapes := []struct {
		name string
		auth string
		want map[string]string
	}{
		{name: "before the identity phase", auth: `{}`},
		{name: "API key, auth.identity is the credential", auth: `{"identity": "Bearer sk-oai-example"}`},
		{name: "TokenReview identity without these fields", auth: `{"identity": {"user": {"username": "alice"}}}`},
		{name: "identity filter without subscription info", auth: `{"identity": {"selected_subscription": "", "subscription_info": {}, "userid": "alice", "groups_str": ""}}`,
			want: map[string]string{"subscription": "", "user": "alice", "group": ""}},
		{name: "identity filter", auth: `{"identity": {
			"selected_subscription": "premium",
			"subscription_info": {"organizationId": "org-1", "costCenter": "cc-1"},
			"userid": "alice",
			"groups_str": "team-a,team-b"}}`, want: resolved},
	}

	for key := range resolved {
		expression, ok := labels[key].(string)
		require.True(t, ok, "label %q", key)
		program := compileAuthorinoCEL(t, expression)
		for _, shape := range shapes {
			out, _, err := program.Eval(map[string]any{"auth": authValue(t, shape.auth)})
			require.NoError(t, err, "label %q, %s", key, shape.name)
			assert.Equal(t, shape.want[key], out.Value(), "label %q, %s", key, shape.name)
		}
	}
}

func compileAuthorinoCEL(t *testing.T, expression string) cel.Program {
	t.Helper()
	env, err := cel.NewEnv(
		cel.Variable("auth", cel.ObjectType("google.protobuf.Struct")),
		ext.Strings(), cel.OptionalTypes(),
	)
	require.NoError(t, err)
	ast, issues := env.Compile(expression)
	require.NoError(t, issues.Err(), expression)
	program, err := env.Program(ast)
	require.NoError(t, err)
	return program
}

func authValue(t *testing.T, auth string) *structpb.Value {
	t.Helper()
	var data structpb.Struct
	require.NoError(t, protojson.Unmarshal([]byte(`{"auth": `+auth+`}`), &data))
	return data.GetFields()["auth"]
}
