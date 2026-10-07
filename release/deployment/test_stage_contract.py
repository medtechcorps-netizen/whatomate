"""Offline stage contracts. Synthetic provider shapes, no network or containers."""
import base64
import copy
import json
from pathlib import Path
import unittest

import ship_common as common
import spec_images
import stage_contract as stage

APP = "44444444-4444-4444-8444-444444444444"
PG = "11111111-1111-4111-8111-111111111111"
VK = "22222222-2222-4222-8222-222222222222"
VPC = "33333333-3333-4333-8333-333333333333"
ORG = "66666666-6666-4666-8666-666666666666"
ORIGIN = "https://synthetic-staging.ondigitalocean.app"
TEAM = "opaque-team-value-with-case-Sensitive"
IMAGES = {component: "sha256:" + letter * 64 for component, letter in zip(common.COMPONENTS, "abc")}
NEW = {component: "sha256:" + letter * 64 for component, letter in zip(common.COMPONENTS, "def")}


def data():
    production = common.loads_strict((Path(__file__).parent / "ship-target.json").read_bytes())
    template = common.loads_strict(stage.TEMPLATE_PATH.read_bytes())
    pins = {"schema_version": 1, "profile": "staging", "team_uuid_sha256": common.sha256_text(TEAM), "app_id_sha256": common.sha256_text(APP)}
    target = {"schema_version": 1, "profile": "staging", "app_id": APP, "origin": ORIGIN,
              "postgres_id": PG, "valkey_id": VK, "vpc_id": VPC, "postgres_name": "rereply-staging-pg",
              "graph_stub": {"image": stage.STAGING_REPOS["graph-stub"] + "@sha256:" + "1" * 64, "source_sha": "a" * 40},
              "bootstrap": {"image": stage.STAGING_REPOS["bootstrap"] + "@sha256:" + "2" * 64, "source_sha": "a" * 40},
              "template_sha256": common.sha256_value(template), "template_values": {"admin_email": "staging-admin@rereply.invalid", "stub_app_id": "900000000000001",
              "stub_accounts": [{"business_account_id": "900000000000003", "phone_number_id": "900000000000002", "display_phone_number": "+15555550101"}], "canary_organization": ORG}}
    return production, template, pins, target


def receipt(**changes):
    value = {"schema_version": 1, "profile": "staging", "run_id": "1234567", "candidate_sha256": "7" * 64,
        "ingress_sha256": common.sha256_text(ORIGIN), "app_id_sha256": common.sha256_text(APP), "drill": "none",
        "previous_images": IMAGES.copy(), "candidate_images": NEW.copy(), "before_spec_sha256": "8" * 64,
        "after_spec_sha256": "9" * 64, "before_deployment_sha256": "a" * 64, "candidate_deployment_sha256": "b" * 64,
        "previous_source_sha": "a" * 40, "candidate_source_sha": "b" * 40}
    value.update(changes)
    return value


