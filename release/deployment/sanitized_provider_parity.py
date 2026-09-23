"""Local, GET-only provider parity adapter; importing performs no work.

The caller supplies a REVIEWED authentication callback, not a file or a verified
flag. It must freshly authenticate current main/clean control, latest exact
predecessor run/attempt/gate/artifact and both attestations on every invocation.
It may return None only for an authenticated genesis launch. Private input
custody is also explicit: the callback returns (target JSON, read token) in
memory; this module never reads ambient credentials, runs a CLI or writes files.

This is a preflight observation, not deployment authority or a TOCTOU lock.
The protected workflow must repeat its own signed-plan/provider checks.
"""

from __future__ import annotations

import hashlib
import os
import time
import types
from pathlib import Path
from typing import Callable


VERIFIER_SHA256 = "1c36fbb582207bb7d62646d6086cac2d13b9ed3e559a589f8f27fe7d22f64cab"
CONTRACT_SHA256 = "0cda6325a566aca3aa5f2d78c4152f708d5aad5c7feb116a67bde7234b1c4c27"
PHASES = ("baseline", "bridge", "backend", "ui")
STATE_WORKFLOW = ".github/workflows/verify-production-crm-canary.yml"
ERROR_CODE = "provider-parity-read-or-verification-failed"
NOT_QUIESCENT_CODE = "provider-valid-observations-not-yet-quiescent"
STATE_KEYS = frozenset({
    "app_identity_sha256", "default_ingress_sha256", "app_updated_at_sha256",
    "active_deployment_identity_sha256", "canonical_spec_sha256",
    "environment_values_sha256", "non_source_projection_sha256", "source_mode", "images",
})
OBSERVED_HASH_KEYS = (
    "app_identity_sha256", "default_ingress_sha256", "app_updated_at_sha256",
    "active_deployment_identity_sha256", "live_canonical_spec_sha256",
    "active_canonical_spec_sha256", "environment_values_sha256", "non_source_projection_sha256",
)


class ProviderParityError(RuntimeError):
    """Only the constant ERROR_CODE is exposed at the public boundary."""


class ProviderNotQuiescent(ProviderParityError):
    """Two valid complete pairs differ only outside their fixed semantic state."""


def _require(condition: bool) -> None:
    if not condition:
        raise ProviderParityError(ERROR_CODE)


def make_environment_private_input_reader() -> Callable[[], tuple[str, str]]:
    """Lazily consume explicit protected runtime inputs, retaining memory only.

    Construct this once per normal-mode launcher reader. Construction/import does
    not inspect the environment; the first authenticated parity call consumes
    the two exact keys, preventing later child processes from inheriting them.
    No ambient doctl context, alternate token key or target discovery is used.
    """
    cached = None

    def read_private_inputs() -> tuple[str, str]:
        nonlocal cached
        try:
            if cached is None:
                target = os.environ.pop("DO_PRODUCTION_TARGET_JSON", None)
                token = os.environ.pop("DO_PRODUCTION_READ_TOKEN", None)
                _require(type(target) is str and bool(target) and type(token) is str and bool(token))
                cached = (target, token)
            return cached
        except Exception:
            raise ProviderParityError(ERROR_CODE) from None

    return read_private_inputs


def _load_reviewed_verifier(worktree: Path):
    # Exact raw-byte pins also bind path templates, target hashes and topology.
    # Execute only the already reviewed bytes, in memory, without pyc output.
    base = Path(worktree) / "release" / "deployment"
    verifier_path = base / "verify_production_plan.py"
    contract_path = base / "production-app-contract.json"
    with verifier_path.open("rb") as handle:
        source = handle.read(1024 * 1024 + 1)
    with contract_path.open("rb") as handle:
        raw_contract = handle.read(131073)
    _require(hashlib.sha256(source).hexdigest() == VERIFIER_SHA256)
    _require(hashlib.sha256(raw_contract).hexdigest() == CONTRACT_SHA256)
    module = types.ModuleType("_reviewed_provider_verifier")
    module.__file__ = str(verifier_path)
    exec(compile(source, str(verifier_path), "exec"), module.__dict__)
    contract = module.loads_strict(raw_contract.decode("utf-8"))
    return module, contract


