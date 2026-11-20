# Authentication & gateway identity (internals)

This page describes how **identity and subscription context** are established at the gateway and how **rate limiting** uses them.

---

## Subscription selection (AuthPolicy) → rate limits (TokenRateLimitPolicy)

Today’s pipeline does **not** drive TokenRateLimitPolicy by splitting group membership in CEL the way older drafts did. The flow is:

1. **AuthPolicy** (controller-generated per model) authenticates the caller and calls **maas-api** at:
   - Default tenant: `POST https://maas-api.opendatahub.svc.cluster.local:8443/internal/v1/subscriptions/select`
   - Additional tenants: `POST https://maas-api-{tenantID}.opendatahub.svc.cluster.local:8443/internal/v1/subscriptions/select`
   
   with the caller’s groups, username, requested subscription header (when applicable), and model scope.

2. **maas-api** resolves which **MaaSSubscription** applies (including validation, auto-selection when only one subscription applies, and error paths). Results are exposed to Authorino as **`subscription-info`** metadata. When a guardrail resolver is configured, a successful model selection also includes **`selectedGuardrailChecks`**, a sparse map whose keys are the shared SHA-256 check IDs and whose values are `true`. The map is `{}` when no guardrails apply; if a selected attachment cannot be resolved, the endpoint returns `guardrail_unavailable` and does not return a partial selection. The field carries selection state only; request headers are handled separately.

