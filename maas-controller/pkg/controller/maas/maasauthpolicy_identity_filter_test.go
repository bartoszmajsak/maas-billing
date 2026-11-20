/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package maas

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	identityFilterModelPath   = "/llm/model-a/v1/chat/completions"
	identityFilterOrgID       = "0f8fad5b-d9cb-469f-a165-70867728950e"
	identityFilterRateLimitID = "0123456789abcdef"
)

// authorinoCELEnv mirrors the environment Authorino compiles response
// expressions in (authorino pkg/expressions/cel Compile): every root binding is a
// google.protobuf.Struct. cel.OptionalTypes is left out because Authorino v0.22
// does not enable it, so an expression that compiles here compiles there.
func authorinoCELEnv(t *testing.T) *cel.Env {
	t.Helper()
	structType := cel.ObjectType("google.protobuf.Struct")
	env, err := cel.NewEnv(
		cel.Variable("metadata", structType),
		cel.Variable("request", structType),
		cel.Variable("source", structType),
		cel.Variable("destination", structType),
		cel.Variable("auth", structType),
		ext.Strings(),
	)
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	return env
}

// authorinoInput decodes each root binding from JSON into a
// google.protobuf.Struct, as Authorino's AuthJsonToCel does.
func authorinoInput(t *testing.T, roots map[string]any) map[string]any {
	t.Helper()
	input := make(map[string]any, len(roots))
	for name, value := range roots {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		decoded := &structpb.Struct{}
		if err := protojson.Unmarshal(raw, decoded); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		input[name] = decoded
	}
	return input
}

// evaluateAuthorinoExpression evaluates expr and converts the result as
// Authorino's Expression.ResolveFor does, through google.protobuf.Value and
// JSON. A result with no JSON form fails there, so it fails here too.
func evaluateAuthorinoExpression(t *testing.T, env *cel.Env, expr string, input map[string]any) (any, error) {
	t.Helper()
	ast, issues := env.Compile(expr)
	if issues.Err() != nil {
		t.Fatalf("compile %q: %v", expr, issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatalf("program %q: %v", expr, err)
	}
	value, _, err := program.Eval(input)
	if err != nil {
		return nil, err
	}
	native, err := value.ConvertToNative(reflect.TypeFor[*structpb.Value]())
	if err != nil {
		return nil, err
	}
	protoValue, ok := native.(*structpb.Value)
	if !ok {
		t.Fatalf("converted value = %T, want *structpb.Value", native)
	}
	raw, err := protojson.Marshal(protoValue)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return out, nil
}

// nestedSpecMap reads a nested map without copying, so it also works on specs
// that hold non-JSON Go types before they reach the API server.
func nestedSpecMap(t *testing.T, obj map[string]any, fields ...string) map[string]any {
	t.Helper()
	value, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if err != nil || !found {
		t.Fatalf("%s missing: found=%v err=%v", strings.Join(fields, "."), found, err)
	}
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %T, want map", strings.Join(fields, "."), value)
	}
	return m
}

// evaluateIdentityFilter evaluates the gateway AuthPolicy's filters.identity the
// way Authorino's response evaluators do: a plain expression once, or each json
// property in turn, failing the whole filter on the first error
// (authorino pkg/evaluators/response/dynamic_json.go).
func evaluateIdentityFilter(t *testing.T, spec map[string]any, roots map[string]any) (map[string]any, error) {
	t.Helper()
	filter := nestedSpecMap(t, spec, "defaults", "rules", "response", "success", "filters", "identity")
	env := authorinoCELEnv(t)
	input := authorinoInput(t, roots)

	if _, isPlain := filter["plain"]; isPlain {
		expr, ok := nestedSpecMap(t, filter, "plain")["expression"].(string)
		if !ok {
			t.Fatal("filters.identity.plain.expression missing")
		}
		value, err := evaluateAuthorinoExpression(t, env, expr, input)
		if err != nil {
			return nil, err
		}
		out, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("filters.identity = %#v, want a map", value)
		}
		return out, nil
	}

	properties := nestedSpecMap(t, filter, "json", "properties")
	out := make(map[string]any, len(properties))
	for name, raw := range properties {
		property, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("filters.identity property %q = %T, want map", name, raw)
		}
		expr, ok := property["expression"].(string)
		if !ok {
			t.Fatalf("filters.identity property %q has no expression", name)
		}
		value, err := evaluateAuthorinoExpression(t, env, expr, input)
		if err != nil {
			return nil, fmt.Errorf("property %s: %w", name, err)
		}
		out[name] = value
	}
	return out, nil
}

