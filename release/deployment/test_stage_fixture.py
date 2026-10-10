"""Synthetic setup-to-canary boundary, including the real private-file contract."""
import base64
import contextlib
import copy
import io
import itertools
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import ship_common as common
import stage_contract as stage
import stage_fixture as bridge
from test_stage_contract import APP, PG, VK, VPC, ORG, ORIGIN, TEAM, IMAGES, data, receipt

NAMESPACE = "rereply-staging-synthetic123"
CONTROL = "synthetic-control-key-never-print-long-enough"
UNEXPORTED = "synthetic-owner-password-never-export"


def has_typescript_node():
    node = shutil.which("node")
    if node is None:
        return False
    try:
        version = subprocess.run([node, "--version"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5)
        major, minor, *_ = map(int, version.stdout.decode("ascii").strip().removeprefix("v").split("."))
        return version.returncode == 0 and (major, minor) >= (22, 6)
    except (OSError, ValueError, subprocess.SubprocessError):
        return False


def fixture():
    def identity(n): return f"{n:08d}-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
    conversations = {key: {"conversation_id": identity(index), "contact_id": identity(index + 2),
        "display_name": NAMESPACE + "-" + key, "sender_wa_id": "99900000000000" + str(index)} for index, key in enumerate(("a", "b"), 1)}
    return {"schema_version": 1, "origin_sha256": common.sha256_text(ORIGIN), "canary": {
        "origin": ORIGIN, "namespace": NAMESPACE, "stub_origin": ORIGIN + "/_stub",
        "stub_app_secret": "synthetic-stub-app-secret", "klinik_organization_id": ORG,
        "fixture": {"descriptor": {"schema_version": 1, "product_origin": ORIGIN, "fixture_namespace": NAMESPACE,
            "klinik": {"organization_id": ORG, "conversations": conversations,
                "meta": {"business_account_id": "999000000000010", "phone_number_id": "999000000000011", "display_phone_number": "999000000000012",
                         "channel_account_id": identity(5), "legacy_account_id": identity(6), "legacy_account_name": NAMESPACE + "-wa"}},
            "non_klinik": {"organization_id": identity(7)}},
            "klinik_login": {"schema_version": 1, "email": NAMESPACE + "-klinik@example.test", "password": "synthetic-klinik-password"},
            "non_klinik_login": {"schema_version": 1, "email": NAMESPACE + "-other@example.test", "password": "synthetic-other-password"}}}}


def setup_state():
    state = fixture()
    state.update(database_verified=True, app_id=APP, deployment_id="77777777-7777-4777-8777-777777777777",
        applied_canary_organization=ORG, target={"schema_version": 1, "team_sha256": common.sha256_text(TEAM), "postgres_id": PG, "valkey_id": VK, "vpc_id": VPC},
        images={"source_sha": "a" * 40, "graph-stub": stage.STAGING_REPOS["graph-stub"] + "@sha256:" + "1" * 64,
                "bootstrap": stage.STAGING_REPOS["bootstrap"] + "@sha256:" + "2" * 64},
        database_private={"doadmin": UNEXPORTED, "rereply_app": UNEXPORTED}, redis_url=UNEXPORTED, secrets={"jwt": UNEXPORTED})
    state["canary"].update(admin_email="staging-admin@rereply.invalid", admin_password=UNEXPORTED,
        stub_control_key=CONTROL, stub_access_token=UNEXPORTED, config_path="private-owner-file",
        stub_app_id="900000000000001", stub_phone_id="900000000000002", stub_waba_id="900000000000003")
    return state


class FixtureTests(unittest.TestCase):
    def setUp(self):
        self.production, self.template, self.pins, self.target = data()

    def args(self, **changes):
        value = receipt()
        raw = stage.receipt_bytes(value)
        result = dict(origin=ORIGIN, control_key=CONTROL, receipt_b64=base64.b64encode(raw).decode(),
            receipt_sha256=common.sha256_bytes(raw), run_id=value["run_id"], candidate_sha256=value["candidate_sha256"],
            app_id_sha256=self.pins["app_id_sha256"], production_origin_sha256=self.production["default_ingress_sha256"])
        result.update(changes)
        return result

    def test_export_selects_only_minimal_fields_and_preserves_source(self):
        state = setup_state(); before = copy.deepcopy(state)
        with mock.patch("socket.create_connection", side_effect=AssertionError("network forbidden")):
            exported = bridge.export_fixture(state, self.production["default_ingress_sha256"])
        self.assertEqual(exported, fixture())
        self.assertEqual(state, before)
        serialized = json.dumps(exported)
        for forbidden in (UNEXPORTED, CONTROL, APP, PG, VK, VPC, "admin_password", "database_private", "config_path", "secrets", "stub_access_token"):
            self.assertNotIn(forbidden, serialized)

    def test_setup_readiness_is_required_before_export(self):
        for mutate in (
            lambda s: s.update(pending_operation="redeploy"), lambda s: s.update(database_verified=False),
            lambda s: s.pop("app_id"), lambda s: s.pop("deployment_id"),
            lambda s: s.update(applied_canary_organization=None),
            lambda s: s["canary"].pop("fixture"),
            lambda s: s["canary"].update(admin_email=s["canary"]["fixture"]["klinik_login"]["email"]),
        ):
            state = setup_state(); mutate(state)
            with self.subTest(mutate=mutate), self.assertRaises(common.ReleaseError):
                bridge.export_fixture(state, self.production["default_ingress_sha256"])

    def test_import_roundtrip_adds_only_control_key_and_has_no_io_capability(self):
        with mock.patch("socket.create_connection", side_effect=AssertionError("network forbidden")):
            actual = bridge.import_fixture(json.dumps(fixture()), **self.args())
        wanted = fixture(); wanted["canary"]["stub_control_key"] = CONTROL
        self.assertEqual(actual, wanted)

    def test_exact_keys_reject_full_owner_state_and_injected_nested_secrets(self):
        cases = [setup_state()]
        mutations = (
            lambda f: f.update(database_private={"doadmin": UNEXPORTED}),
            lambda f: f["canary"].update(admin_password=UNEXPORTED),
            lambda f: f["canary"]["fixture"].update(config_path=UNEXPORTED),
            lambda f: f["canary"]["fixture"]["klinik_login"].update(is_super_admin=True),
            lambda f: f["canary"]["fixture"]["descriptor"]["klinik"]["meta"].update(access_token=UNEXPORTED),
            lambda f: f["canary"].update(stub_control_key=CONTROL),
        )
        for mutate in mutations:
            value = fixture(); mutate(value); cases.append(value)
        for value in cases:
            with self.subTest(keys=list(value)), self.assertRaises(common.ReleaseError) as error:
                bridge.import_fixture(json.dumps(value), **self.args())
            self.assertNotIn(UNEXPORTED, str(error.exception))

    def test_duplicate_json_keys_and_non_integer_schema_are_refused(self):
        raw = json.dumps(fixture()).replace('"schema_version": 1', '"schema_version": 1, "schema_version": 1', 1)
        with self.assertRaises(common.ReleaseError): bridge.import_fixture(raw, **self.args())
        for value in (True, 1.0, "1"):
            changed = fixture(); changed["schema_version"] = value
            with self.assertRaises(common.ReleaseError): bridge.import_fixture(json.dumps(changed), **self.args())

    def test_origins_receipts_and_all_synthetic_identities_bind_before_import(self):
        mutations = (
            lambda f: f.update(origin_sha256="0" * 64),
            lambda f: f["canary"].update(stub_origin="https://wrong.invalid/_stub"),
            lambda f: f["canary"].update(namespace="rereply-canary"),
            lambda f: f["canary"]["fixture"]["descriptor"].update(product_origin="https://rereply.app"),
            lambda f: f["canary"]["fixture"]["descriptor"]["non_klinik"].update(organization_id=ORG),
            lambda f: f["canary"]["fixture"]["descriptor"]["klinik"]["conversations"].update(b=copy.deepcopy(f["canary"]["fixture"]["descriptor"]["klinik"]["conversations"]["a"])),
            lambda f: f["canary"]["fixture"]["klinik_login"].update(email="staging-admin@rereply.invalid"),
            lambda f: f["canary"]["fixture"]["non_klinik_login"].update(password=f["canary"]["fixture"]["klinik_login"]["password"]),
        )
        for mutate in mutations:
            value = fixture(); mutate(value)
            with self.subTest(mutate=mutate), self.assertRaises(common.ReleaseError):
                bridge.import_fixture(json.dumps(value), **self.args())
        for change in ({"origin": ORIGIN.replace("synthetic", "other")}, {"control_key": "short"}, {"run_id": "99"}, {"candidate_sha256": "f" * 64}):
            with self.subTest(change=list(change)), self.assertRaises(common.ReleaseError):
                bridge.import_fixture(json.dumps(fixture()), **self.args(**change))

    def test_target_export_contains_no_canary_login_or_infrastructure_password(self):
        actual = bridge.export_target(setup_state(), self.pins, self.production, self.template)
        self.assertEqual(actual, self.target)
        raw = json.dumps(actual)
        for secret in (UNEXPORTED, CONTROL, "synthetic-klinik-password", "synthetic-other-password", "synthetic-stub-app-secret"):
            self.assertNotIn(secret, raw)
        with self.assertRaises(common.ReleaseError):
            bridge.export_target(setup_state(), {**self.pins, "app_id_sha256": None}, self.production, self.template)

    def test_setup_spec_matches_canonical_target_export_and_every_account_key_order(self):
        sys.path.insert(0, str(stage.ROOT / "release/staging"))
        import setup
        state = setup_state()
        state["secrets"] = {key: UNEXPORTED for key in setup.SECRET_NAMES}
        state["product"] = {"images": IMAGES.copy()}
        kit = setup.Setup.__new__(setup.Setup)  # Pure renderer: no config, private file or provider.
        kit.state, kit.target, kit.images = state, state["target"], state["images"]
        kit.template = self.template
        kit.clusters = {"postgres_id": {"name": "rereply-staging-pg"}}
        with mock.patch("socket.create_connection", side_effect=AssertionError("network forbidden")):
            actual = kit.build_spec()
            exported = bridge.export_target(state, self.pins, self.production, self.template)
            loaded = common.loads_strict(common.canonical_file_bytes(exported))
        before, fingerprint = copy.deepcopy(actual), stage.spec_fingerprint(actual)
        original = exported["template_values"]["stub_accounts"][0]
        self.assertNotEqual(list(original), list(loaded["template_values"]["stub_accounts"][0]))
        for keys in itertools.permutations(original):
            target = copy.deepcopy(loaded)
            target["template_values"]["stub_accounts"][0] = {key: original[key] for key in keys}
            with self.subTest(order=keys):
                stage.validate_target(target, self.pins, self.production, self.template)
                stage.validate_spec(actual, target, self.template, IMAGES)
        self.assertEqual(actual, before)
        self.assertEqual(stage.spec_fingerprint(actual), fingerprint)

    def test_stub_account_target_fields_and_types_remain_exact_after_export(self):
        target = common.loads_strict(common.canonical_file_bytes(
            bridge.export_target(setup_state(), self.pins, self.production, self.template)))
        mutations = (
            lambda a: a.pop("phone_number_id"), lambda a: a.update(extra="unexpected"),
            lambda a: a.update(phone_number_id=900000000000002),
            lambda a: a.update(business_account_id=None),
            lambda a: a.update(display_phone_number="not-a-staging-number"),
        )
        for mutate in mutations:
            changed = copy.deepcopy(target); mutate(changed["template_values"]["stub_accounts"][0])
            with self.subTest(mutate=mutate), self.assertRaises(common.ReleaseError):
                stage.validate_target(changed, self.pins, self.production, self.template)
        raw = common.canonical_file_bytes(target).replace(b'"business_account_id":', b'"business_account_id":"900000000000003","business_account_id":', 1)
        with self.assertRaises(common.ReleaseError): common.loads_strict(raw)

    def test_actual_stub_json_and_other_general_values_keep_strict_comparison(self):
        target = common.loads_strict(common.canonical_file_bytes(
            bridge.export_target(setup_state(), self.pins, self.production, self.template)))
        spec = stage.expected_spec(target, self.template, IMAGES)
        def env(value, component, key):
            return next(item for service in value["services"] if service["name"] == component
                        for item in service["envs"] if item["key"] == key)
        original = env(spec, "graph-stub", "STUB_ACCOUNTS")["value"]
        account = json.loads(original)[0]
        values = ("not-json", "null", "[]", "{}", None, False, 1,
                  original + " ", json.dumps([account], sort_keys=True),
                  json.dumps([account], separators=(",", ":")),
                  original.replace('"phone_number_id":', '"phone_number_id": "900000000000099", "phone_number_id":'),
                  json.dumps([{**account, "phone_number_id": "900000000000099"}]),
                  json.dumps([{**account, "phone_number_id": 900000000000002}]),
                  json.dumps([{**account, "unexpected": "value"}]))
        for value in values:
            actual = copy.deepcopy(spec)
            env(actual, "graph-stub", "STUB_ACCOUNTS")["value"] = value
            with self.subTest(value=value), self.assertRaises(common.ReleaseError):
                stage.validate_spec(actual, target, self.template, IMAGES)
        actual = copy.deepcopy(spec)
        env(actual, "meta-relay", "META_RELAY_ACCOUNTS_JSON")["value"] += " "
        with self.assertRaises(common.ReleaseError): stage.validate_spec(actual, target, self.template, IMAGES)

    def test_bad_image_and_health_drills_cannot_start_browser_fixture(self):
        for drill in ("health-fail", "bad-image"):
            raw = stage.receipt_bytes(receipt(drill=drill))
            with self.assertRaises(common.ReleaseError):
                bridge.import_fixture(json.dumps(fixture()), **self.args(receipt_b64=base64.b64encode(raw).decode(), receipt_sha256=common.sha256_bytes(raw)))

    def test_stub_secret_boundaries_match_pr7_and_graph_stub(self):
        for key in ("", "eightkey", "a" * 31, "a" * 257):
            with self.subTest(length=len(key)), self.assertRaises(common.ReleaseError):
                bridge.import_fixture(json.dumps(fixture()), **self.args(control_key=key))
        for key in ("a" * 32, "a" * 256):
            bridge.import_fixture(json.dumps(fixture()), **self.args(control_key=key))
        for key in ("a" * 8, "a" * 15, "a" * 257):
            value = fixture(); value["canary"]["stub_app_secret"] = key
            with self.subTest(length=len(key)), self.assertRaises(common.ReleaseError):
                bridge.import_fixture(json.dumps(value), **self.args())
        value = fixture(); value["canary"]["stub_app_secret"] = "a" * 16
        bridge.import_fixture(json.dumps(value), **self.args())

    def test_private_publication_never_clobbers_a_concurrent_destination(self):
        sys.path.insert(0, str(stage.ROOT / "release/staging"))
        import setup
        with tempfile.TemporaryDirectory() as folder:
            directory = Path(folder) / "rereply-staging"
            setup.private_directory(directory, setup.Runner())
            source, output = directory / "state.json", directory / "fixture.json"
            setup.save_private(source, setup_state())
            link = os.link
            def competing_file(temporary, destination):
                Path(destination).write_bytes(b"peer-owned-content")
                return link(temporary, destination)
            logs = io.StringIO()
            with mock.patch.object(bridge.os, "link", side_effect=competing_file), contextlib.redirect_stdout(logs), contextlib.redirect_stderr(logs):
                self.assertEqual(bridge.main(["export-fixture", "--private-file", str(source), "--output", str(output)], env={}), 1)
            self.assertEqual(output.read_bytes(), b"peer-owned-content")
            self.assertEqual(list(directory.glob(".stage-export-*")), [])
            self.assertEqual(logs.getvalue(), "stage-fixture: refused\n")

    def test_private_cli_export_redacts_output_and_does_not_overwrite(self):
        sys.path.insert(0, str(stage.ROOT / "release/staging"))
        import setup
        with tempfile.TemporaryDirectory() as folder:
            directory = Path(folder) / "rereply-staging"
            setup.private_directory(directory, setup.Runner())
            source = directory / "state.json"
            setup.save_private(source, setup_state())
            source_bytes = source.read_bytes()
            output = directory / "fixture.json"
            logs = io.StringIO()
            with contextlib.redirect_stdout(logs), contextlib.redirect_stderr(logs):
                self.assertEqual(bridge.main(["export-fixture", "--private-file", str(source), "--output", str(output)], env={}), 0)
                self.assertEqual(bridge.main(["export-fixture", "--private-file", str(source), "--output", str(output)], env={}), 1)
                self.assertEqual(bridge.main(["export-fixture", "--private-file", str(source), "--output", str(source)], env={}), 1)
            self.assertEqual(source.read_bytes(), source_bytes)
            self.assertEqual(common.loads_strict(output.read_bytes()), fixture())
            self.assertNotIn(UNEXPORTED, logs.getvalue())
            self.assertNotIn(str(source), logs.getvalue())
            setup.check_private_state(output, setup.Runner())

    @unittest.skipUnless(has_typescript_node(), "Node 22.6+ TypeScript stripping needed for optional PR7 interop")
    def test_import_is_consumed_by_actual_pr7_profile_and_scenario(self):
        sys.path.insert(0, str(stage.ROOT / "release/staging"))
        import setup
        with tempfile.TemporaryDirectory() as folder:
            directory = Path(folder) / "rereply-staging"
            setup.private_directory(directory, setup.Runner())
            private = directory / "canary.json"
            setup.save_private(private, bridge.import_fixture(json.dumps(fixture()), **self.args()))
            # Constructor validates the descriptor/logins and registers no
            # network capability until prepare(); browser is never launched.
            profiles = (stage.ROOT / "frontend/e2e/canary/profiles.ts").as_uri()
            scenario = (stage.ROOT / "frontend/e2e/canary/scenario.ts").as_uri()
            script = f"import {{loadProfile}} from {json.dumps(profiles)}; import {{createScenario}} from {json.dumps(scenario)}; createScenario(undefined, loadProfile()); console.log('interop: passed');"
            env = {key: value for key, value in os.environ.items() if not key.startswith(("CANARY_", "WHATOMATE_", "META_RELAY_", "GMAIL_RELAY_", "STUB_"))}
            env.update(CANARY_PROFILE="staging", CANARY_PRIVATE_FILE=str(private))
            process = subprocess.run([shutil.which("node"), "--experimental-strip-types", "--input-type=module", "-e", script], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
            # Never include captured error output, which could contain inputs.
            self.assertEqual(process.returncode, 0, "PR7 private profile/scenario rejected the sanitized fixture")
            self.assertEqual(process.stdout, b"interop: passed\n")
            setup.check_private_state(private, setup.Runner())


if __name__ == "__main__":
    unittest.main()
