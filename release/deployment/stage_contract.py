#!/usr/bin/env python3
"""Pure, fail-closed contracts for the future stage lane; no provider capability.

These guards deliberately do not enable ship.py or ship.yml. A configured target
is not attestation, authorization, stable observation, or proof of a deployment.
The stage executor must perform those checks before giving a client PUT access.
Production spec_images.py remains unchanged.
"""
from __future__ import annotations

import base64
import copy
import json
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common
import spec_images

ROOT = Path(__file__).resolve().parents[2]
TEMPLATE_PATH = ROOT / "release/staging/app-spec.template.yaml"
APP_HOST_SUFFIX = "ondigitalocean" + ".app"
STAGING_REPOS = {name: f"ghcr.io/medtechcorps-netizen/rereply-staging-{name}" for name in ("graph-stub", "bootstrap")}
SERVICES = frozenset({"omnitech-web", "meta-relay", "gmail-relay", "graph-stub"})
BINDINGS = common.SPEC_BINDINGS
DRILLS = frozenset({"none", "e2e-fail", "health-fail", "bad-image"})
PIN_KEYS = {"schema_version", "profile", "team_uuid_sha256", "app_id_sha256"}
TARGET_KEYS = {"schema_version", "profile", "app_id", "origin", "postgres_id", "valkey_id", "vpc_id", "postgres_name", "graph_stub", "bootstrap", "template_sha256", "template_values"}
RECEIPT_KEYS = {"schema_version", "profile", "run_id", "candidate_sha256", "ingress_sha256", "app_id_sha256", "drill", "previous_images", "candidate_images", "before_spec_sha256", "after_spec_sha256", "before_deployment_sha256", "candidate_deployment_sha256", "previous_source_sha", "candidate_source_sha"}


def require(condition, code):
    if not condition:
        common.fail(code)


def schema(value, keys, code):
    common.exact_keys(value, keys, code)
    require(type(value["schema_version"]) is int and value["schema_version"] == 1, code)
    return value


def stage_origin(value, production_origin_sha256):
    code = "target-invalid:staging-origin"
    common.exact_string(value, code)
    try:
        parsed = urlsplit(value)
        require(parsed.scheme == "https" and parsed.netloc == parsed.hostname and
                parsed.hostname is not None and parsed.hostname.endswith("." + APP_HOST_SUFFIX) and
                re.fullmatch(r"[a-z0-9][a-z0-9-]*\." + re.escape(APP_HOST_SUFFIX), parsed.hostname) is not None and
                not parsed.path and not parsed.query and not parsed.fragment and not parsed.username and
                value == "https://" + parsed.hostname, code)
    except ValueError:
        common.fail(code)
    common.require_sha256(production_origin_sha256, code)
    require(common.sha256_text(value) != production_origin_sha256, "target-invalid:production-origin")
    return value


def validate_pins(value, production):
    code = "target-invalid:staging-pins"
    schema(value, PIN_KEYS, code)
    require(value["profile"] == "staging", code)
    for key in ("team_uuid_sha256", "app_id_sha256"):
        # null is the intentional unconfigured state committed before owner setup.
        require(value[key] is not None, "target-invalid:staging-unconfigured")
        common.require_sha256(value[key], code)
        require(value[key] != "0" * 64, code)
    require(value["app_id_sha256"] != production["app_id_sha256"], "target-invalid:production-app")
    return value


def image_ref(value, repository, code="target-invalid:staging-image"):
    common.exact_string(value, code)
    require(value.startswith(repository + "@"), code)
    common.require_digest(value[len(repository) + 1:], code)
    return value