3. **AuthPolicy `filters.identity`** copies the fields that rate limits and telemetry read onto **`auth.identity`** (all of them are listed under [Identity metadata](#identity-metadata)), including:
   - **`selected_subscription_key`** — model-scoped key of the form  
     `{subscriptionNamespace}/{subscriptionName}@{modelNamespace}/{modelName}`  
     Kept for telemetry and debugging, and still the **rate-limit identity** for subscriptions that existed before the short-ID rollout.
   - **`selected_subscription_id`** — first 16 hex chars of SHA-256 over that key (from
     maas-api `subscription-info.rateLimitId`). Set only when the selected MaaSSubscription
     has `status.rateLimitIdentity=short` (created after upgrade).
     Also mirrored as response header **`X-MaaS-Subscription-Rate-Limit-Id`** without replacing
     existing headers.

4. **TokenRateLimitPolicy** (aggregated per model by the MaaSSubscription reconciler) defines **one limit entry per distinct rate set** among the subscriptions that apply to that model, split further when that set mixes legacy and short-ID members (they cannot share counters). Each limit’s **`when`** predicate matches:
   - **new subscriptions:** `auth.identity.selected_subscription_id`
   - **pre-upgrade subscriptions:** `auth.identity.selected_subscription_key`
   
   Inference paths are distinguished from discovery; `/v1/models` is exempt from token consumption limits where configured. Counters use the same identity field as the predicate, plus `userid`. See [Reconciliation Flow](./reconciliation-flow.md#maassubscription-reconciler).

So enforcement is: **subscription resolved in AuthPolicy → the same identity the TRLP was built for**. Group-based **authorization** is resolved during subscription selection: **maas-api** evaluates **MaaSAuthPolicy** rules for the caller and returns `accessAllowed` in **`subscription-info`** metadata. The gateway authorization rule simply reads that boolean rather than embedding per-model allowlists, keeping the AuthPolicy CR fixed-size at any subscription count.

**TRLP predicates vs other identity:** TokenRateLimitPolicy **`when`** clauses use **`selected_subscription_id` for new subscriptions and `selected_subscription_key` for legacy ones**—not `groups_str`, group arrays, or header mirrors. Anything else on `auth.identity` is **not** part of TRLP matching; it exists for **subscription selection** (inputs to maas-api), **Authorino cache/metadata**, and **telemetry** at the gateway/mesh.

---

## Identity metadata

`filters.identity` in the gateway AuthPolicy (`maasauthpolicy_controller.go`) is the only source of **`auth.identity`** in wasm-shim, where TokenRateLimitPolicy predicates and counters and TelemetryPolicy labels read it. It carries only the fields one of those reads, each always present as a string. A label or predicate on a path the filter doesn't emit either reads `null` or drops the request's whole Limitador descriptor (kuadrant-operator#2242), depending on the wasm-shim build, so a policy you write yourself should read only these fields.

| Field | Value | Read by |
|---|---|---|
| `userid` | API key: `apiKeyValidation.username`. OIDC: `preferred_username`, else `sub`. TokenReview: `user.username` | TRLP counters, `user` label |
| `groups_str` | comma-joined groups, see below | `group` label (`captureGroup`) |
| `selected_subscription` | selected subscription name, `""` when none | `subscription` label |
| `selected_subscription_key` | `{subscriptionNamespace}/{subscriptionName}@{modelNamespace}/{modelName}`, `""` when none | TRLP for pre-upgrade subscriptions |
| `selected_subscription_id` | `subscription-info.rateLimitId`, `""` for pre-upgrade subscriptions | TRLP for new subscriptions |
| `subscription_info.organizationId`, `subscription_info.costCenter` | from the selected MaaSSubscription, `""` when unset | `organization_id` and `cost_center` labels |

The filter runs only when the request names a model, in the path or through `X-Gateway-Model-Name`, the same condition as the `subscription-info` call.

### Groups

`groups_str` uses the same **`celGroups`** expression as subscription selection and, for token identities, the `subscription-info` cache key:

- API key: `auth.metadata.apiKeyValidation.groups`, the snapshot taken when the key was created.
- Kubernetes TokenReview (OpenShift bearer token): `auth.identity.user.groups`, from TokenReview `status.user.groups`.
- OIDC: the `groups` claim. When the token carries a valid `org_id`, each group is prefixed with `<org_id>/`.

If any token group fails the group-name allowlist, the list is empty. For an OIDC token with an `org_id`, it is also empty when `org_id` isn't a canonical UUID or the token has more than 128 groups. TRLP predicates never match on groups. Rate limits use `selected_subscription_id` for subscriptions created after the short-ID rollout and `selected_subscription_key` for pre-upgrade ones.

### Troubleshooting: which field to inspect

| Concern | API key flow | TokenReview (kube) token | OIDC |
|--------|----------------|---------------------------|------|
| Raw groups after auth | `auth.metadata.apiKeyValidation.groups` | `auth.identity.user.groups` | `auth.identity.groups` |
| Groups after normalization (`celGroups`, `groups_str`) | the raw groups | the raw groups, or none if one fails the allowlist | the raw groups, prefixed with `org_id/` when present, or none if one fails the allowlist |
| Subscription selection and Authorino cache inputs | `celGroups`, `celUsername`, `celSubscription`, then `auth.metadata["subscription-info"]`. The cache key is the key ID, username, bound subscription and model, without groups | Same inputs. The cache key includes the groups | Same as TokenReview |
| Rate limiting | `auth.identity.selected_subscription_id` (new CRs) or `selected_subscription_key` (pre-upgrade CRs) | Same | Same |
| Telemetry / debug subscription@model | `auth.identity.selected_subscription_key` | Same | Same |

If you read older notes about a “string trick” solely for TRLP group matching, treat that as **obsolete**.

---

## Identity headers and defense-in-depth

**Model inference routes** (HTTPRoutes to model workloads):

- Controller-generated AuthPolicies generally **do not** inject most identity headers (`X-MaaS-Username`, `X-MaaS-Group`, `X-MaaS-Key-Id`) upstream to model pods, to reduce leakage via logs or misconfigured proxies.

For **API keys**, AuthPolicy injects **`X-MaaS-Subscription`** only when validation returns a nonempty bound subscription. For **user tokens**, AuthPolicy preserves a client-supplied subscription header and uses it for subscription selection; it does not inject another copy. A user-token request without that header does not gain one during authentication, so telemetry that reads the header may have an empty subscription label.

AuthPolicy always sets organization scope and the llm-d flow-control headers, replacing any client-supplied values. Organization comes from the subscription selected by maas-api; an empty value identifies unresolved scope rather than trusting client input:

- **`x-maas-organization-id`**: the stable `MaaSSubscription.spec.organization.id`, with legacy `tokenMetadata.organizationId` as the fallback.

The llm-d flow-control headers are:

- **`x-llm-d-inference-objective`** and **`x-gateway-inference-objective`**: the `objective` returned by maas-api subscription selection, which is the requested model's `objectiveName` from the subscription's [`flowControlStatuses`](../reference/crds/maas-subscription.md#status-flowcontrolstatuses). Empty when the model has none (for example, an ExternalModel) or when another object holds the name, so the scheduler applies priority `0`.
- **`x-llm-d-inference-fairness-id`** and **`x-gateway-inference-fairness-id`**: the `fairnessId` returned by maas-api, which is the tenant name of the maas-api instance.

**MaaS API routes** use a separate static AuthPolicy that may inject headers required by maas-api middleware (trusted internal service).

---

## Token validation (short)

**OpenShift tokens:** Authorino uses Kubernetes **TokenReview**; groups and username come from the review result.

**API keys:** Authorino calls MaaS API validation; the key carries a bound subscription; validation returns user fields used for identity and subscription resolution.

**Groups for authorization:** Values in **MaaSAuthPolicy** / **MaaSSubscription** must align with groups from TokenReview or API key validation, not only with OpenShift `Group` objects, unless your IdP maps them consistently.

```bash
TOKEN=$(kubectl create token default -n default --duration=1m)
echo '{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":"'$TOKEN'"}}' | \
  kubectl create -o jsonpath='{.status.user.groups}' -f -
```

---

## Related documentation

- [Controller Architecture](./controller-architecture.md)
- [Reconciliation Flow](./reconciliation-flow.md)
- [Access and Quota Overview](../concepts/subscription-overview.md)
- [MaaS Setup](../install/maas-setup.md)
