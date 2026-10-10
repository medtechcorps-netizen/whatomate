#!/usr/bin/env python3
"""Offline, allowlisted bridge from owner setup to a private staging canary file.

No network requests, provider credentials, browser, or provisioning API exists in
this module. All errors are fixed codes. The full owner state must never become
STAGING_CANARY_FIXTURE_JSON; export explicitly selects each allowed field.
"""
from __future__ import annotations

import argparse
import copy
import os
from pathlib import Path
import re
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common
import stage_contract as contract

CANARY_KEYS = {"origin", "namespace", "stub_origin", "stub_app_secret", "klinik_organization_id", "fixture"}
DIGITS = re.compile(r"[1-9][0-9]{5,31}")
LABEL = re.compile(r"[A-Za-z0-9][A-Za-z0-9 ._:-]{0,95}")
CODE = "input-invalid:stage-fixture"
require = contract.require


def validate_fixture(value, production_origin_sha256):
    contract.schema(value, {"schema_version", "origin_sha256", "canary"}, CODE)
    common.require_sha256(value["origin_sha256"], CODE)
    canary = common.exact_keys(value["canary"], CANARY_KEYS, CODE)
    origin = contract.stage_origin(canary["origin"], production_origin_sha256)
    require(common.sha256_text(origin) == value["origin_sha256"], CODE)
    require(canary["stub_origin"] == origin + "/_stub", CODE)
    namespace = common.exact_string(canary["namespace"], CODE, re.compile(r"rereply-staging-[a-z0-9][a-z0-9-]{0,25}"))
    common.require_uuid(canary["klinik_organization_id"], CODE)
    secret(canary["stub_app_secret"], minimum=16)
    fixture = common.exact_keys(canary["fixture"], {"descriptor", "klinik_login", "non_klinik_login"}, CODE)
    descriptor = contract.schema(fixture["descriptor"], {"schema_version", "product_origin", "fixture_namespace", "klinik", "non_klinik"}, CODE)
    require(descriptor["product_origin"] == origin and descriptor["fixture_namespace"] == namespace, CODE)
    klinik = common.exact_keys(descriptor["klinik"], {"organization_id", "conversations", "meta"}, CODE)
    other = common.exact_keys(descriptor["non_klinik"], {"organization_id"}, CODE)
    common.require_uuid(other["organization_id"], CODE)
    require(klinik["organization_id"] == canary["klinik_organization_id"] and klinik["organization_id"] != other["organization_id"], CODE)
    conversations = common.exact_keys(klinik["conversations"], {"a", "b"}, CODE)
    for conversation in conversations.values():
        common.exact_keys(conversation, {"conversation_id", "contact_id", "display_name", "sender_wa_id"}, CODE)
        for key in ("conversation_id", "contact_id"):
            common.require_uuid(conversation[key], CODE)
        label = common.exact_string(conversation["display_name"], CODE, LABEL)
        require(label.lower().startswith(namespace + "-"), CODE)
        common.exact_string(conversation["sender_wa_id"], CODE, DIGITS)
    require(all(conversations["a"][key] != conversations["b"][key] for key in conversations["a"]), CODE)
    meta = common.exact_keys(klinik["meta"], {"business_account_id", "phone_number_id", "display_phone_number", "channel_account_id", "legacy_account_id", "legacy_account_name"}, CODE)
    for key in ("business_account_id", "phone_number_id", "display_phone_number"):
        common.exact_string(meta[key], CODE, DIGITS)
    for key in ("channel_account_id", "legacy_account_id"):
        common.require_uuid(meta[key], CODE)
    label = common.exact_string(meta["legacy_account_name"], CODE, LABEL)
    require(label.startswith(namespace + "-"), CODE)
    for key, suffix in (("klinik_login", "klinik"), ("non_klinik_login", "other")):
        login = contract.schema(fixture[key], {"schema_version", "email", "password"}, CODE)
        # These are exactly the distinct non-super users created by PR7. A
        # bootstrap administrator cannot be substituted for tenant isolation.
        require(login["email"] == namespace + "-" + suffix + "@example.test", CODE)
        secret(login["password"])
    require(fixture["klinik_login"]["password"] != fixture["non_klinik_login"]["password"], CODE)
    return value