def validate_target(value, pins, production, template):
    """Local identity mismatch is rejected without any provider request."""
    validate_pins(pins, production)
    code = "target-invalid:staging-target"
    schema(value, TARGET_KEYS, code)
    require(value["profile"] == "staging", code)
    for key in ("app_id", "postgres_id", "valkey_id", "vpc_id"):
        common.require_uuid(value[key], code)
    require(len({value[key] for key in ("app_id", "postgres_id", "valkey_id", "vpc_id")}) == 4, code)
    require(common.sha256_text(value["app_id"]) == pins["app_id_sha256"], "app-identity-mismatch")
    stage_origin(value["origin"], production["default_ingress_sha256"])
    require(common.sha256_text(value["vpc_id"]) != production["vpc_id_sha256"], "target-invalid:production-vpc")
    require(value["postgres_name"] == "rereply-staging-pg", code)
    require(common.sha256_text(value["postgres_name"]) != production["postgres"]["cluster_name_sha256"], "target-invalid:production-database")
    for field, component in (("graph_stub", "graph-stub"), ("bootstrap", "bootstrap")):
        binding = common.exact_keys(value[field], {"image", "source_sha"}, code)
        image_ref(binding["image"], STAGING_REPOS[component])
        common.require_sha1(binding["source_sha"], code)
    # Hash the canonical JSON, independent of checkout line endings.
    require(value["template_sha256"] == common.sha256_value(template), "target-invalid:staging-template")
    values = common.exact_keys(value["template_values"], {"admin_email", "stub_app_id", "stub_accounts", "canary_organization"}, code)
    require(type(values["admin_email"]) is str and re.fullmatch(r"[a-zA-Z0-9._+-]+@rereply\.invalid", values["admin_email"]), code)
    common.exact_string(values["stub_app_id"], code, re.compile(r"[0-9]{5,30}"))
    common.require_uuid(values["canary_organization"], code)
    accounts = values["stub_accounts"]
    require(type(accounts) is list and len(accounts) == 1, code)
    account = common.exact_keys(accounts[0], {"business_account_id", "phone_number_id", "display_phone_number"}, code)
    for key in ("business_account_id", "phone_number_id"):
        common.exact_string(account[key], code, re.compile(r"[0-9]{5,30}"))
    common.exact_string(account["display_phone_number"], code, re.compile(r"\+1555[0-9]{7}"))
    return value


def validate_environment(env):
    # Presence, including an empty value, is refused. No ambient production or
    # doctl credential can become an accidental fallback for the future client.
    forbidden = set(common.FORBIDDEN_AMBIENT) | {"SHIP_DO_TOKEN", "SHIP_TARGET_JSON", "DOCTL_ACCESS_TOKEN", "DOCTL_CONTEXT", "DOCTL_CONFIG", "DOCTL_API_URL", "DIGITALOCEAN_CONTEXT"}
    # These are the explicit stage inputs, not forbidden fallbacks when common
    # later adds them to the production ambient refusal list.
    forbidden -= {"STAGING_DO_TOKEN", "STAGING_TARGET_JSON"}
    require(not forbidden.intersection(env), "context-invalid:staging-ambient")


def validate_drill(mode, drill):
    require(type(mode) is str and mode in {"stage", "dry-run", "promote", "rollback"}, "input-invalid:mode")
    require(type(drill) is str and drill in DRILLS and (drill == "none" or mode == "stage"), "input-invalid:stage-drill")
    return drill