def _image_authority(verifier, contract, records):
    _require(type(records) is list)
    expected_repositories = {
        item["release_component"]: item["image_repository"] for item in contract["components"]
    }
    _require(len(records) == len(expected_repositories))
    images = {}
    for record in records:
        _require(type(record) is dict and set(record) == {"component", "repository", "digest", "subject"})
        component = record["component"]
        _require(type(component) is str and component in expected_repositories and component not in images)
        repository = expected_repositories[component]
        digest = verifier.require_digest(record["digest"], "predecessor image digest")
        _require(record["repository"] == "ghcr.io/" + repository)
        _require(record["subject"] == "ghcr.io/" + repository + "@" + digest)
        images[component] = {"repository": repository, "digest": digest, "subject": record["subject"]}
    _require(records == verifier.target_image_records(contract, images))
    return images


def _expectation(verifier, contract, control_sha, phase, authenticated):
    bootstrap = contract["bootstrap_state"]
    if phase == "baseline":
        _require(authenticated is None)
        expected, images = verifier.predecessor_provider_expectation(contract, {}, None)
        _require(expected["source_mode"] == "digest-images")
        _require(images == _image_authority(verifier, contract, bootstrap["images"]))
        predecessor_hash = verifier.genesis_state_sha256(contract)
        _require(predecessor_hash == bootstrap["genesis_state_sha256"])
        return expected, images, predecessor_hash

    _require(type(authenticated) is bytes and 0 < len(authenticated) <= 131072)
    state = verifier.loads_strict(authenticated.decode("utf-8"))
    _require(type(state) is dict and state.get("authority") == "production-phase-state"
             and state.get("repository") == contract["repository"])
    control, lineage = state["control"], state["lineage"]
    previous = PHASES[PHASES.index(phase) - 1]
    _require(control["workflow_sha"] == control_sha and control["workflow_path"] == STATE_WORKFLOW
             and control["runner_environment"] == "github-hosted")
    verifier.require_run_id(control["run_id"], "authenticated predecessor run")
    _require(type(control["run_attempt"]) is int and control["run_attempt"] >= 1)
    _require(lineage["phase"] == previous and lineage["to"] == previous
             and type(lineage["phase_ordinal"]) is int and lineage["phase_ordinal"] == PHASES.index(previous) + 1)
    _require(all(state["gates"].get(key) is True for key in
                 ("deployment_succeeded", "migration_succeeded", "canary_succeeded")))
    provider = state["provider_state"]
    _require(type(provider) is dict and set(provider) == STATE_KEYS)
    for key in STATE_KEYS - {"source_mode", "images"}:
        verifier.require_sha256(provider[key], "authenticated provider fingerprint")
    _require(provider["app_identity_sha256"] == contract["provider"]["app_id_sha256"]
             and provider["default_ingress_sha256"] == contract["provider"]["default_ingress_sha256"]
             and provider["environment_values_sha256"] == bootstrap["environment_values_sha256"]
             and provider["non_source_projection_sha256"] == bootstrap["non_source_projection_sha256"]
             and provider["source_mode"] == "digest-images")
    images = _image_authority(verifier, contract, provider["images"])
    expected = {key: provider[key] for key in (
        "active_deployment_identity_sha256", "canonical_spec_sha256", "environment_values_sha256",
        "non_source_projection_sha256", "source_mode",
    )}
    return expected, images, hashlib.sha256(authenticated).hexdigest()


