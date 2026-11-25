# Authorino Caching Configuration

This document describes how to tune the Authorino cache TTL for metadata evaluators in MaaS.

---

## What is Cached

The gateway AuthPolicy (`maas-gateway-auth`) caches the metadata evaluators that call maas-api:

- `apiKeyValidation` - validates API keys and returns user identity + groups (`/internal/v1/api-keys/validate`)
- `subscription-info` - selects the appropriate subscription for the request (`/internal/v1/subscriptions/select`)

Authorization evaluators (`auth-valid`, `subscription-valid`, `require-group-membership`) are not cached. They recompute from the cached metadata on every request; caching their results could retain an allow decision after newer metadata contains a denial.

Caching reduces load on maas-api by reusing results when the cache key repeats within the TTL window. The `apiKeyValidation` key is the API key itself. The `subscription-info` key covers every input of the subscription select request, so requests only share a cached result when they send maas-api the same request:

- **API keys:** the key ID, the key's username and bound subscription, and the model. A key's groups are fixed when it is created, so they are left out, and the `X-MaaS-Subscription` header does not change the cached result.
- **OpenShift and OIDC tokens** (and an API key whose validation returned no key ID): the identity, groups, requested subscription, the `X-MaaS-Subscription` header, and the model.

MaaS wraps each cache key in a one-element list, which makes Authorino store a 32-byte hash of the key instead of the key itself, so raw API keys are not kept in the cache. Each Authorino pod has its own cache, so the first request with a given key on each pod still calls maas-api.

An entry larger than Authorino's per-entry limit is not cached; see [Cache Entry Size](#cache-entry-size).

---

## Configuration

### CLI Flags

The maas-controller deployment configures the cache TTL with a command-line flag:

- `--metadata-cache-ttl`: TTL for metadata HTTP caching in seconds. Default: `60`. Must be `>= 0`; `0` disables caching.
- `--authz-cache-ttl`: Deprecated and ignored. It remains accepted so existing deployments continue to start.

### Deployment Configuration

The steps below apply to kustomize deployments, such as `./scripts/deploy.sh --deployment-mode kustomize`. When an operator manages the maas-controller Deployment, as on RHOAI or with `./scripts/deploy.sh` in its default operator mode, it reverts changes to the Deployment, so a patch does not persist and the controller keeps the TTL from the operator's manifests (`60` by default).

#### Via manager.yaml (base deployment)

Edit `deployment/base/maas-controller/manager/manager.yaml` to change hardcoded values, then deploy again:

```yaml
args:
  # ... other args ...
  - --metadata-cache-ttl=300  # 5 minutes
```

#### Via controller Deployment args (running cluster)

To change the TTL on a running cluster, replace the flag in the Deployment args. The new value lasts until the next deploy applies `manager.yaml` again:

```bash
CONTROLLER_NS=opendatahub
ARGS=$(kubectl get deployment maas-controller -n "$CONTROLLER_NS" -o json |
  jq -c '.spec.template.spec.containers[0].args | map(select(startswith("--metadata-cache-ttl=") | not)) + ["--metadata-cache-ttl=300"]')

kubectl patch deployment maas-controller -n "$CONTROLLER_NS" --type='json' \
  -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":${ARGS}}]"
```

The patch rolls out a new controller pod, which updates `maas-gateway-auth`. To check the TTL the gateway uses (empty output means caching is disabled):

```bash
kubectl get authpolicy maas-gateway-auth -n openshift-ingress \
  -o jsonpath='{.spec.defaults.rules.metadata.apiKeyValidation.cache.ttl}'
```

> **Note on Customization:** Direct modification of Authorino deployments or settings may interact with operator reconciliation during upgrades. Test changes in non-production environments and document any customizations for support handoff.

---

## Operational Impact

### Stale Data Window

Cache TTL represents the maximum staleness window for access control changes:

- **API key revocation:** May take up to the configured metadata cache TTL to propagate
- **API key expiry:** Not delayed; maas-api returns `expiresAt` with validation, and the gateway checks it on every request
- **Group membership changes:** API keys keep the groups they were created with, so a change needs a new key. For OpenShift and OIDC tokens the groups are part of the `subscription-info` cache key, so a request with different groups never gets a cached selection
- **Subscription changes:** May take up to the metadata cache TTL to propagate, because a cached selection is reused until it expires. This covers a deleted or changed subscription, a newly granted subscription, and a subscription that turns `Active`
- **MaaSAuthPolicy changes:** Not delayed by the cache. The model access rules are part of `maas-gateway-auth` and are evaluated on every request once the controller updates it

#### Security implication: revocation lag

> **Known accepted trade-off (ASVS V3.3.1 / CWE-613):** A revoked API key can continue to produce successful inference responses for up to `--metadata-cache-ttl` seconds (default: **60 seconds**) after the revocation is recorded in maas-api. This is a deliberate trade-off - reducing the cache TTL was evaluated and rejected for performance reasons.
>
> The `apiKeyValidation` cache entry is keyed on the individual API key (functionally equivalent to keying on JTI), so cache hits are always per-key. Authorino does not expose an active cache invalidation API, so there is no mechanism to evict a specific key's cache entry without the workarounds below.

For **immediate enforcement** after a sensitive revocation (e.g., a compromised key):

1. **Restart Authorino pods** - forces a full cache flush across all tenants. Use only during maintenance windows.
   ```bash
   kubectl rollout restart deployment/authorino -n <authorino-namespace>
   ```
2. **Wait for TTL to expire** - acceptable for non-urgent revocations; the default window is 60 seconds.