def secret(value, *, minimum=8):
    common.exact_string(value, CODE, re.compile(r"[\x20-\x7e]{" + str(minimum) + r",256}"))
    return value


def export_fixture(state, production_origin_sha256):
    require(type(state) is dict and type(state.get("canary")) is dict, CODE)
    require(type(state.get("schema_version")) is int and state["schema_version"] == 1 and state.get("database_verified") is True and not state.get("pending_operation"), "input-invalid:stage-setup-incomplete")
    common.require_uuid(state.get("app_id"), CODE)
    common.require_uuid(state.get("deployment_id"), CODE)
    canary = state["canary"]
    require(all(key in canary for key in CANARY_KEYS), CODE)
    require(state.get("applied_canary_organization") == canary["klinik_organization_id"], "input-invalid:stage-allowlist-not-applied")
    result = {"schema_version": 1, "origin_sha256": state.get("origin_sha256"),
              "canary": {key: copy.deepcopy(canary[key]) for key in CANARY_KEYS}}
    validate_fixture(result, production_origin_sha256)
    # Additional defense against a source file whose admin email was replaced
    # with one of the otherwise valid synthetic login names.
    require(all(result["canary"]["fixture"][key]["email"] != canary.get("admin_email") for key in ("klinik_login", "non_klinik_login")), CODE)
    return result


def import_fixture(raw, *, origin, control_key, receipt_b64, receipt_sha256, run_id, candidate_sha256, app_id_sha256, production_origin_sha256):
    require(type(raw) is str and len(raw.encode("utf-8")) <= 32768, CODE)
    fixture = validate_fixture(common.loads_strict(raw, code=CODE), production_origin_sha256)
    # All independent bindings must agree BEFORE a consumer can acquire a
    # Playwright/network capability. Recomputing only the supplied hash is not
    # evidence that it came from the owner's setup or this deployment run.
    receipt = contract.decode_receipt(receipt_b64, receipt_sha256, run_id=run_id,
        candidate_sha256=candidate_sha256, origin=origin,
        production_origin_sha256=production_origin_sha256, app_id_sha256=app_id_sha256)
    require(fixture["canary"]["origin"] == origin and fixture["origin_sha256"] == receipt["ingress_sha256"], "input-invalid:stage-fixture-origin-binding")
    require(receipt["drill"] in {"none", "e2e-fail"}, "input-invalid:stage-fixture-drill")
    result = copy.deepcopy(fixture)
    result["canary"]["stub_control_key"] = secret(control_key, minimum=32)
    return result


def export_target(state, pins, production, template):
    # The export validates fixture readiness without serializing its logins or
    # any database/Valkey/app secret into the provider job's target.
    export_fixture(state, production["default_ingress_sha256"])
    source = state.get("target", {})
    canary = state["canary"]
    require(source.get("team_sha256") == pins.get("team_uuid_sha256"), "target-invalid:staging-team")
    images = state.get("images", {})
    result = {"schema_version": 1, "profile": "staging", "app_id": state["app_id"], "origin": canary["origin"],
              "postgres_id": source.get("postgres_id"), "valkey_id": source.get("valkey_id"), "vpc_id": source.get("vpc_id"),
              "postgres_name": "rereply-staging-pg", "template_sha256": common.sha256_value(template),
              "graph_stub": {"image": images.get("graph-stub"), "source_sha": images.get("source_sha")},
              "bootstrap": {"image": images.get("bootstrap"), "source_sha": images.get("source_sha")},
              "template_values": {"admin_email": canary.get("admin_email"), "stub_app_id": canary.get("stub_app_id"),
                  "stub_accounts": [{"business_account_id": canary.get("stub_waba_id"), "phone_number_id": canary.get("stub_phone_id"), "display_phone_number": "+15555550101"}],
                  "canary_organization": canary["klinik_organization_id"]}}
    return contract.validate_target(result, pins, production, template)


