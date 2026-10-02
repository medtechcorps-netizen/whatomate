#!/usr/bin/env python3
"""Image-only App Platform spec transform and topology guards for ship.yml.

Copied and trimmed from verify_production_release.py:501-747 (component map,
digest extraction, image-only diff, fingerprints) and verify_production_plan.py
(:43, :1805-1819 VPC binding, :1880-1921 component contract). Only digest
sources are accepted: a legacy git source is refused, never converted.
"""

from __future__ import annotations

import copy
import json
import sys
from pathlib import Path
from typing import Any, Mapping

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common


SOURCE_SELECTORS = ("git", "github", "gitlab", "bitbucket", "image")
FORBIDDEN_IMAGE_FIELDS = ("tag", "deploy_on_push", "registry_credentials")
IMAGE_KEYS = frozenset({"registry_type", "registry", "repository", "digest"})
EXPECTED_SERVICES = frozenset({"omnitech-web", "meta-relay", "gmail-relay"})
EXPECTED_JOBS = frozenset({common.PRE_DEPLOY_JOB})
EMPTY_COLLECTIONS = ("workers", "static_sites", "functions")


def component_map(spec: Mapping[str, Any], collection: str) -> dict[str, dict[str, Any]]:
    values = spec.get(collection)
    if type(values) is not list:
        common.fail("topology-differs:collection-malformed")
    output: dict[str, dict[str, Any]] = {}
    for value in values:
        if type(value) is not dict:
            common.fail("topology-differs:component-malformed")
        name = common.exact_string(value.get("name"), "topology-differs:component-name")
        if name in output:
            common.fail("topology-differs:duplicate-component")
        output[name] = value
    return output


def _binding(spec: Mapping[str, Any], collection: str, name: str) -> dict[str, Any]:
    if type(spec) is not dict:
        common.fail("topology-differs:spec-malformed")
    item = component_map(spec, collection).get(name)
    if item is None:
        common.fail("topology-differs:component-missing")
    return item


def refuse_forbidden_image_fields(spec: Mapping[str, Any]) -> None:
    """A tag, auto-deploy or registry credential would let something other
    than the reviewed digest reach production (contract
    logical_source_transform.forbidden_image_fields)."""
    for collection, name, _component, _repository in common.SPEC_BINDINGS:
        item = _binding(spec, collection, name)
        image = item.get("image")
        if type(image) is dict and any(field in image for field in FORBIDDEN_IMAGE_FIELDS):
            common.fail("forbidden-image-field")


def require_digest_sources(spec: Mapping[str, Any]) -> None:
    for collection, name, _component, _repository in common.SPEC_BINDINGS:
        item = _binding(spec, collection, name)
        if set(item).intersection(SOURCE_SELECTORS) != {"image"} or "dockerfile_path" in item:
            common.fail("topology-differs:source-mode")


def extract_image_digests(spec: Mapping[str, Any]) -> dict[str, str]:
    refuse_forbidden_image_fields(spec)
    require_digest_sources(spec)
    output: dict[str, str] = {}
    for collection, name, component, repository in common.SPEC_BINDINGS:
        item = _binding(spec, collection, name)
        image = common.exact_keys(item.get("image"), IMAGE_KEYS, "topology-differs:image-selector")
        if (
            image["registry_type"] != "GHCR"
            or image["registry"] != "ghcr.io"
            or image["repository"] != repository
        ):
            common.fail("topology-differs:image-repository")
        digest = common.require_digest(image["digest"], "topology-differs:image-digest")
        prior = output.get(component)
        if prior is not None and prior != digest:
            common.fail("topology-differs:migration-web-digest")
        output[component] = digest
    if set(output) != set(common.COMPONENTS):
        common.fail("topology-differs:image-set")
    return {component: output[component] for component in common.COMPONENTS}


def require_image_set(digests: Any, code: str) -> dict[str, str]:
    value = common.exact_keys(digests, common.COMPONENTS, code)
    return {component: common.require_digest(value[component], code) for component in common.COMPONENTS}


def set_images(spec: Mapping[str, Any], digests: Mapping[str, str]) -> dict[str, Any]:
    """verify_production_release.py:544-573 without the legacy-git branch:
    only the four ``image.digest`` leaves change."""
    target = require_image_set(dict(digests), "internal-error:target-images")
    extract_image_digests(spec)
    desired = copy.deepcopy(dict(spec))
    for collection, name, component, _repository in common.SPEC_BINDINGS:
        _binding(desired, collection, name)["image"]["digest"] = target[component]
    require_image_only_change(spec, desired)
    return desired


