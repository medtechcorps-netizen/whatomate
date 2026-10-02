from __future__ import annotations

import copy
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common
import spec_images
import test_ship_support as support


class ExtractTests(unittest.TestCase):
    def test_four_bindings_and_the_job_follows_web(self) -> None:
        self.assertEqual(spec_images.extract_image_digests(support.make_spec()), support.LIVE)
        spec = support.make_spec()
        spec["jobs"][0]["image"]["digest"] = support.digest("9")
        with self.assertRaisesRegex(common.ReleaseError, "^topology-differs:migration-web-digest$"):
            spec_images.extract_image_digests(spec)

    def test_forbidden_image_fields_are_refused(self) -> None:
        for field, value in (("tag", "latest"), ("deploy_on_push", {"enabled": True}),
                             ("registry_credentials", "user:pass")):
            for collection, index in (("services", 0), ("services", 2), ("jobs", 0)):
                spec = support.make_spec()
                spec[collection][index]["image"][field] = value
                with self.subTest(field=field, collection=collection, index=index):
                    with self.assertRaisesRegex(common.ReleaseError, "^forbidden-image-field$"):
                        spec_images.extract_image_digests(spec)

    def test_legacy_and_ambiguous_sources_are_refused_never_converted(self) -> None:
        mutations = {
            "git": lambda item: item.update({"git": {"repo_clone_url": "https://example.invalid/x.git", "branch": "main"}}),
            "github": lambda item: item.update({"github": {"repo": "x/y", "branch": "main"}}),
            "dockerfile": lambda item: item.update({"dockerfile_path": "docker/Dockerfile"}),
            "git-only": lambda item: (item.pop("image"), item.update({"git": {"branch": "main"}})),
        }
        for label, mutate in mutations.items():
            spec = support.make_spec()
            mutate(spec["services"][1])
            with self.subTest(case=label), self.assertRaisesRegex(common.ReleaseError, "^topology-differs:source-mode$"):
                spec_images.extract_image_digests(spec)

    def test_repository_and_selector_shape(self) -> None:
        spec = support.make_spec()
        spec["services"][0]["image"]["repository"] = "medtechcorps-netizen/rereply-release-meta-relay"
        with self.assertRaisesRegex(common.ReleaseError, "^topology-differs:image-repository$"):
            spec_images.extract_image_digests(spec)
        spec = support.make_spec()
        spec["services"][0]["image"]["extra"] = "x"
        with self.assertRaisesRegex(common.ReleaseError, "^topology-differs:image-selector$"):
            spec_images.extract_image_digests(spec)
        spec = support.make_spec()
        spec["services"] = spec["services"][:2]
        with self.assertRaisesRegex(common.ReleaseError, "^topology-differs:component-missing$"):
            spec_images.extract_image_digests(spec)


class SetImagesTests(unittest.TestCase):
    def test_only_the_four_digest_leaves_change(self) -> None:
        before = support.make_spec()
        after = spec_images.set_images(before, support.NEW)
        self.assertEqual(spec_images.extract_image_digests(after), support.NEW)
        self.assertEqual(before, support.make_spec())
        changed = spec_images.changed_leaf_pointers(before, after)
        self.assertEqual(changed, sorted(spec_images.image_digest_pointers(before)))
        self.assertEqual(len(changed), 4)

    def test_partial_change_is_allowed_and_empty_change_is_refused(self) -> None:
        before = support.make_spec()
        partial = dict(support.LIVE, **{"meta-relay": support.NEW["meta-relay"]})
        after = spec_images.set_images(before, partial)
        self.assertEqual(spec_images.require_image_only_change(before, after), ["/services/1/image/digest"])
        with self.assertRaisesRegex(common.ReleaseError, "^topology-differs:no-image-change$"):
            spec_images.set_images(before, support.LIVE)

    def test_any_other_change_or_reorder_fails(self) -> None:
        before = support.make_spec()
        after = spec_images.set_images(before, support.NEW)
        for label, mutate in {
            "env": lambda spec: spec["envs"].append({"key": "NEW", "value": "x"}),
            "instance": lambda spec: spec["services"][0].update({"instance_count": 2}),
            "vpc": lambda spec: spec.pop("vpc"),
            "registry": lambda spec: spec["services"][0]["image"].update({"registry": "docker.io"}),
        }.items():
            changed = copy.deepcopy(after)
            mutate(changed)
            with self.subTest(case=label), self.assertRaisesRegex(common.ReleaseError, "^topology-differs:outside-image-digests$"):
                spec_images.require_image_only_change(before, changed)
        reordered = copy.deepcopy(after)
        reordered["services"] = [reordered["services"][1], reordered["services"][0], reordered["services"][2]]
        with self.assertRaisesRegex(common.ReleaseError, "^topology-differs:component-order$"):
            spec_images.require_image_only_change(before, reordered)

    def test_fingerprints_are_stable_across_a_digest_change(self) -> None:
        before = support.make_spec()
        after = spec_images.set_images(before, support.NEW)
        self.assertEqual(spec_images.environment_value_fingerprint(before), spec_images.environment_value_fingerprint(after))
        self.assertEqual(spec_images.non_source_fingerprint(before), spec_images.non_source_fingerprint(after))
        changed = copy.deepcopy(after)
        changed["envs"][1]["value"] = "other"
        self.assertNotEqual(spec_images.environment_value_fingerprint(before), spec_images.environment_value_fingerprint(changed))
        self.assertNotEqual(spec_images.non_source_fingerprint(before), spec_images.non_source_fingerprint(changed))

    def test_environment_fingerprint_refuses_unreviewed_shapes(self) -> None:
        for label, mutate in {
            "scope": lambda spec: spec["envs"][0].update({"scope": "BUILD_TIME"}),
            "type": lambda spec: spec["envs"][0].update({"type": "OTHER"}),
            "duplicate": lambda spec: spec["envs"].append(dict(spec["envs"][0])),
            "missing-value": lambda spec: spec["envs"][1].pop("value"),
        }.items():
            spec = support.make_spec()
            mutate(spec)
            with self.subTest(case=label), self.assertRaisesRegex(common.ReleaseError, "^topology-differs:environment"):
                spec_images.environment_value_fingerprint(spec)