def publish_private(path, value):
    """Publish a complete owner-only file atomically, without replacing a peer.

    The caller has already secured/checked the parent directory. Hard-link
    creation on the same filesystem is atomic and fails if the destination
    exists; os.replace would violate the bridge's create-only contract.
    Unsupported filesystems fail closed without falling back to replacement.
    """
    descriptor, temporary = tempfile.mkstemp(prefix=".stage-export-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(common.canonical_file_bytes(value))
            stream.flush()
            os.fsync(stream.fileno())
        os.link(temporary, path)
    finally:
        os.unlink(temporary)


def main(argv=None, env=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("export-fixture", "export-target", "import-fixture"))
    parser.add_argument("--private-file", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args(argv)
    env = os.environ if env is None else env
    try:
        contract.validate_environment(env)
        require(not any(key.startswith("CANARY_") or re.match(r"^(WHATOMATE_|META_RELAY_|GMAIL_RELAY_|STUB_)", key) for key in env), "context-invalid:staging-canary-ambient")
        production = common.loads_strict((Path(__file__).parent / "ship-target.json").read_bytes())
        pins = common.loads_strict((Path(__file__).parent / "ship-target-staging.json").read_bytes())
        # Reuse the reviewed owner's Windows DACL / POSIX permissions contract.
        sys.path.insert(0, str(ROOT / "release/staging"))
        import setup
        runner = setup.Runner()
        output = setup.private_path(args.output)
        require(not output.exists(), "input-invalid:stage-output-exists")
        if args.command.startswith("export-"):
            require(args.private_file is not None, CODE)
            source = setup.private_path(args.private_file)
            setup.check_private_state(source, runner)
            require(source.is_file() and source.stat().st_size <= common.MAX_JSON_BYTES and output.parent == source.parent, CODE)
            state = common.loads_strict(source.read_bytes(), code=CODE)
            if args.command == "export-fixture":
                value = export_fixture(state, production["default_ingress_sha256"])
            else:
                template = common.loads_strict(contract.TEMPLATE_PATH.read_bytes())
                value = export_target(state, pins, production, template)
        else:
            require(args.private_file is None, CODE)
            contract.validate_pins(pins, production)
            require(not any(key.startswith("STAGING_") and key not in {"STAGING_ORIGIN", "STAGING_CANARY_FIXTURE_JSON", "STAGING_STUB_CONTROL_KEY"} for key in env), "context-invalid:staging-fixture-inputs")
            value = import_fixture(env.get("STAGING_CANARY_FIXTURE_JSON"), origin=env.get("STAGING_ORIGIN"),
                control_key=env.get("STAGING_STUB_CONTROL_KEY"), receipt_b64=env.get("STAGE_RECEIPT_B64"),
                receipt_sha256=env.get("STAGE_RECEIPT_SHA256"), run_id=env.get("GITHUB_RUN_ID"),
                candidate_sha256=env.get("STAGE_CANDIDATE_SHA256"), app_id_sha256=pins["app_id_sha256"],
                production_origin_sha256=production["default_ingress_sha256"])
            # Fresh directory only: never replace the owner state or a prior run.
            setup.private_directory(output.parent, runner)
        publish_private(output, value)
        setup.check_private_state(output, runner)
        print("stage-fixture: private output written")
        return 0
    except Exception:
        # Provider/OS/JSON/ACL exceptions can contain private values and paths.
        print("stage-fixture: refused", file=sys.stderr)
        return 1


ROOT = Path(__file__).resolve().parents[2]
if __name__ == "__main__":
    sys.exit(main())