def changed_leaf_pointers(left: Any, right: Any, prefix: str = "") -> list[str]:
    """verify_production_release.py:576-596, verbatim."""
    if type(left) is not type(right):
        return [prefix or "/"]
    if type(left) is dict:
        pointers: list[str] = []
        for key in sorted(set(left) | set(right)):
            escaped = key.replace("~", "~0").replace("/", "~1")
            child = f"{prefix}/{escaped}"
            if key not in left or key not in right:
                pointers.append(child)
            else:
                pointers.extend(changed_leaf_pointers(left[key], right[key], child))
        return pointers
    if type(left) is list:
        if len(left) != len(right):
            return [prefix or "/"]
        pointers = []
        for index, (before, after) in enumerate(zip(left, right, strict=True)):
            pointers.extend(changed_leaf_pointers(before, after, f"{prefix}/{index}"))
        return pointers
    return [] if left == right else [prefix or "/"]


def _component_index(spec: Mapping[str, Any], collection: str, name: str) -> int:
    values = spec.get(collection)
    if type(values) is not list:
        common.fail("topology-differs:collection-malformed")
    matches = [index for index, item in enumerate(values) if type(item) is dict and item.get("name") == name]
    if len(matches) != 1:
        common.fail("topology-differs:component-selector")
    return matches[0]


def image_digest_pointers(spec: Mapping[str, Any]) -> set[str]:
    return {
        f"/{collection}/{_component_index(spec, collection, name)}/image/digest"
        for collection, name, _component, _repository in common.SPEC_BINDINGS
    }


def require_image_only_change(before: Mapping[str, Any], after: Mapping[str, Any]) -> list[str]:
    """verify_production_release.py:609-633 adapted: the changed leaves must be
    a NON-EMPTY subset of the four digest leaves, in unchanged component order."""
    allowed: set[str] = set()
    for collection, name, _component, _repository in common.SPEC_BINDINGS:
        index = _component_index(before, collection, name)
        if _component_index(after, collection, name) != index:
            common.fail("topology-differs:component-order")
        allowed.add(f"/{collection}/{index}/image/digest")
    changed = set(changed_leaf_pointers(before, after))
    if not changed:
        common.fail("topology-differs:no-image-change")
    if not changed.issubset(allowed):
        common.fail("topology-differs:outside-image-digests")
    return sorted(changed)