def _observe(verifier, contract, target, token, expected, images):
    client = verifier.ProviderClient(contract, target, token)
    pair_hashes, states = [], []
    for index in range(2):
        if index:
            # Fixed production delay; no launcher flag can shorten it.
            time.sleep(60)
        app = client.get_json(client.app_path)
        active_id = app["app"]["active_deployment"]["id"]
        deployment_path = client.bind_active_deployment(target, active_id)
        deployment = client.get_json(deployment_path)
        observed, _private_spec = verifier.provider_state(app, deployment, contract, target, expected, images)
        # Compare complete canonical app+deployment responses, not selected fields.
        pair_hashes.append(verifier.sha256_value({"app_response": app, "deployment_response": deployment}))
        states.append(observed)
    # Every semantic guard above must pass in BOTH rounds. Only metadata/full-
    # response drift and the observed update timestamp may reach the transient
    # path; a difference in any other sanitized state field fails closed.
    semantic_states = [{key: value for key, value in state.items() if key != "app_updated_at_sha256"}
                       for state in states]
    _require(semantic_states[0] == semantic_states[1])
    _require(client.request_log == [
        ("GET", client.app_path), ("GET", client.deployment_path),
        ("GET", client.app_path), ("GET", client.deployment_path),
    ])
    quiescent = pair_hashes[0] == pair_hashes[1] and states[0] == states[1]
    return states[1], pair_hashes[1], quiescent


def require_provider_parity(
    *, worktree: Path, control_sha: str, phase: str,
    authenticate_predecessor: Callable[[], bytes | None],
    read_private_inputs: Callable[[], tuple[str, str]],
) -> dict[str, str]:
    """Fresh double observation; return only fixed statuses and validated hashes.

    read_private_inputs returns (target_descriptor_json, read_token). The caller
    owns credential scope/custody; only fixed GETs are supported here. No raw
    predecessor-file path, operator verification flag, interval or CLI is accepted.
    Missing optional deployment flags retain the reviewed provider_state policy
    (reject any non-null value); they are NOT reported as explicitly false.
    """
    try:
        verifier, contract = _load_reviewed_verifier(worktree)
        verifier.require_sha1(control_sha, "current control")
        _require(phase in PHASES and callable(authenticate_predecessor) and callable(read_private_inputs))
        authenticated = authenticate_predecessor()
        expected, images, predecessor_hash = _expectation(verifier, contract, control_sha, phase, authenticated)
        private = read_private_inputs()
        _require(type(private) is tuple and len(private) == 2)
        raw_target, token = private
        _require(type(raw_target) is str and 0 < len(raw_target.encode("utf-8")) <= 4096)
        target = verifier.normalize_target_descriptor(raw_target, contract)
        observed, pair_hash, quiescent = _observe(verifier, contract, target, token, expected, images)
        # Re-authenticate current control AND the same predecessor after the delay.
        _require(authenticate_predecessor() == authenticated)
        report = {key: verifier.require_sha256(observed[key], "observed fingerprint") for key in OBSERVED_HASH_KEYS}
        report.update({
            "status": "provider-parity-verified",
            "observation_status": "two-complete-identical-pairs-60s-apart",
            "active_status": "ACTIVE",
            "pending_policy_status": "accepted-no-nonnull-pending-fields",
            "source_status": "digest-images",
            "control_sha1": control_sha,
            "verifier_sha256": VERIFIER_SHA256,
            "contract_sha256": CONTRACT_SHA256,
            "predecessor_state_sha256": predecessor_hash,
            "complete_provider_pair_sha256": pair_hash,
            "images_sha256": verifier.sha256_value(verifier.target_image_records(contract, images)),
        })
    except Exception:
        # Never include request paths, raw bodies, credentials, chained exceptions
        # or untrusted callback exception text in caller-visible errors.
        raise ProviderParityError(ERROR_CODE) from None
    # Deliberately outside the exception boundary: a callback/transport throwing
    # this type itself is NOT a validated transient and must become generic.
    # Authenticated current control and exact predecessor bytes were rechecked
    # above even when the individually valid provider pairs were still moving.
    if not quiescent:
        raise ProviderNotQuiescent(NOT_QUIESCENT_CODE)
    return report