func identityFilterTestSpec() map[string]any {
	r := &MaaSAuthPolicyReconciler{
		InfraNamespace:   "maas-system",
		GatewayName:      "maas-default-gateway",
		GatewayNamespace: "openshift-ingress",
		MetadataCacheTTL: 60,
		AuthzCacheTTL:    60,
	}
	return r.buildGatewayAuthPolicySpec(nil, false, "", "models-as-a-service", "openshift-ingress", "maas-default-gateway", nil)
}

func identityFilterRoots(identity any, metadata map[string]any) map[string]any {
	return map[string]any{
		"auth": map[string]any{"identity": identity, "metadata": metadata},
		"request": map[string]any{
			"path":    identityFilterModelPath,
			"method":  "POST",
			"headers": map[string]any{"content-type": "application/json"},
		},
	}
}

func apiKeyValidationFixture(groups ...string) map[string]any {
	validation := map[string]any{
		"valid":        true,
		"userId":       "3c1e7d0a-0000-4000-8000-000000000001",
		"username":     "alice",
		"keyId":        "key-1",
		"keyName":      "ci",
		"subscription": "gold",
	}
	if len(groups) > 0 {
		validation["groups"] = groups
	}
	return validation
}

func subscriptionInfoFixture(fields map[string]any) map[string]any {
	info := map[string]any{
		"name":          "gold",
		"namespace":     "models-as-a-service",
		"resolvedModel": "llm/model-a",
		"phase":         "Active",
		"ready":         true,
		"accessAllowed": true,
	}
	for k, v := range fields {
		info[k] = v
	}
	return info
}

func tokenReviewIdentity(groups []string) map[string]any {
	asAny := make([]any, len(groups))
	for i, g := range groups {
		asAny[i] = g
	}
	return map[string]any{
		"authenticated": true,
		"user": map[string]any{
			"username": "system:serviceaccount:team:bot",
			"groups":   asAny,
		},
	}
}

func numberedGroups(prefix string, n int) []string {
	groups := make([]string, n)
	for i := range groups {
		groups[i] = fmt.Sprintf("%s-%02d", prefix, i)
	}
	return groups
}

