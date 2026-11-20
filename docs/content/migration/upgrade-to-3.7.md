# Upgrade to 3.7

## Gateway identity fields

The gateway AuthPolicy exports the caller's identity to Kuadrant's wasm-shim as `auth.identity`, where TokenRateLimitPolicy predicates and counters and TelemetryPolicy labels read it. In 3.7 it carries only the fields MaaS-managed policies read:

| Field | Change |
|---|---|
| `userid`, `groups_str`, `selected_subscription`, `selected_subscription_key`, `selected_subscription_id` | Unchanged |
| `subscription_info` | Only `organizationId` and `costCenter`, `""` when the subscription sets neither. Previously the whole subscription-info response, without those keys when unset |
| `groups`, `keyId`, `keyName`, `subscription_error`, `subscription_error_message` | Removed |

Limitador counters key on unchanged fields, so quotas carry over.

A TelemetryPolicy label or TokenRateLimitPolicy you wrote yourself that reads a removed field stops resolving, for example `auth.identity.groups` (Kuadrant's TelemetryPolicy examples use it) or `auth.identity.subscription_info.labels.<key>`. Depending on the wasm-shim build the label reads `null`, or the request's whole Limitador descriptor is dropped, which loses its metrics and can return 500 when an AuthPolicy is on the route ([Kuadrant/kuadrant-operator#2242](https://github.com/Kuadrant/kuadrant-operator/issues/2242)). Use `auth.identity.groups_str` for groups.

Before upgrading, list the fields the gateway's wasm config reads:

```bash
kubectl get envoyfilter,wasmplugin -n <gateway-namespace> -o yaml | grep -oE 'auth\.identity\.[A-Za-z_.]+' | sort -u
```

Kuadrant keeps a label removed from a TelemetryPolicy in the wasm config until the policy is deleted or kuadrant-operator restarts ([Kuadrant/kuadrant-operator#2243](https://github.com/Kuadrant/kuadrant-operator/issues/2243)). After removing such a label, delete and recreate the TelemetryPolicy, or restart kuadrant-operator.

Subscriptions without an organization or cost center now report `organization_id` and `cost_center` as `""`, so a query such as `organization_id!=""` leaves them out.