def validate_inventory(account, apps, clusters, firewalls, target, pins, production):
    """Validate read-only inventory before granting PUT; never discovers targets.

    The caller must exhaust pagination. Seeing production necessarily needs a
    GET; the safety invariant is zero writes, not the old plan's zero requests.
    """
    code = "target-invalid:staging-inventory"
    require(type(account) is dict and account.get("status") == "active", code)
    team = account.get("team")
    require(type(team) is dict and team.get("name") == "ReReply Staging", code)
    team_id = common.exact_string(team.get("uuid"), code)
    require(common.sha256_text(team_id) == pins["team_uuid_sha256"], "target-invalid:staging-team")
    require(type(apps) is list and type(clusters) is list, code)
    for app in apps:
        require(type(app) is dict, code)
        identity = common.require_uuid(app.get("id"), code)
        require(common.sha256_text(identity) != production["app_id_sha256"], "target-invalid:production-app-visible")
    # A separate staging team has exactly this app and these two clusters. New
    # resources require an owner-reviewed contract update, not silent selection.
    require([app["id"] for app in apps] == [target["app_id"]], code)
    require(len(clusters) == 2, code)
    observed = {}
    for cluster in clusters:
        require(type(cluster) is dict, code)
        identity = common.require_uuid(cluster.get("id"), code)
        name = common.exact_string(cluster.get("name"), code)
        require(common.sha256_text(name) != production["postgres"]["cluster_name_sha256"], "target-invalid:production-database-visible")
        require(identity not in observed, code)
        observed[identity] = cluster
    common.exact_keys(firewalls, {target["postgres_id"], target["valkey_id"]}, code)
    for field, engine, name in (("postgres_id", "pg", target["postgres_name"]), ("valkey_id", "valkey", "rereply-staging-valkey")):
        cluster = observed.get(target[field], {})
        require(cluster.get("engine") == engine and cluster.get("name") == name and
                cluster.get("status") == "online" and cluster.get("region") == "sgp1" and
                cluster.get("private_network_uuid") == target["vpc_id"], code)
        require(engine != "pg" or cluster.get("version") == "17", code)
        rules = firewalls[target[field]]
        require(type(rules) is list and len(rules) == 1 and type(rules[0]) is dict and
                rules[0].get("type") == "app" and rules[0].get("value") == target["app_id"], "target-invalid:staging-firewall")


def selector(reference):
    repository, digest = reference.split("@")
    return {"registry_type": "GHCR", "registry": "ghcr.io", "repository": repository.removeprefix("ghcr.io/"), "digest": digest, "deploy_on_push": {"enabled": False}}


def expected_spec(target, template, images):
    """Resolve non-secret leaves only. This result must NEVER be sent to PUT."""
    spec_images.require_image_set(images, "candidate-invalid:staging-images")
    values = {"vpc_id": target["vpc_id"], "pg_name": target["postgres_name"],
              "admin_email": target["template_values"]["admin_email"],
              "stub_app_id": target["template_values"]["stub_app_id"],
              # Canonical target exports sort nested object keys. Rebuild the
              # validated fields in setup.py's order so this GENERAL string
              # remains byte-identical after export/reload; actual specs stay strict.
              "stub_accounts": json.dumps([
                  {key: account[key] for key in ("business_account_id", "phone_number_id", "display_phone_number")}
                  for account in target["template_values"]["stub_accounts"]]),
              "allowlist": target["template_values"]["canary_organization"], "reply_enabled": "true"}
    def fill(item):
        if type(item) is dict:
            if item.get("type") == "SECRET":
                return {**item, "value": "<private-value>"}
            return {key: fill(value) for key, value in item.items()}
        if type(item) is list:
            return [fill(value) for value in item]
        if type(item) is str and item.startswith("@@") and item.endswith("@@"):
            require(item[2:-2] in values, "target-invalid:staging-placeholder")
            return values[item[2:-2]]
        return item
    result = fill(template)
    for group in ("services", "jobs"):
        for component in result[group]:
            kind = {"omnitech-web": "web", common.PRE_DEPLOY_JOB: "web"}.get(component["name"], component["name"])
            reference = target["graph_stub"]["image"] if kind == "graph-stub" else common.IMAGE_REPOSITORY[kind] + "@" + images[kind]
            component["image"] = selector(reference)
    return result


