"""What TokenRateLimitPolicy and TelemetryPolicy read from the gateway identity.

The gateway AuthPolicy exports the caller's identity to wasm-shim as
auth.identity (filters.identity). TRLP predicates and counters and
TelemetryPolicy labels read it there, and both fail silently when a field stops
resolving: a predicate that can't match serves the request with no token limit,
and a label that can't resolve reads null or drops the request's Limitador
series, depending on the wasm-shim build. These tests
prove both through requests and the series they produce, never through the
generated Kuadrant objects.
"""

import json
import logging
import subprocess
import time
import uuid
from dataclasses import dataclass, field

import pytest
from multitenancy_helpers import (
    TENANT_CONFIG_KIND,
    TENANT_CR_NAME,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
)
from test_helper import (
    MODEL_NAMESPACE,
    RATE_LIMIT_ENFORCEMENT_TIMEOUT,
    UNCONFIGURED_MODEL_PATH,
    UNCONFIGURED_MODEL_REF,
    _create_api_key,
    _create_api_key_raw,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _delete_sa,
    _get_cluster_token,
    _get_cr,
    _inference,
    _is_transient_gateway_response,
    _limitador_authorized_hits_series,
    _ns,
    _poll_status,
    _revoke_api_key,
    _sa_to_user,
    _wait_for_cr_absent,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_subscription_inference_ready,
    exercise_until_token_limited,
)
from worker_tenant_fixtures import (
    activate_worker_tenant,
    bootstrap_worker_tenant,
    build_worker_tenant_case,
    teardown_worker_tenant,
)

log = logging.getLogger(__name__)

pytestmark = [
    pytest.mark.serial,
    pytest.mark.limitador,
    # Shares the unconfigured model's AuthPolicy, subscriptions and TRLP with
    # test_subscription and test_trlp_rate_grouping.
    pytest.mark.xdist_group("api_unconfigured_model"),
]

MODEL_PATH = UNCONFIGURED_MODEL_PATH
SA_TOKEN_DURATION = "20m"

# Tokens per response vary, so send enough requests to cross a small budget.
# The window outlasts the test, and fresh subscription names mean counters
# never carry over between runs.
LOW_TOKEN_LIMIT = 10
LOW_LIMIT_WINDOW = "10m"
EXHAUST_MAX_REQUESTS = 15

# Limitador exports a series once the report after a response lands, and the
# TelemetryPolicy change from a captureGroup patch has to reach the gateway.
SERIES_TIMEOUT = 180


def _suffix():
    return uuid.uuid4().hex[:8]


def _inference_min(credential, path=None, extra_headers=None, model_name=None):
    return _inference(
        credential,
        path=path,
        extra_headers=extra_headers,
        model_name=model_name,
        max_tokens=1,
    )


@dataclass
class _Created:
    crs: list = field(default_factory=list)  # (kind, name)
    sas: list = field(default_factory=list)  # service account names
    keys: list = field(default_factory=list)  # (oc_token, key_id)


@pytest.fixture
def cleanup():
    """Collect API keys, CRs and service accounts; remove them best-effort."""
    created = _Created()
    yield created
    ns = _ns()
    try:
        for oc_token, key_id in created.keys:
            _revoke_api_key(oc_token, key_id)
        for kind, name in reversed(created.crs):
            _delete_cr(kind, name, namespace=ns)
        for kind, name in created.crs:
            if kind == "maassubscription":
                try:
                    _wait_for_cr_absent(kind, name, namespace=ns)
                except TimeoutError as e:
                    log.warning("maassubscription %s/%s not removed: %s", ns, name, e)
    finally:
        for sa in created.sas:
            _delete_sa(sa, namespace=ns)


def _caller(cleanup, base, token_limit, window, s):
    """A service account with access to the unconfigured model through its own subscription.

    Waits on MaaS status only: the MaaSAuthPolicy turns Active once the gateway
    AuthPolicy it wrote is accepted and enforced, and the subscription reports
    its TRLP accepted. The requests in each test prove the rest.
    """
    sa = f"{base}-{s}"
    cleanup.sas.append(sa)
    token = _create_sa_token(sa, namespace=_ns(), duration=SA_TOKEN_DURATION)
    user = _sa_to_user(sa, namespace=_ns())

    policy = f"e2e-idf-auth-{s}"
    cleanup.crs.append(("maasauthpolicy", policy))
    _create_test_auth_policy(
        name=policy, model_refs=[UNCONFIGURED_MODEL_REF], users=[user]
    )
    _wait_for_maas_auth_policy_phase(policy, require_enforced=False)

    subscription = f"e2e-idf-sub-{s}"
    cleanup.crs.append(("maassubscription", subscription))
    _create_test_subscription(
        name=subscription,
        model_refs=[UNCONFIGURED_MODEL_REF],
        users=[user],
        token_limit=token_limit,
        window=window,
    )
    _wait_for_maas_subscription_phase(subscription)
    _wait_for_subscription_inference_ready(
        subscription,
        UNCONFIGURED_MODEL_REF,
        model_namespace=MODEL_NAMESPACE,
        require_enforced_trlp=False,
    )
    return token, subscription