func TestGatewayIdentityFilterOutput(t *testing.T) {
	resolved := subscriptionInfoFixture(map[string]any{
		"rateLimitId":    identityFilterRateLimitID,
		"organizationId": "org-1",
		"costCenter":     "cc-1",
		"labels":         map[string]any{"team": "a"},
		"modelRefs":      []any{map[string]any{"namespace": "llm", "name": "model-a"}},
		"headers":        map[string]any{"x-tenant": []any{"a"}},
	})
	legacy := subscriptionInfoFixture(nil)
	denied := map[string]any{
		"error":         "not_found",
		"message":       "no matching subscription",
		"accessAllowed": false,
		"phase":         "",
		"ready":         false,
	}
	tokenGroups40 := append([]string{"system:authenticated"}, numberedGroups("team", 39)...)

	const goldKey = "models-as-a-service/gold@llm/model-a"
	emptyInfo := map[string]any{"organizationId": "", "costCenter": ""}
	resolvedInfo := map[string]any{"organizationId": "org-1", "costCenter": "cc-1"}

	cases := []struct {
		name     string
		identity any
		metadata map[string]any
		want     map[string]any
	}{
		{
			name:     "api key, resolved subscription",
			identity: "Bearer sk-oai-test",
			metadata: map[string]any{
				"apiKeyValidation":  apiKeyValidationFixture("system:authenticated", "team-a"),
				"subscription-info": resolved,
			},
			want: map[string]any{
				"groups_str":                "system:authenticated,team-a",
				"userid":                    "alice",
				"selected_subscription":     "gold",
				"selected_subscription_key": goldKey,
				"selected_subscription_id":  identityFilterRateLimitID,
				"subscription_info":         resolvedInfo,
			},
		},
		{
			name:     "api key, legacy subscription without org, cost center or rateLimitId",
			identity: "Bearer sk-oai-test",
			metadata: map[string]any{
				"apiKeyValidation":  apiKeyValidationFixture("system:authenticated"),
				"subscription-info": legacy,
			},
			want: map[string]any{
				"groups_str":                "system:authenticated",
				"userid":                    "alice",
				"selected_subscription":     "gold",
				"selected_subscription_key": goldKey,
				"selected_subscription_id":  "",
				"subscription_info":         emptyInfo,
			},
		},
		{
			name:     "api key, subscription denied",
			identity: "Bearer sk-oai-test",
			metadata: map[string]any{
				"apiKeyValidation":  apiKeyValidationFixture("system:authenticated"),
				"subscription-info": denied,
			},
			want: map[string]any{
				"groups_str":                "system:authenticated",
				"userid":                    "alice",
				"selected_subscription":     "",
				"selected_subscription_key": "",
				"selected_subscription_id":  "",
				"subscription_info":         emptyInfo,
			},
		},
		{
			name:     "token review, 3 groups",
			identity: tokenReviewIdentity([]string{"system:authenticated", "system:serviceaccounts", "team-a"}),
			metadata: map[string]any{"subscription-info": resolved},
			want: map[string]any{
				"groups_str":                "system:authenticated,system:serviceaccounts,team-a",
				"userid":                    "system:serviceaccount:team:bot",
				"selected_subscription":     "gold",
				"selected_subscription_key": goldKey,
				"selected_subscription_id":  identityFilterRateLimitID,
				"subscription_info":         resolvedInfo,
			},
		},
		{
			name:     "token review, 40 groups",
			identity: tokenReviewIdentity(tokenGroups40),
			metadata: map[string]any{"subscription-info": legacy},
			want: map[string]any{
				"groups_str":                strings.Join(tokenGroups40, ","),
				"userid":                    "system:serviceaccount:team:bot",
				"selected_subscription":     "gold",
				"selected_subscription_key": goldKey,
				"selected_subscription_id":  "",
				"subscription_info":         emptyInfo,
			},
		},
		{
			name: "oidc with organization",
			identity: map[string]any{
				"iss":                "https://issuer.example.com/realms/test",
				"sub":                "subject-1",
				"preferred_username": "bob",
				"org_id":             identityFilterOrgID,
				"groups":             []any{"team-a", "team-b"},
			},
			metadata: map[string]any{"subscription-info": resolved},
			want: map[string]any{
				"groups_str":                identityFilterOrgID + "/team-a," + identityFilterOrgID + "/team-b",
				"userid":                    "bob",
				"selected_subscription":     "gold",
				"selected_subscription_key": goldKey,
				"selected_subscription_id":  identityFilterRateLimitID,
				"subscription_info":         resolvedInfo,
			},
		},
		{
			name: "legacy oidc without preferred_username",
			identity: map[string]any{
				"iss":    "https://issuer.example.com/realms/test",
				"sub":    "subject-2",
				"groups": []any{"team-b"},
			},
			metadata: map[string]any{"subscription-info": legacy},
			want: map[string]any{
				"groups_str":                "team-b",
				"userid":                    "subject-2",
				"selected_subscription":     "gold",
				"selected_subscription_key": goldKey,
				"selected_subscription_id":  "",
				"subscription_info":         emptyInfo,
			},
		},
	}

	spec := identityFilterTestSpec()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluateIdentityFilter(t, spec, identityFilterRoots(tc.identity, tc.metadata))
			if err != nil {
				t.Fatalf("filters.identity failed: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				wantJSON, _ := json.MarshalIndent(tc.want, "", "  ")
				t.Errorf("filters.identity output:\n%s\nwant:\n%s", gotJSON, wantJSON)
			}
		})
	}
}

var identityPathPattern = regexp.MustCompile(`auth\.identity\.([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)`)

func collectIdentityPaths(text string, into map[string][]string, source string) {
	for _, m := range identityPathPattern.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(into[m[1]], source) {
			into[m[1]] = append(into[m[1]], source)
		}
	}
}

func renderedTelemetryLabels(t *testing.T, telemetry *maasv1alpha1.TenantTelemetryConfig) map[string]any {
	t.Helper()
	tenant := &maasv1alpha1.MaasTenantConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      maasv1alpha1.MaasTenantConfigInstanceName,
			Namespace: "models-as-a-service",
		},
		Spec: maasv1alpha1.MaasTenantConfigSpec{Telemetry: telemetry},
	}
	resources, err := tenantreconcile.PostRender(t.Context(), logr.Discard(), tenant, nil, tenantreconcile.PlatformParams{
		GatewayNamespace: "openshift-ingress",
		GatewayName:      "maas-default-gateway",
		SkipIPP:          true,
	})
	if err != nil {
		t.Fatalf("PostRender: %v", err)
	}
	for i := range resources {
		if resources[i].GroupVersionKind() != tenantreconcile.GVKTelemetryPolicy {
			continue
		}
		return nestedSpecMap(t, resources[i].Object, "spec", "metrics", "default", "labels")
	}
	t.Fatal("PostRender produced no TelemetryPolicy")
	return nil
}