def projection(spec, *, private_values=False):
    """Normalize only known provider defaults; reject all other shape changes."""
    code = "topology-differs:staging-spec"
    require(type(spec) is dict, code)
    result = copy.deepcopy(spec)
    for key in ("domains", "workers", "static_sites", "functions", "envs", "alerts"):
        if key in result:
            require(result[key] == [], code)
            del result[key]
    common.exact_keys(result, {"name", "region", "vpc", "services", "jobs", "databases", "ingress"}, code)
    for group, names in (("services", SERVICES), ("jobs", {common.PRE_DEPLOY_JOB})):
        components = spec_images.component_map(result, group)
        require(set(components) == names, code)
        for component in components.values():
            # None/false is not accepted for source selectors or sensitive keys.
            for key in ("git", "github", "gitlab", "bitbucket", "dockerfile_path", "source_dir", "build_command", "routes", "autoscaling", "log_destinations", "volumes"):
                require(key not in component, code)
            image = component.get("image")
            require(type(image) is dict, code)
            if "deploy_on_push" in image:
                deploy_on_push = image.pop("deploy_on_push")
                # godo omits false, leaving an exact empty disabled object.
                require(type(deploy_on_push) is dict and set(deploy_on_push) <= {"enabled"} and
                        deploy_on_push.get("enabled", False) is False, code)
            common.exact_keys(image, {"registry_type", "registry", "repository", "digest"}, code)
            require(image["registry_type"] == "GHCR" and image["registry"] == "ghcr.io", code)
            common.require_digest(image["digest"], code)
            if group == "services":
                # Default service-name LAN routing needs no additional ports.
                # Normalize only the provider's absent/empty-list equivalent.
                ports = component.pop("internal_ports", [])
                require(type(ports) is list and ports == [], code)
                require(component.pop("protocol", "HTTP") == "HTTP", code)
                if component.get("run_command") == "":
                    del component["run_command"]
            envs = component.get("envs")
            require(type(envs) is list, code)
            seen = set()
            for env in envs:
                require(type(env) is dict, code)
                env.setdefault("scope", "RUN_TIME")
                env.setdefault("type", "GENERAL")
                # Only absent GENERAL strings have the provider's empty default.
                # SECRET presence/nonempty checks and explicit null are unchanged.
                if env["type"] == "GENERAL": env.setdefault("value", "")
                common.exact_keys(env, {"key", "value", "scope", "type"}, code)
                key = common.exact_string(env["key"], code)
                require(key not in seen and env["scope"] == "RUN_TIME" and env["type"] in {"GENERAL", "SECRET"}, code)
                seen.add(key)
                require(type(env["value"]) is str and not any(c in env["value"] for c in "\r\n\x00"), code)
                if env["type"] == "SECRET":
                    require(bool(env["value"]), code)
                    if not private_values:
                        env["value"] = "<private-value>"
            envs.sort(key=lambda env: env["key"])
        result[group] = sorted(components.values(), key=lambda component: component["name"])
    databases = result["databases"]
    require(type(databases) is list and all(type(item) is dict and type(item.get("name")) is str for item in databases), code)
    require(len({item["name"] for item in databases}) == len(databases), code)
    result["databases"] = sorted(databases, key=lambda item: item["name"])
    ingress = result["ingress"]
    require(type(ingress) is dict and type(ingress.get("rules")) is list, code)
    for rule in ingress["rules"]:
        require(type(rule) is dict and type(rule.get("component")) is dict, code)
        component = rule["component"]
        if component.get("preserve_path_prefix") is False:
            del component["preserve_path_prefix"]
        if component.get("rewrite") == "":
            del component["rewrite"]
    return result


def validate_spec(spec, target, template, images):
    # Python considers True == 1: canonical bytes retain JSON value types.
    require(common.canonical_payload_bytes(projection(spec)) ==
            common.canonical_payload_bytes(projection(expected_spec(target, template, images))),
            "topology-differs:staging-template")
    return spec


