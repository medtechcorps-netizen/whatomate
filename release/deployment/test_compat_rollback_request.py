"""Captured source plus pure all-head rollback qualification, no live authority.

Real resolver/policy/producer/verifier code consumes synthetic immutable Git,
GitHub and signature transcripts. No service, provider or collector is enabled.
"""
from dataclasses import replace
import json
import unittest

import compat_request as bridge
import compat_evidence as evidence
import compat_rollback_history as history_policy
import test_compat_rollback_history as histories
import test_record_resolver_rollback as sources
import test_compat_adapter as signatures
import cli


def source_resolution():
    case = sources.RollbackProofResolverTests()
    case.setUp()
    try:
        target = case.add(version=1, sha='1' * 40, images=case.boot['images'])
        live_image = case.add(version=1, sha='4' * 40)
        case.add(sha='2' * 40, db_change='data', dormant=True)
        case.add(sha='3' * 40, db_change='expand', dormant=True)
        case.add(kind='rollback', target=live_image)
        for sha in (case.boot['sha'], live_image['sha'], case.world.main):
            case.profile(sha)
        case.world.files[case.boot['sha'], case.RECIPE] += b'# exact target recipe\n'
        case.world.files[live_image['sha'], case.RECIPE] += b'# exact live recipe\n'
        value = case.proof(case.world.main, target['release'])
        assert case.client.budget.closed and not case.client.budget.failed
        return value
    finally:
        case.doCleanups()


def producer_binding(value):
    """Independent actual producer projection, not the bridge under test."""
    ctx = cli.Context(value.producer.sha, 100, 1, value.resolved.selected.release,
                      'workflow_dispatch', 'refs/heads/main')
    snapshot = cli._rollback_snapshot(value, ctx)
    return evidence.parse_rollback_binding({'producer_sha': snapshot['sha'], **{
        key: snapshot[key] for key in ('latest', 'selected', 'database_base',
        'selected_web_source_sha', 'live_web_source_sha', 'span', 'source_material_sha256')}})


def collected(history, issues=()):
    # Counts and transcript are synthetic transport metadata; types grant no
    # authentication or current-main authority. Full raw history is validated.
    return history_policy.CollectedRollbackHistory(history, issues, 20, 128 * 1024, 'a' * 64)


class RollbackRequestTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        sources.RollbackProofResolverTests.setUpClass()
        cls.resolution = source_resolution()
        cls.binding = producer_binding(cls.resolution)
        cls.history = histories.history(cls.binding)
        cls.collected = collected(cls.history)

    def refuse(self, value=None, result=None, *, code='compat-stale'):
        with self.assertRaises(evidence.CompatRefusal) as caught:
            bridge.qualify_rollback(self.resolution if value is None else value,
                                    self.collected if result is None else result)
        self.assertEqual(caught.exception.code, code)
        self.assertNotIn('PRIVATE', str(caught.exception))
        return caught.exception

    def test_complete_resolver_producer_and_request_derive_the_same_distinct_binding(self):
        request = bridge.build_rollback_request(self.resolution, self.collected)
        self.assertEqual(request.binding, self.binding)
        self.assertEqual(request.workflow, self.resolution.workflow)
        self.assertEqual(request.observation_id, self.history.observation_id)
        self.assertEqual(request.binding.producer_sha, 'a' * 40)
        self.assertEqual(request.binding.selected.sha, '1' * 40)
        self.assertEqual(request.binding.latest.sha, '4' * 40)
        self.assertEqual(request.binding.database_base.source_sha, '3' * 40)
        self.assertNotEqual(request.binding.selected_web_source_sha, request.binding.selected.sha)
        self.assertEqual(request.binding.live_web_source_sha, '4' * 40)
        self.assertEqual(request.binding.span.non_none, 2)
        self.assertEqual(request.binding.span.records, 5)
        qualified = bridge.qualify_rollback(self.resolution, self.collected)
        self.assertEqual(qualified.binding, request.binding)
        self.assertEqual(qualified.observation_id, request.observation_id)
        self.assertNotIsInstance(qualified, bridge.policy.RollbackDecision)
        with self.assertRaisesRegex(bridge.policy.PolicyRefusal, 'rollback-qualification-unavailable'):
            bridge.policy.rollback_plan(self.resolution.resolved.chain, request.binding.selected.release)

    def test_input_types_cannot_substitute_normal_or_unbound_objects(self):
        for value in (None, {}, object(), self.resolution.resolved):
            with self.subTest(source=type(value)), self.assertRaises(evidence.CompatRefusal):
                bridge.build_rollback_request(value, self.collected)
        for value in (None, {}, object(), self.history):
            # None is passed explicitly here rather than the helper default.
            with self.subTest(collected=type(value)), self.assertRaises(evidence.CompatRefusal):
                bridge.build_rollback_request(self.resolution, value)

    def test_full_raw_chain_and_exact_selection_are_replayed_before_compact_binding(self):
        value = self.resolution; resolved = value.resolved
        variants = [replace(value, resolved=replace(resolved, selected=resolved.latest)),
            replace(value, resolved=replace(resolved, latest=resolved.selected)),
            replace(value, resolved=replace(resolved, chain=replace(resolved.chain,
                inputs=resolved.chain.inputs[:-1]))),
            replace(value, requirements=replace(value.requirements, span=value.requirements.span[1:])),
            replace(value, requirements=replace(value.requirements, span_sha256='f' * 64)),
            replace(value, requirements=replace(value.requirements, non_none_count=1))]
        for index, bad in enumerate(variants):
            with self.subTest(case=index): self.refuse(value=bad)

    def test_captured_main_inventory_and_all_image_sources_are_not_caller_assertions(self):
        value = self.resolution; resolved = value.resolved
        bindings = list(resolved.image_bindings)
        # Alter an intermediate non-web source, not merely the displayed target.
        row = bindings[2]
        fields = list(row.source_shas); fields[-1] = (fields[-1][0], 'f' * 40)
        bindings[2] = replace(row, source_shas=tuple(fields))
        variants = [replace(value, resolved=replace(resolved,
                        inventory=replace(resolved.inventory, main_sha='f' * 40))),
                    replace(value, resolved=replace(resolved,
                        inventory=replace(resolved.inventory, records=resolved.inventory.records[:-1]))),
                    replace(value, resolved=replace(resolved, image_bindings=tuple(bindings)))]
        for index, bad in enumerate(variants):
            with self.subTest(case=index): self.refuse(value=bad)

    def test_raw_source_not_just_a_supplied_digest_must_pass_complete_material_validation(self):
        value = self.resolution
        material = value.materials[0]
        for path in bridge.policy.SOURCE_PATHS:
            rows = tuple(replace(row, raw=row.raw + b'\n') if row.path == path else row
                         for row in material.blobs)
            bad = replace(value, materials=(replace(material, blobs=rows), *value.materials[1:]))
            with self.subTest(path=path): self.refuse(value=bad)
        for bad in (replace(value, materials=value.materials[:-1]),
                    replace(value, materials=(replace(material, provenance='PRIVATE'), *value.materials[1:]))):
            self.refuse(value=bad)

    def test_endpoint_recipe_source_and_raw_hash_disagreement_refuse(self):
        value = self.resolution
        for name in ('target_recipe', 'live_recipe'):
            recipe = getattr(value, name)
            for changed in (replace(recipe, source_sha=value.producer.sha),
                            replace(recipe, raw=recipe.raw + b'# PRIVATE\n'),
                            replace(recipe, sha256='f' * 64), replace(recipe, path='PRIVATE')):
                with self.subTest(role=name, recipe=changed):
                    self.refuse(value=replace(value, **{name: changed}))

    def test_supported_history_workflow_must_equal_exact_resolved_H_raw_bytes(self):
        value = self.resolution
        changed = value.workflow + b'# source differs\n'
        self.refuse(value=replace(value, workflow=changed, workflow_sha256=bridge.common.sha256_bytes(changed)))
        self.refuse(value=replace(value, workflow_sha256='f' * 64))
        for raw in (None, changed):
            workflows = tuple(replace(row, raw=raw) if row.sha == value.producer.sha else row
                              for row in self.history.workflows)
            with self.subTest(absent=raw is None):
                self.refuse(result=collected(replace(self.history, workflows=workflows)))

    def test_collected_metadata_and_observation_identity_remain_closed_and_bounded(self):
        for key, wrong in [('command_count', True), ('command_count', 0), ('command_count', 6001),
                           ('captured_bytes', -1), ('captured_bytes', 128 * 1024 * 1024 + 1),
                           ('transcript_sha256', 'PRIVATE'), ('issues', ('PRIVATE',))]:
            with self.subTest(key=key): self.refuse(result=replace(self.collected, **{key: wrong}))
        self.refuse(result=collected(replace(self.history, producer_sha='f' * 40)))
        self.refuse(result=collected(replace(self.history, observation_id='PRIVATE')))
        self.refuse(result=collected(replace(self.history, enumeration='incomplete')))

    def test_fatal_collection_issues_cannot_qualify_an_apparent_complete_positive(self):
        for issue in ('inventory-changed', 'inventory-unavailable', 'collection-deadline'):
            with self.subTest(issue=issue):
                self.refuse(result=replace(self.collected, issues=(issue,)))

    def test_real_fixed_Gh_verifier_pair_composes_with_source_and_raw_history(self):
        row = self.history.attempts[0]
        workflow = next(value for value in self.history.workflows if value.sha == self.binding.producer_sha)
        replayed = histories.envelope.replay(workflow, row.envelope, observed_at=self.history.observed_at)
        original = row.measurement
        meta = signatures.adapter.ArtifactLocation(original.artifact.artifact_id,
            original.artifact.name, original.artifact.run_id, original.artifact.run_attempt,
            original.artifact.sha, False)
        with signatures.harness(replayed.attempt) as (root, client, fake, attempt):
            pair = signatures.adapter.verify_rollback_pair(client, signatures.archive(original), meta, attempt, root)
            self.assertEqual(len(fake.calls), 3)
        history = replace(self.history, attempts=(replace(row, measurement=pair),))
        qualified = bridge.qualify_rollback(self.resolution, collected(history))
        self.assertEqual(qualified.result_sha256, bridge.common.sha256_bytes(pair.result))
        self.assertEqual(qualified.context_sha256, bridge.common.sha256_bytes(pair.context))
        self.assertEqual(qualified.execution_window, replayed.attempt.execution_window)

    def test_exact_bound_negative_keeps_specific_old_server_or_migration_refusal(self):
        for options, code in [({'server_false': 'read'}, 'compat-old-server-incompatible'),
                              ({'migration_false': 'rw'}, 'irreversible-needs-fix-forward-mode'),
                              ({'forward': False}, 'compat-stale')]:
            with self.subTest(options=options):
                history = histories.history(self.binding, histories.row(self.binding, **options))
                self.refuse(result=collected(history), code=code)

    def test_artifact_context_never_supplies_the_request_source_facts(self):
        alternatives = [replace(self.binding, source_material_sha256='f' * 64),
            replace(self.binding, span=replace(self.binding.span, sha256='f' * 64)),
            replace(self.binding, selected_web_source_sha='f' * 40),
            replace(self.binding, live_web_source_sha='f' * 40),
            replace(self.binding, database_base=replace(self.binding.database_base, source_sha='f' * 40))]
        for index, wrong in enumerate(alternatives):
            history = histories.history(self.binding, histories.row(wrong))
            result = collected(history)
            request = bridge.build_rollback_request(self.resolution, result)
            self.assertEqual(request.binding, self.binding)
            with self.subTest(binding=index): self.refuse(result=result)

    def test_measured_catalog_must_match_expected_measurement_not_golden_hash(self):
        for wrong in ('f' * 64, dict(bridge.policy.V0_PINS)[bridge.policy.GOLDEN]):
            row = histories.row(self.binding, catalog_sha256=wrong)
            result = collected(histories.history(self.binding, row))
            request = bridge.build_rollback_request(self.resolution, result)
            self.assertEqual(request.binding.database_base.catalog.catalog_sha256, bridge.policy.V0_MEASUREMENT)
            self.refuse(result=result)

    def test_earlier_complete_negative_or_unknown_artifact_can_recover(self):
        for options in ({'server_false': 'workers'}, {'missing': evidence.MissingEvidence.UNKNOWN}):
            older = histories.row(self.binding, run=101, number=20, minute=1, **options)
            newer = histories.row(self.binding, run=100, number=10, minute=10)
            history = histories.history(self.binding, older, newer)
            qualified = bridge.qualify_rollback(self.resolution, collected(history, ('artifact-unverified',)))
            self.assertEqual(qualified.run_id, 100)
            self.assertEqual(qualified.binding, self.binding)

    def test_later_foreign_green_and_unsupported_earlier_source_cannot_be_filtered_out(self):
        later = histories.row(self.binding, run=101, number=11, minute=20, sha='b' * 40)
        self.refuse(result=collected(histories.history(self.binding, self.history.attempts[0], later)))
        earlier = histories.row(self.binding, run=101, number=11, minute=1, sha='b' * 40)
        valid = histories.history(self.binding, earlier, self.history.attempts[0])
        self.assertEqual(bridge.qualify_rollback(self.resolution, collected(valid)).run_id, 100)
        bad = replace(valid, workflows=tuple(replace(row, raw=None) if row.sha == 'b' * 40 else row
                                            for row in valid.workflows))
        self.refuse(result=collected(bad))

    def test_reused_job_times_never_gain_current_execution_freshness(self):
        row = histories.row(self.binding, missing=evidence.MissingEvidence.UNKNOWN)
        raw = json.loads(row.envelope.raw)
        raw['run']['run_started_at'] = raw['run']['updated_at']
        row = replace(row, envelope=history_policy.EnvelopeFacts(bridge.common.canonical_file_bytes(raw)))
        history = histories.history(self.binding, row)
        replayed = histories.envelope.replay(history.workflows[0], row.envelope, observed_at=history.observed_at)
        self.assertIsNone(replayed.attempt.execution_window)
        self.refuse(result=collected(history))

    def test_normal_current_positive_is_not_reinterpreted_as_rollback(self):
        row = histories.row(self.binding, normal=True)
        self.refuse(result=collected(histories.history(self.binding, row)))

    def test_missing_or_duplicate_raw_attempts_cannot_be_filled_by_an_artifact(self):
        empty = replace(self.history, attempts=())
        self.refuse(result=collected(empty), code='compat-missing')
        duplicate = replace(self.history, attempts=self.history.attempts * 2)
        self.refuse(result=collected(duplicate))
        gap = histories.row(self.binding, attempt=2, latest=2)
        self.refuse(result=collected(histories.history(self.binding, gap)))


if __name__ == '__main__':
    unittest.main()