// identityConsumerPaths returns every auth.identity.<path> that a MaaS-managed
// consumer reads in wasm-shim, mapped to where it is read.
func identityConsumerPaths(t *testing.T) map[string][]string {
	t.Helper()
	paths := map[string][]string{}

	allOn := &maasv1alpha1.TenantTelemetryConfig{Metrics: &maasv1alpha1.TenantMetricsConfig{
		CaptureOrganization: ptr.To(true),
		CaptureUser:         ptr.To(true),
		CaptureGroup:        ptr.To(true),
		CaptureModelUsage:   ptr.To(true),
	}}
	for name, telemetry := range map[string]*maasv1alpha1.TenantTelemetryConfig{"defaults": nil, "all capture flags": allOn} {
		for label, expr := range renderedTelemetryLabels(t, telemetry) {
			s, ok := expr.(string)
			if !ok {
				t.Fatalf("TelemetryPolicy label %q = %T, want string", label, expr)
			}
			collectIdentityPaths(s, paths, "TelemetryPolicy ("+name+") label "+label)
		}
	}

	for _, file := range []string{
		filepath.Join("..", "..", "..", "..", "deployment", "base", "observability", "gateway-telemetry-policy.yaml"),
		filepath.Join("..", "..", "..", "..", "deployment", "base", "maas-controller", "policies", "gateway-default-deny.yaml"),
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		collectIdentityPaths(string(data), paths, filepath.Base(file))
	}

	for _, legacy := range []bool{false, true} {
		si := subInfo{subNamespace: "models-as-a-service", subName: "gold", modelScoped: "models-as-a-service/gold@llm/model-a", legacy: legacy}
		source := fmt.Sprintf("TRLP (legacy=%t)", legacy)
		collectIdentityPaths(subscriptionSelectClause(si), paths, source+" predicate")
		collectIdentityPaths(subscriptionCounterExpression(legacy), paths, source+" counter")
		limit, err := json.Marshal(buildGroupLimit([]subInfo{si}))
		if err != nil {
			t.Fatalf("marshal TRLP limit: %v", err)
		}
		collectIdentityPaths(string(limit), paths, source+" limit")
	}
	return paths
}

func lookupIdentityPath(out map[string]any, path string) (any, bool) {
	var current any = out
	for segment := range strings.SplitSeq(path, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// TestGatewayIdentityFilterServesEveryConsumer fails when a TelemetryPolicy
// label, TRLP predicate or counter reads an auth.identity path the filter does
// not emit as a string. wasm-shim drops empty maps and rejects non-scalar label
// values, so the path has to end in a string, not just exist.
func TestGatewayIdentityFilterServesEveryConsumer(t *testing.T) {
	paths := identityConsumerPaths(t)
	for _, want := range []string{"userid", "groups_str", "selected_subscription", "selected_subscription_id",
		"selected_subscription_key", "subscription_info.organizationId", "subscription_info.costCenter"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("no consumer reads auth.identity.%s; the collector is missing a source", want)
		}
	}

	spec := identityFilterTestSpec()
	shapes := map[string]map[string]any{
		"api key, legacy subscription": identityFilterRoots("Bearer sk-oai-test", map[string]any{
			"apiKeyValidation":  apiKeyValidationFixture("system:authenticated"),
			"subscription-info": subscriptionInfoFixture(nil),
		}),
		"token review, resolved subscription": identityFilterRoots(
			tokenReviewIdentity([]string{"system:authenticated"}),
			map[string]any{"subscription-info": subscriptionInfoFixture(map[string]any{
				"rateLimitId": identityFilterRateLimitID, "organizationId": "org-1", "costCenter": "cc-1",
			})},
		),
	}

	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)

	for shape, roots := range shapes {
		t.Run(shape, func(t *testing.T) {
			out, err := evaluateIdentityFilter(t, spec, roots)
			if err != nil {
				t.Fatalf("filters.identity failed: %v", err)
			}
			for _, path := range names {
				value, ok := lookupIdentityPath(out, path)
				if !ok {
					t.Errorf("auth.identity.%s is not emitted; read by %v", path, paths[path])
					continue
				}
				if _, isString := value.(string); !isString {
					t.Errorf("auth.identity.%s = %#v, want a string; read by %v", path, value, paths[path])
				}
			}
		})
	}
}