def _mint_key(cleanup, oc_token, name, subscription):
    r = _create_api_key_raw(oc_token, name=name, subscription=subscription)
    assert r.status_code in (200, 201), (
        f"minting API key {name} failed: {r.status_code} {r.text[:200]}"
    )
    body = r.json()
    if body.get("id"):
        cleanup.keys.append((oc_token, body["id"]))
    assert body.get("key"), f"API key response for {name} has no 'key' field"
    return body["key"]


def _require_telemetry(tenant_namespace):
    """Skip only when telemetry can't be observed for the tenant.

    That means no TelemetryPolicy API, telemetry turned off in the tenant's
    MaasTenantConfig, or Limitador unreachable three times in a row. Anything
    else, including series without the expected labels, must fail the test:
    a label that stops resolving looks exactly like that.
    """
    crd = subprocess.run(
        ["oc", "get", "crd", "telemetrypolicies.extensions.kuadrant.io"],
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    if crd.returncode != 0:
        pytest.skip("TelemetryPolicy CRD is not installed")
    config = _get_cr(TENANT_CONFIG_KIND, TENANT_CR_NAME, tenant_namespace)
    if config is None:
        pytest.skip(
            f"{TENANT_CONFIG_KIND} {tenant_namespace}/{TENANT_CR_NAME} not found"
        )
    if ((config.get("spec") or {}).get("telemetry") or {}).get("enabled") is False:
        pytest.skip(f"telemetry is disabled for tenant namespace {tenant_namespace}")
    unavailable = None
    for attempt in range(3):
        _, unavailable = _limitador_authorized_hits_series()
        if not unavailable:
            return
        time.sleep(5 * (attempt + 1))
    pytest.skip(unavailable)


def _wait_for_series(send, match, accept, what):
    """Send requests until Limitador exports a series carrying the match labels that accept() takes.

    Gateway propagation blips and scrape errors are retried until the deadline:
    the captureGroup patch rewrites the TelemetryPolicy just before.
    """
    deadline = time.monotonic() + SERIES_TIMEOUT
    observed, last = [], "no request sent"
    while time.monotonic() < deadline:
        r = send()
        if r.status_code != 200:
            assert _is_transient_gateway_response(r), (
                f"inference failed: {r.status_code} {r.text[:200]}"
            )
            last = f"transient {r.status_code}"
        else:
            series, unavailable = _limitador_authorized_hits_series()
            if unavailable:
                last = unavailable
            else:
                observed = [
                    labels
                    for labels in series
                    if all(labels.get(k) == v for k, v in match.items())
                ]
                if any(accept(labels) for labels in observed):
                    return observed
                last = f"observed {observed}"
        time.sleep(5)
    pytest.fail(f"no authorized_hits series with {match} and {what}; last: {last}")


def _assert_token_limited(cleanup, identity):
    """TRLP predicates match auth.identity.selected_subscription_id. If it stops
    resolving for an identity type, no limit matches and that type is served
    without a token budget, while every other test still sees 200s.
    """
    s = _suffix()
    token, subscription = _caller(
        cleanup,
        f"e2e-idf-{identity.replace('_', '-')}",
        LOW_TOKEN_LIMIT,
        LOW_LIMIT_WINDOW,
        s,
    )
    if identity == "api_key":
        credential, headers = (
            _mint_key(cleanup, token, f"e2e-idf-{s}", subscription),
            None,
        )
    else:
        credential, headers = token, {"x-maas-subscription": subscription}
    _poll_status(
        credential,
        200,
        path=MODEL_PATH,
        extra_headers=headers,
        inference_fn=_inference_min,
    )

    # Fails without a 429 by the enforcement deadline; the limiter may trail
    # the subscription status, so planned attempts alone aren't enough.
    result = exercise_until_token_limited(
        lambda: _inference_min(credential, path=MODEL_PATH, extra_headers=headers),
        max_attempts=EXHAUST_MAX_REQUESTS,
        enforcement_timeout_seconds=RATE_LIMIT_ENFORCEMENT_TIMEOUT,
    )
    log.info("%s limited after %d successful requests", identity, result.success_count)


# Smoke: on every PR, the only check that an API key still gets a token limit.
@pytest.mark.smoke
def test_token_limit_applies_to_api_key(cleanup):
    _assert_token_limited(cleanup, "api_key")


@pytest.mark.full_suite
def test_token_limit_applies_to_token_review(cleanup):
    _assert_token_limited(cleanup, "token_review")


@pytest.mark.full_suite
def test_series_for_subscription_without_organization_or_cost_center(cleanup):
    """The organization_id and cost_center labels read
    auth.identity.subscription_info.organizationId and .costCenter. maas-api
    omits both when the subscription sets neither, and the labels must still
    resolve to empty values. Otherwise the label reads null or the request's
    series is dropped, depending on the wasm-shim build.
    """
    s = _suffix()
    token, subscription = _caller(cleanup, "e2e-idf-noorg", 1000, "1m", s)
    key = _mint_key(cleanup, token, f"e2e-idf-noorg-{s}", subscription)
    _poll_status(key, 200, path=MODEL_PATH, inference_fn=_inference_min)
    _require_telemetry(_ns())

    observed = _wait_for_series(
        lambda: _inference_min(key, path=MODEL_PATH),
        {"subscription": subscription},
        lambda labels: True,
        "any labels",
    )

    for labels in observed:
        assert labels.get("organization_id", "") == "", (
            f"organization_id should be empty: {labels}"
        )
        assert labels.get("cost_center", "") == "", (
            f"cost_center should be empty: {labels}"
        )


def _patch_tenant_metrics(namespace, metrics):
    result = subprocess.run(
        [
            "oc",
            "patch",
            TENANT_CONFIG_KIND,
            TENANT_CR_NAME,
            "-n",
            namespace,
            "--type=merge",
            "-p",
            json.dumps({"spec": {"telemetry": {"metrics": metrics}}}),
        ],
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    assert result.returncode == 0, result.stderr or result.stdout


@pytest.mark.full_suite
def test_group_label_with_capture_group():
    """With captureGroup on, the group label reads auth.identity.groups_str and
    carries the caller's groups.

    Runs on its own tenant and gateway. Kuadrant keeps a label removed from a
    TelemetryPolicy in the gateway's wasm config (kuadrant-operator#2243), so
    turning captureGroup on for a shared tenant and back off would keep
    exporting caller groups after the test. Deleting the tenant deletes its
    TelemetryPolicy, which clears the binding.
    """
    require_aitenant_crd()
    require_tenant_namespace_discovery()
    s = _suffix()
    context = build_worker_tenant_case(f"idfgroup{s}")
    policy = f"e2e-idf-group-auth-{s}"
    subscription = f"e2e-idf-group-{s}"
    try:
        context = bootstrap_worker_tenant(context, baseline=False)
        _require_telemetry(context.tenant_namespace)
        _patch_tenant_metrics(context.tenant_namespace, {"captureGroup": True})

        _create_test_auth_policy(
            name=policy,
            model_refs=[context.model_ref],
            groups=["system:authenticated"],
            namespace=context.tenant_namespace,
            model_namespace=context.model_namespace,
        )
        _wait_for_maas_auth_policy_phase(
            policy, namespace=context.tenant_namespace, require_enforced=False
        )
        _create_test_subscription(
            name=subscription,
            model_refs=[context.model_ref],
            groups=["system:authenticated"],
            token_limit=1000,
            window="1m",
            namespace=context.tenant_namespace,
            model_namespace=context.model_namespace,
        )
        _wait_for_maas_subscription_phase(
            subscription, namespace=context.tenant_namespace
        )
        _wait_for_subscription_inference_ready(
            subscription,
            context.model_ref,
            namespace=context.tenant_namespace,
            model_namespace=context.model_namespace,
            require_enforced_trlp=False,
        )

        path = f"/{context.model_namespace}/{context.model_ref}"
        model_name = f"e2e/{context.model_ref}"
        with activate_worker_tenant(context):
            key = _create_api_key(_get_cluster_token(), subscription=subscription)
            _poll_status(
                key, 200, path=path, model_name=model_name, inference_fn=_inference_min
            )
            observed = _wait_for_series(
                lambda: _inference_min(key, path=path, model_name=model_name),
                {"subscription": subscription, "tenant_name": context.tenant_name},
                lambda labels: labels.get("group", "") != "",
                "a non-empty group label",
            )
    finally:
        teardown_worker_tenant(context)

    groups = {
        group for labels in observed for group in labels.get("group", "").split(",")
    }
    assert "system:authenticated" in groups, (
        f"group label should list the key's groups: {observed}"
    )