### Cached Errors and Denials

Authorino caches the body maas-api returns and ignores the HTTP status code, so denials and maas-api errors are cached for the TTL as well. Only a call that fails outright, for example because maas-api is unreachable or times out, is not cached.

- **Key validation errors:** When maas-api cannot complete a validation, for example while PostgreSQL is unavailable, `/internal/v1/api-keys/validate` returns HTTP 500 with `{"error":"validation failed"}`. Requests with that key are denied until the entry expires, even after maas-api recovers.
- **Subscription selection denials:** `/internal/v1/subscriptions/select` returns its errors (`not_found`, `access_denied`, `multiple_subscriptions`, `model_not_in_subscription`, `model_unhealthy`, `internal_error`) with HTTP 200, and they are cached like a successful selection. So is a selected subscription whose phase is not `Active` or `Degraded`. A request denied because the subscription was missing, not yet `Active`, or did not include the model stays denied for up to the TTL after the cause is fixed.

To clear a cached denial before it expires, use the steps under [Security implication: revocation lag](#security-implication-revocation-lag).

### Cache Entry Size

Authorino gives every cached evaluator its own cache of `evaluatorCacheSize` megabytes (default `1`), split into 256 segments. It does not store an entry (cache key plus cached response) larger than a quarter of a segment minus 24 bytes. An entry that does not fit is skipped without an error: the request still succeeds, but maas-api is called on every request.

Cache keys are stored as a 32-byte hash (see [What is Cached](#what-is-cached)), so what counts against the limit is the cached response.

| `evaluatorCacheSize` | Largest cached entry (key + response) | Room for the response |
|---|---|---|
| 1 (default) | 1000 bytes | 968 bytes |
| 2 | 2024 bytes | 1992 bytes |
| 4 | 4072 bytes | 4040 bytes |

maas-api leaves display metadata, priority, labels and the model list out of the select response, so a long description or many models do not grow an entry. What grows one is the groups `apiKeyValidation` returns: the key's groups, listed once, each costing its name length plus 3 bytes.

| Lookup | Where groups count | Cost per group | Fits at size 1 (default) | At size 2 | At size 4 |
|---|---|---|---|---|---|
| `subscription-info`, any identity | nowhere: the key is stored as a hash, and the response carries no groups | none | any number | any number | any number |
| `apiKeyValidation`, API keys | the response, which lists the key's groups once | name length plus 3 bytes | about 17 groups | about 45 groups | about 100 groups |

The rest of a validation response takes about 290 bytes, which leaves about 670 bytes for the group list at size 1, 1700 at size 2 and 3750 at size 4. The group counts assume 33-character group names, 20-character username, key name and subscription name, the default tenant name, and the system groups `system:authenticated` and `system:authenticated:oauth` stored with the key. They are computed from the response size, not measured, so treat them as rough. The groups that count are the ones the user had when the key was created.

To see how many groups an OpenShift user has and how long their names are:

```bash
oc get groups -o json | jq -r --arg u alice \
  '[.items[] | select(.users // [] | index($u)) | .metadata.name] | "\(length) groups, \(join("") | length) characters"'
```

A key fits at the default size while those characters plus 3 bytes per group stay under about 620 bytes. The rest of the 670 bytes goes to the system groups, which this command does not list.

To raise the limit, patch the Authorino CR in the Kuadrant namespace (`kuadrant-system` by default). Kuadrant does not manage this field, so the change persists:

```bash
oc patch authorino authorino -n kuadrant-system --type merge -p '{"spec":{"evaluatorCacheSize":4}}'
```

The operator restarts the Authorino pods, so caches start empty. Each cached evaluator of every AuthConfig gets its own cache, and MaaS caches two evaluators per AuthConfig. Kuadrant creates one AuthConfig per HTTPRoute rule behind the gateway, so memory per Authorino pod can grow up to the number of AuthConfigs times 2 times `evaluatorCacheSize` MB. Each cache only fills with entries for requests on its own route.

### When to Tune

**Increase metadata cache TTL** if:
- maas-api experiences high load from repeated validation calls
- API key metadata changes infrequently
- Example: `--metadata-cache-ttl=300` (5 minutes) reduces maas-api load by 5x

**Decrease metadata cache TTL** if:
- Faster API key revocation or subscription-change propagation is required (e.g., compliance or security policy requires a shorter staleness window)
- Cached denials after a maas-api outage or a subscription fix last too long
- Note: this increases load on maas-api's `/internal/v1/api-keys/validate` endpoint proportionally; monitor request rates before and after

**Increase `evaluatorCacheSize`** if:
- API keys of users in many groups call `/internal/v1/api-keys/validate` on every request even with caching enabled (see [Cache Entry Size](#cache-entry-size))

**Monitor after changes:**
- maas-api load: Reduced `/internal/v1/api-keys/validate` and `/internal/v1/subscriptions/select` call rates. Compare `maas_api_http_requests_total` for those routes with the gateway request rate; a ratio near 1 means lookups are not being cached
- Authorino CPU: Reduced HTTP evaluator work
- Request latency: Cache hits should show lower P99 latency

If `/internal/v1/subscriptions/select` or `/internal/v1/api-keys/validate` calls stay close to the request rate after enabling caching, check [Cache Entry Size](#cache-entry-size): an entry that exceeds Authorino's per-entry limit is not stored.

---

## References

- [AuthPolicy Reference](https://docs.kuadrant.io/latest/kuadrant-operator/doc/reference/authpolicy/)
- [Controller Architecture](../architecture-internals/controller-architecture.md)