def image_set(spec):
    # Also validates exact component counts and duplicate names before selection.
    projection(spec)
    images = {}
    for group, name, kind, repository in BINDINGS:
        image = spec_images.component_map(spec, group)[name]["image"]
        require(image["repository"] == repository, "topology-differs:staging-repository")
        if kind in images:
            require(images[kind] == image["digest"], "topology-differs:staging-web-job")
        images[kind] = image["digest"]
    stub = spec_images.component_map(spec, "services")["graph-stub"]["image"]
    require(stub["repository"] == STAGING_REPOS["graph-stub"].removeprefix("ghcr.io/"), "topology-differs:staging-stub")
    return spec_images.require_image_set(images, "topology-differs:staging-images")


def set_images(spec, images):
    """Change exactly product digest leaves, preserving every private value."""
    image_set(spec)
    images = spec_images.require_image_set(images, "candidate-invalid:staging-images")
    result = copy.deepcopy(spec)
    allowed = set()
    for group, name, kind, _ in BINDINGS:
        for index, component in enumerate(result[group]):
            if component["name"] == name:
                component["image"]["digest"] = images[kind]
                allowed.add(f"/{group}/{index}/image/digest")
    changed = set(spec_images.changed_leaf_pointers(spec, result))
    require(bool(changed) and changed <= allowed, "topology-differs:staging-image-transform")
    return result


def bad_image_spec(spec, target, *, mode, drill):
    validate_drill(mode, drill)
    require(mode == "stage" and drill == "bad-image", "input-invalid:stage-drill")
    image_set(spec)
    result = copy.deepcopy(spec)
    # The intentional failure changes the FULL source selector of web and job;
    # a bootstrap digest under the release-web repository is not the same image.
    image_ref(target["bootstrap"]["image"], STAGING_REPOS["bootstrap"])
    for group, name in (("services", "omnitech-web"), ("jobs", common.PRE_DEPLOY_JOB)):
        spec_images.component_map(result, group)[name]["image"] = selector(target["bootstrap"]["image"])
    return result


def spec_fingerprint(spec):
    # Include encrypted secret values for CAS. Never return the projection.
    return common.sha256_value(projection(spec, private_values=True))


def validate_receipt(value):
    code = "input-invalid:stage-receipt"
    schema(value, RECEIPT_KEYS, code)
    require(value["profile"] == "staging", code)
    common.require_run_id(value["run_id"], code)
    require(type(value["run_id"]) is str, code)
    validate_drill("stage", value["drill"])
    for key in ("previous_source_sha", "candidate_source_sha"):
        common.require_sha1(value[key], code)
    for key in RECEIPT_KEYS - {"schema_version", "profile", "run_id", "drill", "previous_images", "candidate_images", "previous_source_sha", "candidate_source_sha"}:
        common.require_sha256(value[key], code)
    for key in ("previous_images", "candidate_images"):
        spec_images.require_image_set(value[key], code)
    require(value["before_deployment_sha256"] != value["candidate_deployment_sha256"], code)
    return value


def receipt_bytes(value):
    return common.canonical_file_bytes(validate_receipt(value))


def decode_receipt(encoded, expected_sha256, *, run_id, candidate_sha256, origin, production_origin_sha256, app_id_sha256):
    common.require_sha256(expected_sha256, "input-invalid:stage-receipt")
    require(type(encoded) is str and len(encoded) <= 16384, "input-invalid:stage-receipt")
    try:
        raw = base64.b64decode(encoded, validate=True)
    except ValueError:
        common.fail("input-invalid:stage-receipt")
    require(common.sha256_bytes(raw) == expected_sha256, "input-invalid:stage-receipt-hash")
    receipt = validate_receipt(common.loads_strict(raw))
    require(raw == receipt_bytes(receipt), "input-invalid:stage-receipt-canonical")
    stage_origin(origin, production_origin_sha256)
    require(receipt["run_id"] == run_id and receipt["candidate_sha256"] == candidate_sha256 and
            receipt["ingress_sha256"] == common.sha256_text(origin) and receipt["app_id_sha256"] == app_id_sha256,
            "input-invalid:stage-receipt-binding")
    return receipt