def environment_value_fingerprint(spec: Mapping[str, Any]) -> str:
    """verify_production_release.py:636-706, verbatim apart from reason codes."""
    records: list[list[str]] = []

    def environment_string(value: Any) -> str:
        if type(value) is not str or not value or len(value) > 512:
            common.fail("topology-differs:environment-entry")
        if "\n" in value or "\r" in value or "\x00" in value:
            common.fail("topology-differs:environment-entry")
        return value

    def collect(collection: str, component: str, raw: Any) -> None:
        if raw is None:
            raw = []
        if type(raw) is not list:
            common.fail("topology-differs:environment-list")
        seen: set[str] = set()
        for item in raw:
            if type(item) is not dict:
                common.fail("topology-differs:environment-entry")
            key = environment_string(item.get("key"))
            if key in seen:
                common.fail("topology-differs:environment-duplicate")
            seen.add(key)
            scope = item.get("scope", "RUN_TIME")
            if scope != "RUN_TIME":
                common.fail("topology-differs:environment-scope")
            value = item.get("value")
            if type(value) is not str:
                common.fail("topology-differs:environment-value")
            environment_type = item.get("type", "GENERAL")
            if environment_type not in {"GENERAL", "SECRET"}:
                common.fail("topology-differs:environment-type")
            records.append(
                [
                    collection,
                    component,
                    key,
                    scope,
                    environment_type,
                    common.sha256_bytes(value.encode("utf-8")),
                ]
            )

    collect("app", "app", spec.get("envs", []))
    for collection in ("services", "jobs", "workers", "static_sites", "functions"):
        raw = spec.get(collection, [])
        if type(raw) is not list:
            common.fail("topology-differs:collection-malformed")
        indexed: dict[str, dict[str, Any]] = {}
        for item in raw:
            if type(item) is not dict:
                common.fail("topology-differs:component-malformed")
            name = environment_string(item.get("name"))
            if name in indexed:
                common.fail("topology-differs:duplicate-component")
            indexed[name] = item
        for name, component in indexed.items():
            collect(collection, name, component.get("envs", []))
    try:
        canonical_records = json.dumps(
            sorted(records),
            allow_nan=False,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")
    except (TypeError, ValueError) as exc:
        raise common.ReleaseError("topology-differs:environment-inventory") from exc
    return common.sha256_bytes(canonical_records)


def strip_image_sources(spec: Mapping[str, Any]) -> dict[str, Any]:
    value = copy.deepcopy(dict(spec))
    for collection, name, _component, _repository in common.SPEC_BINDINGS:
        item = component_map(value, collection)[name]
        for key in ("git", "github", "gitlab", "dockerfile_path", "image"):
            item.pop(key, None)
    return value


def non_source_fingerprint(spec: Mapping[str, Any]) -> str:
    return common.sha256_value(strip_image_sources(spec))


def validate_target(value: Any) -> dict[str, Any]:
    """release/deployment/ship-target.json: committed public hashes only."""
    target = common.exact_keys(
        value,
        {
            "schema_version", "app_name", "region", "app_id_sha256",
            "default_ingress_sha256", "vpc_id_sha256", "postgres", "services",
            "pre_deploy_job",
        },
        "target-invalid:ship-target",
    )
    if target["schema_version"] != 1:
        common.fail("target-invalid:ship-target")
    common.exact_string(target["app_name"], "target-invalid:ship-target")
    common.exact_string(target["region"], "target-invalid:ship-target")
    for key in ("app_id_sha256", "default_ingress_sha256", "vpc_id_sha256"):
        common.require_sha256(target[key], "target-invalid:ship-target")
    postgres = common.exact_keys(
        target["postgres"], {"cluster_name_sha256", "version", "region"}, "target-invalid:ship-target"
    )
    common.require_sha256(postgres["cluster_name_sha256"], "target-invalid:ship-target")
    common.exact_string(postgres["version"], "target-invalid:ship-target")
    common.exact_string(postgres["region"], "target-invalid:ship-target")
    services = common.exact_keys(target["services"], EXPECTED_SERVICES, "target-invalid:ship-target")
    for name in sorted(EXPECTED_SERVICES):
        service = common.exact_keys(services[name], {"http_port", "health_path"}, "target-invalid:ship-target")
        common.exact_int(service["http_port"], "target-invalid:ship-target", 1, 65535)
        common.exact_string(service["health_path"], "target-invalid:ship-target")
    job = common.exact_keys(target["pre_deploy_job"], {"name", "run_command"}, "target-invalid:ship-target")
    if job["name"] != common.PRE_DEPLOY_JOB:
        common.fail("target-invalid:ship-target")
    common.exact_string(job["run_command"], "target-invalid:ship-target")
    return target


def require_vpc(spec: Mapping[str, Any], target: Mapping[str, Any]) -> str:
    """verify_production_plan.py:1815-1819. A token without vpc:read returns a
    spec without ``vpc``, so this also proves the token scope before any PUT."""
    vpc = spec.get("vpc")
    if type(vpc) is not dict or type(vpc.get("id")) is not str or not vpc["id"]:
        common.fail("vpc-missing-or-differs")
    if common.sha256_text(vpc["id"]) != target["vpc_id_sha256"]:
        common.fail("vpc-missing-or-differs")
    return vpc["id"]


def require_topology(spec: Mapping[str, Any], target: Mapping[str, Any]) -> str:
    """Name, region, VPC, component set, ports, health paths, the PRE_DEPLOY
    job and the PostgreSQL binding. Returns the bound PostgreSQL cluster name
    for the caller only; it is never emitted."""
    if type(spec) is not dict:
        common.fail("topology-differs:spec-malformed")
    if spec.get("name") != target["app_name"]:
        common.fail("topology-differs:name")
    if spec.get("region") != target["region"]:
        common.fail("topology-differs:region")
    require_vpc(spec, target)
    for collection in EMPTY_COLLECTIONS:
        if spec.get(collection, []) not in (None, []):
            common.fail("topology-differs:unexpected-component")
    services = component_map(spec, "services")
    jobs = component_map(spec, "jobs")
    if set(services) != EXPECTED_SERVICES:
        common.fail("topology-differs:services")
    if set(jobs) != EXPECTED_JOBS:
        common.fail("topology-differs:jobs")
    for name in sorted(EXPECTED_SERVICES):
        expected = target["services"][name]
        component = services[name]
        if type(component.get("http_port")) is not int or component.get("http_port") != expected["http_port"]:
            common.fail("topology-differs:http-port")
        health = component.get("health_check")
        if type(health) is not dict or health.get("http_path") != expected["health_path"]:
            common.fail("topology-differs:health-path")
    job = jobs[common.PRE_DEPLOY_JOB]
    if job.get("kind") != "PRE_DEPLOY":
        common.fail("topology-differs:job-kind")
    if job.get("run_command") != target["pre_deploy_job"]["run_command"]:
        common.fail("topology-differs:job-run-command")
    databases = spec.get("databases")
    if type(databases) is not list or any(type(item) is not dict for item in databases):
        common.fail("topology-differs:databases")
    postgres = [item for item in databases if item.get("engine") == "PG"]
    if len(postgres) != 2:
        common.fail("topology-differs:postgres-bindings")
    names: set[str] = set()
    for item in postgres:
        cluster = item.get("cluster_name")
        if type(cluster) is not str or not cluster:
            common.fail("topology-differs:postgres-bindings")
        if item.get("version") != target["postgres"]["version"] or item.get("production") is not True:
            common.fail("topology-differs:postgres-bindings")
        names.add(cluster)
    if len(names) != 1:
        common.fail("topology-differs:postgres-bindings")
    cluster_name = names.pop()
    if common.sha256_text(cluster_name) != target["postgres"]["cluster_name_sha256"]:
        common.fail("topology-differs:postgres-cluster")
    return cluster_name