class TopologyTests(unittest.TestCase):
    def setUp(self) -> None:
        self.target = spec_images.validate_target(support.make_target())

    def test_valid_topology_returns_the_cluster_name(self) -> None:
        self.assertEqual(spec_images.require_topology(support.make_spec(), self.target), support.CLUSTER_NAME)

    def test_vpc_guard(self) -> None:
        spec = support.make_spec()
        spec.pop("vpc")
        with self.assertRaisesRegex(common.ReleaseError, "^vpc-missing-or-differs$"):
            spec_images.require_topology(spec, self.target)
        for vpc in ({"id": "55555555-5555-4555-8555-000000000000"}, {}, "x", {"id": 3}, {"id": ""}):
            spec = support.make_spec()
            spec["vpc"] = vpc
            with self.subTest(vpc=vpc), self.assertRaisesRegex(common.ReleaseError, "^vpc-missing-or-differs$"):
                spec_images.require_topology(spec, self.target)

    def test_topology_refusals(self) -> None:
        def worker(spec: dict) -> None:
            spec["workers"] = [{"name": "w"}]

        cases = {
            "name": (lambda spec: spec.update({"name": "other"}), "name"),
            "region": (lambda spec: spec.update({"region": "nyc"}), "region"),
            "worker": (worker, "unexpected-component"),
            "static": (lambda spec: spec.update({"static_sites": [{"name": "s"}]}), "unexpected-component"),
            "extra-service": (lambda spec: spec["services"].append(dict(spec["services"][1], name="extra")), "services"),
            "extra-job": (lambda spec: spec["jobs"].append(dict(spec["jobs"][0], name="job-2")), "jobs"),
            "port": (lambda spec: spec["services"][1].update({"http_port": 9000}), "http-port"),
            "health": (lambda spec: spec["services"][0]["health_check"].update({"http_path": "/health"}), "health-path"),
            "job-kind": (lambda spec: spec["jobs"][0].update({"kind": "POST_DEPLOY"}), "job-kind"),
            "run-command": (lambda spec: spec["jobs"][0].update({"run_command": "./rereply rls-migrate -rollback"}), "job-run-command"),
            "pg-count": (lambda spec: spec["databases"].pop(0), "postgres-bindings"),
            "pg-split": (lambda spec: spec["databases"][1].update({"cluster_name": "other-cluster"}), "postgres-bindings"),
            "pg-version": (lambda spec: [item.update({"version": "16"}) for item in spec["databases"][:2]], "postgres-bindings"),
            "pg-hash": (lambda spec: [item.update({"cluster_name": "renamed"}) for item in spec["databases"][:2]], "postgres-cluster"),
        }
        for label, (mutate, detail) in cases.items():
            spec = support.make_spec()
            mutate(spec)
            with self.subTest(case=label), self.assertRaisesRegex(common.ReleaseError, f"^topology-differs:{detail}$"):
                spec_images.require_topology(spec, self.target)

    def test_target_file_validation(self) -> None:
        target = support.make_target()
        target["extra"] = 1
        with self.assertRaisesRegex(common.ReleaseError, "^target-invalid"):
            spec_images.validate_target(target)
        target = support.make_target()
        target["pre_deploy_job"]["name"] = "other"
        with self.assertRaisesRegex(common.ReleaseError, "^target-invalid"):
            spec_images.validate_target(target)


if __name__ == "__main__":
    unittest.main()