class ContractTests(unittest.TestCase):
    def setUp(self):
        self.production, self.template, self.pins, self.target = data()
        self.spec = stage.expected_spec(self.target, self.template, IMAGES)

    def check_target(self, value=None):
        return stage.validate_target(self.target if value is None else value, self.pins, self.production, self.template)

    def test_committed_target_is_explicitly_unconfigured(self):
        pins = common.loads_strict((Path(__file__).parent / "ship-target-staging.json").read_bytes())
        with self.assertRaisesRegex(common.ReleaseError, "staging-unconfigured"):
            stage.validate_pins(pins, self.production)

    def test_target_exact_shape_and_local_identity(self):
        self.assertEqual(self.check_target(), self.target)
        for mutate in (
            lambda x: x.update(app_id=PG), lambda x: x.update(profile="production"),
            lambda x: x.update(unknown="sensitive"), lambda x: x.update(schema_version=True),
            lambda x: x.update(template_sha256="0" * 64), lambda x: x.update(valkey_id=PG),
            lambda x: x["template_values"].update(admin_email="real@example.com"),
            lambda x: x["graph_stub"].update(source_sha="main"),
            lambda x: x["bootstrap"].update(image=x["graph_stub"]["image"]),
        ):
            value = copy.deepcopy(self.target)
            mutate(value)
            with self.subTest(mutate=mutate), self.assertRaises(common.ReleaseError):
                self.check_target(value)

    def test_canonical_stage_origins_and_production_exclusion(self):
        self.assertEqual(stage.stage_origin(ORIGIN, self.production["default_ingress_sha256"]), ORIGIN)
        for value in (ORIGIN + "/", ORIGIN + ":443", ORIGIN + "/path", ORIGIN + "?", ORIGIN.replace("https", "http"),
                      "https://rereply.app", "https://localhost", "https://user@synthetic-staging.ondigitalocean.app",
                      "https://Synthetic-staging.ondigitalocean.app", "https://synthetic-staging.ondigitalocean.app.evil.invalid"):
            with self.subTest(origin=value), self.assertRaises(common.ReleaseError):
                stage.stage_origin(value, self.production["default_ingress_sha256"])
        with self.assertRaises(common.ReleaseError):
            stage.stage_origin(ORIGIN, common.sha256_text(ORIGIN))

    def test_ambient_presence_and_drill_allowlists(self):
        provider_inputs = {"STAGING_DO_TOKEN", "STAGING_TARGET_JSON"}
        forbidden = set(common.FORBIDDEN_AMBIENT) - provider_inputs
        for name in forbidden | {"SHIP_DO_TOKEN", "SHIP_TARGET_JSON", "DOCTL_CONFIG", "DOCTL_CONTEXT"}:
            with self.subTest(name=name), self.assertRaises(common.ReleaseError):
                stage.validate_environment({name: ""})
        stage.validate_environment({"STAGING_DO_TOKEN": "synthetic", "STAGING_TARGET_JSON": "{}"})
        stage.validate_environment({name: "" for name in provider_inputs})
        for mode in ("dry-run", "promote", "rollback"):
            stage.validate_drill(mode, "none")
            for drill in stage.DRILLS - {"none"}:
                with self.subTest(mode=mode, drill=drill), self.assertRaises(common.ReleaseError):
                    stage.validate_drill(mode, drill)
        for drill in stage.DRILLS:
            stage.validate_drill("stage", drill)

    def inventory(self):
        return {"account": {"status": "active", "team": {"name": "ReReply Staging", "uuid": TEAM}},
            "apps": [{"id": APP}], "clusters": [
            {"id": PG, "name": "rereply-staging-pg", "engine": "pg", "version": "17", "status": "online", "region": "sgp1", "private_network_uuid": VPC},
            {"id": VK, "name": "rereply-staging-valkey", "engine": "valkey", "status": "online", "region": "sgp1", "private_network_uuid": VPC}],
            "firewalls": {key: [{"type": "app", "value": APP}] for key in (PG, VK)}}

    def check_inventory(self, value):
        stage.validate_inventory(**value, target=self.target, pins=self.pins, production=self.production)

    def test_inventory_opaque_team_dual_clusters_and_exact_firewalls(self):
        self.check_inventory(self.inventory())
        mutations = [lambda x: x["account"]["team"].update(uuid=TEAM.lower()), lambda x: x["apps"].append({"id": PG}),
            lambda x: x["clusters"][0].update(version="18"), lambda x: x["clusters"][1].update(private_network_uuid=PG),
            lambda x: x["clusters"].append(x["clusters"][0]), lambda x: x["firewalls"][PG].clear(),
            lambda x: x["firewalls"][PG].append({"type": "ip_addr", "value": "0.0.0.0/0"}),
            lambda x: x["firewalls"][VK][0].update(value=PG)]
        for mutate in mutations:
            value = self.inventory(); mutate(value)
            with self.subTest(mutate=mutate), self.assertRaises(common.ReleaseError): self.check_inventory(value)

    def test_visible_production_is_refused_after_inventory_read(self):
        value = self.inventory()
        synthetic_production = copy.deepcopy(self.production)
        synthetic_production["app_id_sha256"] = common.sha256_text(PG)
        value["apps"].append({"id": PG})
        with self.assertRaisesRegex(common.ReleaseError, "production-app-visible"):
            stage.validate_inventory(**value, target=self.target, pins=self.pins, production=synthetic_production)
        # This module has no HTTP/client capability, so even a failed check
        # cannot write. The future caller must test GET pagination separately.

    def test_full_template_and_narrow_provider_defaults(self):
        stage.validate_spec(self.spec, self.target, self.template, IMAGES)
        value = copy.deepcopy(self.spec)
        value["domains"] = []
        for component in value["services"]:
            component["protocol"] = "HTTP"
            component["run_command"] = ""
            component["image"].pop("deploy_on_push")
            for env in component["envs"]:
                if env["type"] == "SECRET": env["value"] = "EV[synthetic-encrypted-value]"
                elif env["type"] == "GENERAL": env.pop("type")
        stage.validate_spec(value, self.target, self.template, IMAGES)

    def test_topology_adversarial_mutations(self):
        mutations = {
            "duplicate": lambda s: s["services"].append(copy.deepcopy(s["services"][0])),
            "missing-stub": lambda s: s["services"].pop(),
            "custom-domain": lambda s: s.update(domains=[{"domain": "example.invalid"}]),
            "git-even-empty": lambda s: s["services"][0].update(git={}),
            "unknown-image": lambda s: s["services"][0]["image"].update(tag="latest"),
            "autodeploy": lambda s: s["services"][0]["image"].update(deploy_on_push={"enabled": True}),
            "autodeploy-wrong-type": lambda s: s["services"][0]["image"].update(deploy_on_push={"enabled": 0}),
            "double-env": lambda s: s["services"][0]["envs"].append(copy.deepcopy(s["services"][0]["envs"][0])),
            "new-env": lambda s: s["services"][0]["envs"].append({"key": "EXFILTRATE", "value": "x"}),
            "env-value": lambda s: s["services"][0]["envs"][0].update(value="production"),
            "secret-type": lambda s: s["services"][0]["envs"][4].update(type="GENERAL"),
            "secret-empty": lambda s: s["services"][0]["envs"][4].update(value=""),
            "job-command": lambda s: s["jobs"][0].update(run_command="./rereply"),
            "job-kind": lambda s: s["jobs"][0].update(kind="POST_DEPLOY"),
            "binding-user": lambda s: s["databases"][0].update(db_user="doadmin"),
            "binding-cluster": lambda s: s["databases"][1].update(cluster_name="other"),
            "binding-version": lambda s: s["databases"][0].update(version="16"),
            "route": lambda s: s["ingress"]["rules"][0]["component"].update(rewrite="/"),
            "extra-route": lambda s: s["ingress"]["rules"].append(s["ingress"]["rules"][0]),
            "vpc": lambda s: s["vpc"].update(id=PG),
            "region": lambda s: s.update(region="nyc"),
            "size": lambda s: s["services"][0].update(instance_count=2),
            "size-boolean": lambda s: s["services"][0].update(instance_count=True),
            "port": lambda s: s["services"][0].update(http_port=80),
            "probe": lambda s: s["services"][0]["health_check"].update(http_path="/health"),
            "logs": lambda s: s["services"][0].update(log_destinations=[]),
        }
        for name, mutate in mutations.items():
            value = copy.deepcopy(self.spec); mutate(value)
            with self.subTest(name=name), self.assertRaises(common.ReleaseError):
                stage.validate_spec(value, self.target, self.template, IMAGES)

    def test_doctl_empty_general_and_disabled_image_defaults_preserve_fingerprint(self):
        value = copy.deepcopy(self.spec)
        for component in value["services"] + value["jobs"]:
            component["image"]["deploy_on_push"] = {}
            for env in component["envs"]:
                if env["type"] == "GENERAL" and env["value"] == "":
                    del env["value"]
                    if component["name"] == "rereply-rls-migrate": del env["type"]
        stage.validate_spec(value, self.target, self.template, IMAGES)
        self.assertEqual(stage.spec_fingerprint(value), stage.spec_fingerprint(self.spec))
        self.assertEqual(stage.projection(value, private_values=True), stage.projection(self.spec, private_values=True))

    def test_empty_general_equivalence_keeps_null_types_and_secret_checks(self):
        for group in ("services", "jobs"):
            for omit_type in (False, True):
                for replacement in (None, False, 0, [], {}, "different"):
                    value = copy.deepcopy(self.spec)
                    env = next(item for item in value[group][0]["envs"] if item["type"] == "GENERAL" and item["value"] == "")
                    env["value"] = replacement
                    if omit_type: del env["type"]
                    with self.subTest(group=group, omit_type=omit_type, value=replacement), self.assertRaises(common.ReleaseError):
                        stage.validate_spec(value, self.target, self.template, IMAGES)
                value = copy.deepcopy(self.spec)
                env = next(item for item in value[group][0]["envs"] if item["type"] == "GENERAL" and item["value"])
                del env["value"]
                if omit_type: del env["type"]
                with self.subTest(group=group, missing_nonempty=True, omit_type=omit_type), self.assertRaises(common.ReleaseError):
                    stage.validate_spec(value, self.target, self.template, IMAGES)
            for missing in (False, True):
                value = copy.deepcopy(self.spec)
                env = next(item for item in value[group][0]["envs"] if item["type"] == "SECRET")
                if missing: del env["value"]
                else: env["value"] = None
                with self.subTest(group=group, secret_missing=missing), self.assertRaises(common.ReleaseError):
                    stage.validate_spec(value, self.target, self.template, IMAGES)

    def test_disabled_image_equivalence_rejects_other_shapes(self):
        for group in ("services", "jobs"):
            for replacement in (None, False, 0, [], "", {"enabled": True}, {"enabled": None},
                                {"enabled": 0}, {"other": False}, {"enabled": False, "other": False}):
                value = copy.deepcopy(self.spec)
                value[group][0]["image"]["deploy_on_push"] = replacement
                with self.subTest(group=group, value=replacement), self.assertRaises(common.ReleaseError):
                    stage.validate_spec(value, self.target, self.template, IMAGES)

    def test_image_transform_preserves_secrets_and_only_four_digest_leaves(self):
        original = copy.deepcopy(self.spec)
        changed = stage.set_images(original, NEW)
        self.assertEqual(stage.image_set(changed), NEW)
        self.assertEqual(original, self.spec)
        pointers = spec_images.changed_leaf_pointers(original, changed)
        self.assertEqual(len(pointers), 4)
        self.assertTrue(all(pointer.endswith("/image/digest") for pointer in pointers))
        self.assertEqual(changed["services"][-1], original["services"][-1])
        with self.assertRaises(common.ReleaseError): stage.set_images(original, IMAGES)

    def test_wrong_repository_and_job_digest_refused(self):
        for mutate in (lambda s: s["jobs"][0]["image"].update(digest=NEW["web"]),
                       lambda s: s["services"][0]["image"].update(repository="wrong/repository")):
            value = copy.deepcopy(self.spec); mutate(value)
            with self.assertRaises(common.ReleaseError): stage.set_images(value, NEW)

    def test_bad_image_drill_changes_web_and_job_full_selectors_only(self):
        changed = stage.bad_image_spec(self.spec, self.target, mode="stage", drill="bad-image")
        self.assertEqual(changed["services"][0]["image"], stage.selector(self.target["bootstrap"]["image"]))
        self.assertEqual(changed["jobs"][0]["image"], changed["services"][0]["image"])
        self.assertEqual(changed["services"][1:], self.spec["services"][1:])
        with self.assertRaises(common.ReleaseError): stage.bad_image_spec(self.spec, self.target, mode="promote", drill="bad-image")

    def test_cas_fingerprint_includes_private_values_and_non_source_drift(self):
        before = stage.spec_fingerprint(self.spec)
        value = copy.deepcopy(self.spec)
        value["services"][0]["envs"][4]["value"] = "EV[changed]"
        self.assertNotEqual(before, stage.spec_fingerprint(value))
        self.assertEqual(stage.projection(self.spec), stage.projection(value))

    def test_receipt_canonical_hash_bindings_and_no_raw_provider_values(self):
        value = receipt(); raw = stage.receipt_bytes(value)
        args = dict(run_id=value["run_id"], candidate_sha256=value["candidate_sha256"], origin=ORIGIN,
            production_origin_sha256=self.production["default_ingress_sha256"], app_id_sha256=self.pins["app_id_sha256"])
        encoded = base64.b64encode(raw).decode()
        self.assertEqual(stage.decode_receipt(encoded, common.sha256_bytes(raw), **args), value)
        for sensitive in (APP, PG, VK, VPC, ORIGIN, TEAM, "EV["):
            self.assertNotIn(sensitive.encode(), raw)
        for key, wrong in (("run_id", "1234568"), ("candidate_sha256", "f" * 64), ("app_id_sha256", "f" * 64), ("origin", ORIGIN.replace("synthetic", "other"))):
            with self.subTest(key=key), self.assertRaises(common.ReleaseError):
                stage.decode_receipt(encoded, common.sha256_bytes(raw), **{**args, key: wrong})
        pretty = json.dumps(value).encode()
        with self.assertRaises(common.ReleaseError):
            stage.decode_receipt(base64.b64encode(pretty).decode(), common.sha256_bytes(pretty), **args)
        for wrong in (receipt(app_id=APP), receipt(drill="promote"), receipt(schema_version=True), receipt(candidate_deployment_sha256="a" * 64)):
            with self.assertRaises(common.ReleaseError): stage.receipt_bytes(wrong)


if __name__ == "__main__":
    unittest.main()
