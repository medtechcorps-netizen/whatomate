"""Bounded codec and pure writer replay; no workflow/provider/network actions."""
import base64
import copy
from dataclasses import replace
from datetime import datetime, timedelta, timezone
import hashlib
import json
import lzma
from pathlib import Path
import unittest
from unittest import mock
import zlib

import record_handoff as codec
import test_record_writer as fixture

w = codec.writer


def envelope(raw, *, compressed=None, version='2'):
    """Adversarial untrusted wire fixture, independent of the encoder."""
    packed = zlib.compress(raw, 9) if compressed is None else compressed
    text = base64.b64encode(packed).decode('ascii')
    result = {'handoff_version': version, 'handoff_sha256': hashlib.sha256(raw).hexdigest(),
        'handoff_raw_length': str(len(raw)), 'handoff_compressed_length': str(len(packed)),
        'handoff_encoded_length': str(len(text)), 'handoff_chunk_count': str((len(text) + 8191) // 8192)}
    result.update((f'handoff_{i + 1:02d}', text[i * 8192:(i + 1) * 8192]) for i in range(32))
    return result


def raw_json(value):
    return (json.dumps(value, ensure_ascii=True, sort_keys=True, separators=(',', ':')) + '\n').encode('ascii')


class HandoffTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        fixture.WriterTests.setUpClass()
        cls.resolution = fixture.WriterTests.forward
        cls.history = fixture.WriterTests.history
        cls.handoff = w.prepare_promotion(cls.resolution, cls.history,
            fixture.images(cls.resolution), fixture.execution(), staging='passed', drills=fixture.drills(cls.resolution))
        cls.outputs = codec.encode(cls.handoff)
        # Retain a separately built historical v2 wire for all inherited JSON,
        # per-blob zlib and closed-type adversarial cases. Public success uses v5.
        cls.historical = fixture.historical_handoff(cls.resolution)
        budget = codec._Budget(version=2)
        node = codec._value(cls.historical, w.PromotionHandoff, budget, 0, encoding=True)
        cls.wire = {'version': 2, 'handoff': node, 'blobs': [
            {'sha256': digest, 'length': len(raw),
             'value': base64.b64encode(zlib.compress(raw, 9)).decode()}
            for digest, raw in sorted(budget.blobs.items())]}
        cls.raw = raw_json(cls.wire)

    def decode(self, outputs=None, **changes):
        args = dict(candidate_sha=self.handoff.candidate_images.sha,
                    workflow_sha=self.handoff.execution.workflow_sha,
                    run_id=self.handoff.execution.run_id, record_attempt=1)
        args.update(changes)
        return codec.decode(self.outputs if outputs is None else outputs, **args)

    def build_record(self, outputs=None, **changes):
        args = dict(candidate_sha=self.handoff.candidate_images.sha,
                    workflow_sha=self.handoff.execution.workflow_sha,
                    run_id=self.handoff.execution.run_id, record_attempt=1,
                    publication=fixture.publication(), current_resolution=self.resolution.resolved)
        args.update(changes)
        return codec.build_record(self.outputs if outputs is None else outputs, **args)

    def refuse(self, outputs=None, **changes):
        with self.assertRaises(codec.HandoffRefusal) as caught:
            self.decode(outputs, **changes)
        self.assertEqual(str(caught.exception), 'record-handoff-invalid')
        self.assertIsNone(caught.exception.__cause__)

    def mutate(self, change):
        value = copy.deepcopy(self.wire)
        change(value['handoff']['fields'])
        return envelope(raw_json(value))

    def test_real_catalog_full_graph_roundtrip_and_actual_bounded_size(self):
        restored = self.decode()
        self.assertEqual(restored, self.handoff)
        self.assertIs(type(restored.resolution.classification), codec.s.PolicyClassification)
        self.assertIs(type(restored.facts), codec.d.ManifestFacts)
        self.assertIs(type(restored.facts.catalog), codec.d.DocumentCatalog)
        blobs = restored.resolution.catalog_material.candidate_blobs
        golden = next(row.raw for row in blobs if row.path == codec.p.GOLDEN)
        self.assertEqual(len(golden), 1762225)
        baseline = restored.resolution.catalog_material.baseline_blobs
        self.assertEqual(next(row.raw for row in baseline if row.path == codec.p.GOLDEN), golden)
        self.assertLess(len(self.raw), len(golden))
        self.assertEqual(len(self.wire['blobs']), len({row['sha256'] for row in self.wire['blobs']}))
        self.assertLessEqual(int(self.outputs['handoff_encoded_length']), 262144)
        self.assertEqual(len(self.outputs), 38)
        self.assertTrue(all(len(value) <= 8192 and value.isascii() for value in self.outputs.values()))
        original = w.build_promotion(self.handoff, fixture.publication(), self.resolution.resolved)
        self.assertEqual(w.build_promotion(restored, fixture.publication(), self.resolution.resolved), original)

    def test_same_frozen_values_encode_identically_and_fit_existing_output_guard(self):
        self.assertEqual(codec.encode(self.handoff), self.outputs)
        for key, value in self.outputs.items():
            self.assertRegex(key, w.common.OUTPUT_KEY_RE)
            self.assertRegex(value, w.common.OUTPUT_VALUE_RE)
            if value: self.assertEqual(w.common.require_public_text(value), value)

    def test_record_rerun_preserves_original_production_attempt(self):
        restored = self.decode(record_attempt=9)
        self.assertEqual(restored.execution.run_attempt, 1)
        for field, value in [('candidate_sha', 'f' * 40), ('workflow_sha', 'f' * 40),
                             ('run_id', self.handoff.execution.run_id + 1), ('record_attempt', 0),
                             ('record_attempt', True), ('run_id', True)]:
            with self.subTest(field=field, value=value): self.refuse(**{field: value})

    def test_foreign_execution_or_original_rerun_refuses(self):
        for field, value in [('repository', 'PRIVATE'), ('workflow', 'PRIVATE'), ('run_attempt', 2)]:
            outputs = self.mutate(lambda fields: fields['execution']['fields'].update({field: value}))
            self.refuse(outputs)
        changed = replace(self.handoff, execution=replace(self.handoff.execution, run_attempt=2))
        with self.assertRaises(codec.HandoffRefusal): codec.encode(changed)

    def test_complete_missing_history_and_enum_markers_are_retained(self):
        actions = fixture.composition.actions
        world = actions.World(); world.add(); world.add(run=101, number=11, minute=30, negative=True)
        world.add(run=102, number=12, minute=60)
        # An expired first artifact yields a bounded MissingEvidence entry with
        # an actual completed window before the newer successful attempt.
        world.artifacts[100][0]['expired'] = True
        history = fixture.composition.collected(self.resolution, world)
        plan = w.prepare_promotion(self.resolution, history, fixture.images(self.resolution), fixture.execution(), staging='passed', drills=fixture.drills(self.resolution))
        restored = self.decode(codec.encode(plan))
        self.assertEqual(restored.collected, history)
        self.assertEqual(len(restored.collected.history.attempts), 3)
        self.assertIs(type(restored.collected.history.attempts[0].measurement), codec.e.MissingEvidence)
        negative = restored.collected.history.attempts[1].measurement
        self.assertIs(type(negative), codec.e.AuthenticatedPair)
        self.assertIs(json.loads(negative.result)['old_server_ok']['workers'], False)

    def test_hash_lengths_counts_versions_and_slots_are_not_optional(self):
        for key, value in [('handoff_sha256', 'f' * 64), ('handoff_version', '6'),
                           ('handoff_raw_length', '1'), ('handoff_compressed_length', '1'),
                           ('handoff_encoded_length', '1'), ('handoff_chunk_count', '0'),
                           ('handoff_chunk_count', '01'), ('handoff_raw_length', True)]:
            changed = dict(self.outputs); changed[key] = value
            with self.subTest(key=key): self.refuse(changed)
        for key in ('handoff_01', 'handoff_32', 'handoff_sha256'):
            changed = dict(self.outputs); del changed[key]; self.refuse(changed)
        changed = dict(self.outputs); changed['PRIVATE'] = 'x'; self.refuse(changed)

    def test_chunk_reordering_gaps_unused_suffix_and_oversize_refuse(self):
        count = int(self.outputs['handoff_chunk_count'])
        self.assertGreater(count, 1); self.assertLess(count, 32)
        changed = dict(self.outputs); changed['handoff_01'], changed['handoff_02'] = changed['handoff_02'], changed['handoff_01']; self.refuse(changed)
        changed = dict(self.outputs); changed['handoff_01'] = ''; self.refuse(changed)
        changed = dict(self.outputs); changed['handoff_32'] = 'AAAA'; self.refuse(changed)
        changed = dict(self.outputs); changed['handoff_01'] = 'A' * 8193; self.refuse(changed)
        changed = dict(self.outputs); changed['handoff_encoded_length'] = str(262145); self.refuse(changed)

    def test_base64_whitespace_non_ascii_and_noncanonical_padding_refuse(self):
        for text in (' ', '\n', '\u00e9', '_', '-'):
            changed = dict(self.outputs); changed['handoff_01'] = text + changed['handoff_01'][1:]
            self.refuse(changed)
        with self.assertRaises(codec.HandoffRefusal): codec._unb64('Zh==', 10)
        with self.assertRaises(codec.HandoffRefusal): codec._unb64('Zg===', 10)

    def test_truncated_concatenated_trailing_and_bomb_compression_refuse(self):
        packed = zlib.compress(self.raw, 9)
        for bad in (packed[:-1], packed + b'PRIVATE', packed + zlib.compress(b'{}'), b'PRIVATE'):
            self.refuse(envelope(self.raw, compressed=bad))
        with mock.patch.object(codec, 'MAX_RAW', 1024):
            bomb = envelope(b' ' * 1025)
            bomb['handoff_raw_length'] = '1024'
            self.refuse(bomb)

    def test_duplicate_keys_nonfinite_float_trailing_and_noncanonical_json_refuse(self):
        for raw in (b'{"version":1,"version":1,"handoff":null}\n',
                    b'{"version":NaN,"handoff":null}\n', b'{"version":1.0,"handoff":null}\n',
                    self.raw + b'{}', self.raw + b' ',
                    json.dumps(self.wire, indent=2).encode()):
            self.refuse(envelope(raw))

    def test_unknown_root_schema_type_fields_and_namedtuple_confusion_refuse(self):
        for modify in (lambda fields: fields.update(PRIVATE=None),
                       lambda fields: fields.pop('facts'),
                       lambda fields: fields['facts'].update(type='PRIVATE'),
                       lambda fields: fields['facts'].update(type='tuple'),
                       lambda fields: fields['resolution']['fields']['classification'].update(type='record_policy.SourceFacts')):
            self.refuse(self.mutate(modify))
        value = copy.deepcopy(self.wire); value['version'] = True; self.refuse(envelope(raw_json(value)))
        value = copy.deepcopy(self.wire); value['handoff']['type'] = '__import__'; self.refuse(envelope(raw_json(value)))

    def test_per_field_types_reject_bool_int_and_list_tuple_confusion(self):
        for modify in (lambda fields: fields['execution']['fields'].update(run_id=True),
                       lambda fields: fields['facts']['fields'].update(reversible=1),
                       lambda fields: fields['candidate_images']['fields'].update(images=[]),
                       lambda fields: fields['candidate_images']['fields']['images']['items'][0].update(items=['web']),
                       lambda fields: fields['execution']['fields'].update(run_id=10 ** 21)):
            self.refuse(self.mutate(modify))

    def test_bytes_datetime_and_missing_enum_have_closed_framing(self):
        for modify in (lambda fields: fields['resolution']['fields']['workflow'].update(sha256='f' * 64),
                       lambda fields: fields['collected']['fields']['history']['fields']['observed_at'].update(value='2026-10-11T00:00:00Z'),
                       lambda fields: fields['collected']['fields']['history']['fields']['observed_at'].update(value='2026-10-11T00:00:00.000000+00:00'),
                       lambda fields: fields['collected']['fields']['history']['fields']['attempts']['items'][0]['fields'].update(measurement={'type':'missing','value':'PRIVATE'})):
            self.refuse(self.mutate(modify))
        for dt in (datetime(2026, 10, 11), datetime(2026, 10, 11, tzinfo=timezone(timedelta(hours=1)))):
            changed = replace(self.handoff.collected.history, observed_at=dt)
            with self.assertRaises(codec.HandoffRefusal):
                codec.encode(replace(self.handoff, collected=replace(self.handoff.collected, history=changed)))

    def test_depth_node_string_and_integer_caps_precede_replay(self):
        self.refuse(envelope(b'[' * 49 + b'0' + b']' * 49))
        with mock.patch.object(codec, 'MAX_NODES', 20): self.refuse(envelope(b'[' + b'0,' * 20 + b'0]'))
        with mock.patch.object(codec, 'MAX_RAW', 100):
            changed = replace(self.handoff, execution=replace(self.handoff.execution, repository='x' * 101))
            with self.assertRaises(codec.HandoffRefusal): codec.encode(changed)
        self.refuse(envelope(b'{"version":999999999999999999999999,"handoff":null}\n'))

    def test_malicious_objects_subclasses_and_hooks_are_never_invoked(self):
        class Malicious:
            def __getattribute__(self, _name): raise AssertionError('hook called')
        class ListLike(tuple):
            def __iter__(self): raise AssertionError('hook called')
        with self.assertRaises(codec.HandoffRefusal): codec.encode(Malicious())
        changed = replace(self.handoff.candidate_images, images=ListLike())
        with self.assertRaises(codec.HandoffRefusal): codec.encode(replace(self.handoff, candidate_images=changed))

    def test_rehashed_source_evidence_or_derived_fact_tamper_is_not_a_proof(self):
        for modify in (lambda fields: fields['facts']['fields'].update(compat_run_id=123),
                       lambda fields: fields['facts']['fields']['catalog']['fields'].update(catalog_sha256='f' * 64),
                       lambda fields: fields['resolution']['fields'].update(previous_web_source_sha='f' * 40),
                       lambda fields: fields['collected']['fields']['history']['fields'].update(enumeration='incomplete')):
            self.refuse(self.mutate(modify))

    def test_blob_table_requires_exact_sorted_hashes_lengths_and_every_reference(self):
        for change in (lambda rows: rows.reverse(), lambda rows: rows.append(rows[-1]),
                       lambda rows: rows.pop(), lambda rows: rows[0].update(length=True),
                       lambda rows: rows[0].update(sha256='f' * 64),
                       lambda rows: rows[0].update(value='PRIVATE'),
                       lambda rows: rows[0].update(extra='PRIVATE')):
            value = copy.deepcopy(self.wire); change(value['blobs']); self.refuse(envelope(raw_json(value)))
        value = copy.deepcopy(self.wire)
        extra = b'unused public synthetic bytes'
        value['blobs'].append({'sha256': hashlib.sha256(extra).hexdigest(), 'length':len(extra),
                               'value':base64.b64encode(zlib.compress(extra)).decode()})
        value['blobs'].sort(key=lambda row:row['sha256'])
        self.refuse(envelope(raw_json(value)))

    def test_blob_stream_bomb_truncation_trailing_and_aggregate_limits(self):
        for suffix in (b'PRIVATE', zlib.compress(b'extra')):
            value = copy.deepcopy(self.wire); row = value['blobs'][0]
            row['value'] = base64.b64encode(base64.b64decode(row['value']) + suffix).decode()
            self.refuse(envelope(raw_json(value)))
        value = copy.deepcopy(self.wire); row = value['blobs'][0]
        row['value'] = base64.b64encode(base64.b64decode(row['value'])[:-1]).decode()
        self.refuse(envelope(raw_json(value)))
        value = copy.deepcopy(self.wire); value['blobs'][0]['length'] = 1
        value['blobs'][0]['value'] = base64.b64encode(zlib.compress(b'x' * 10000)).decode()
        self.refuse(envelope(raw_json(value)))
        with mock.patch.object(codec, 'MAX_EXPANDED_BYTES', 1): self.refuse()
        with mock.patch.object(codec, 'MAX_BLOB', 1): self.refuse()
        with mock.patch.object(codec, 'MAX_BLOBS', 1): self.refuse()

    def test_later_main_allowed_but_changed_parent_still_refused_after_decode(self):
        restored = self.decode(record_attempt=2)
        resolved = self.resolution.resolved
        fresh = replace(resolved, inventory=replace(resolved.inventory, main_sha='f' * 40))
        raw = w.build_promotion(restored, fixture.publication(), fresh)
        advanced = fixture.appended(resolved, raw, self.resolution.candidate)
        with self.assertRaises(w.WriterRefusal): w.build_promotion(restored, fixture.publication(), advanced)

    def test_no_emission_file_command_or_provider_hook_exists_and_size_failure_returns_nothing(self):
        with mock.patch.object(Path, 'open', side_effect=AssertionError('I/O')), \
             mock.patch.object(w.transport.runtime, 'bounded_command', side_effect=AssertionError('command')):
            self.assertEqual(self.decode(), self.handoff)
        with mock.patch.object(codec, 'MAX_COMPRESSED', 1), \
             mock.patch.object(w.common.Output, 'set_outputs', side_effect=AssertionError('partial output')):
            with self.assertRaises(codec.HandoffRefusal): codec.encode(self.handoff)

    def legacy_outputs(self):
        """Exact pre-audit version-1 field graph, not a new encoder route."""
        wire = copy.deepcopy(self.wire)
        wire['version'] = 1
        del wire['handoff']['fields']['resolution']['fields']['audit']
        used = set()
        def visit(value):
            if type(value) is dict:
                if value.get('type') == 'bytes': used.add(value['sha256'])
                for child in value.values(): visit(child)
            elif type(value) is list:
                for child in value: visit(child)
        visit(wire['handoff'])
        wire['blobs'] = [row for row in wire['blobs'] if row['sha256'] in used]
        return envelope(raw_json(wire), version='1')

    def test_version2_preserves_all_audit_ranges_and_drill_source_bytes(self):
        restored = self.decode(envelope(self.raw), record_attempt=2)
        self.assertEqual(self.outputs['handoff_version'], '5')
        self.assertEqual(self.wire['version'], 2)
        self.assertEqual(restored.resolution.audit, self.resolution.audit)
        self.assertEqual(len(restored.resolution.audit.changes), 3)
        self.assertEqual(restored.resolution.audit.drills.raw,
            (fixture.composition.source.ROOT / codec.r._DRILLS).read_bytes())
        self.assertEqual({role for row in restored.resolution.audit.changes for role in row.roles},
            {'database-origin', 'latest-record', 'selected-web'})

    def test_version1_is_historical_custody_only_and_record_retry_still_replays(self):
        restored = self.decode(self.legacy_outputs(), record_attempt=3)
        self.assertIsNone(restored.resolution.audit)
        fresh = replace(self.resolution.resolved,
            inventory=replace(self.resolution.resolved.inventory, main_sha='f' * 40))
        self.assertEqual(self.build_record(self.legacy_outputs(), current_resolution=fresh, record_attempt=3),
            w.build_promotion(self.handoff, fixture.publication(), fresh))
        with self.assertRaises(w.WriterRefusal): w.build_promotion(restored, fixture.publication(), fresh)
        with self.assertRaises(codec.HandoffRefusal): codec.encode(restored)
        with self.assertRaises(codec.p.PolicyRefusal):
            codec.r.validate_forward_audit(restored.resolution.audit,
                candidate_sha=restored.resolution.candidate.sha,
                database_base_source=restored.resolution.database_base_source,
                latest_record_sha=restored.resolution.resolved.latest.record_sha,
                selected_web_source_sha=restored.resolution.previous_web_source_sha)

    def test_versions_cannot_be_relabelled_or_omit_or_inject_audit_fields(self):
        self.refuse(self.mutate(lambda fields: fields['resolution']['fields'].pop('audit')))
        self.refuse(self.mutate(lambda fields: fields['resolution']['fields'].update(audit=None)))
        old = self.legacy_outputs(); old['handoff_version'] = '2'; self.refuse(old)
        changed = dict(self.outputs); changed['handoff_version'] = '1'; self.refuse(changed)
        wire = copy.deepcopy(self.wire); wire['version'] = 1
        self.refuse(envelope(raw_json(wire), version='1'))

    def test_audit_wrong_partition_binding_and_malformed_rows_refuse(self):
        def audit(fields): return fields['resolution']['fields']['audit']['fields']
        def first(fields): return audit(fields)['changes']['items'][0]['fields']
        for change in (lambda f: audit(f).update(candidate_sha='f' * 40),
                       lambda f: audit(f)['drills']['fields'].update(path='PRIVATE'),
                       lambda f: audit(f)['changes']['items'].pop(),
                       lambda f: first(f)['roles'].update(items=[]),
                       lambda f: first(f).update(base_sha='f' * 40),
                       lambda f: first(f).update(head_sha='f' * 40),
                       lambda f: first(f)['rows']['items'].append({'type':'record_resolver.ChangedPath',
                           'fields':{'status':'M','path':'../PRIVATE'}})):
            self.refuse(self.mutate(change))

    def test_complete_large_audit_refuses_original_codec_cap_before_any_emission(self):
        audit = self.handoff.resolution.audit
        rows = tuple(sorted((codec.r.ChangedPath('M', 'files/' + hashlib.sha512(str(i).encode()).hexdigest() + '.py')
                             for i in range(2200)), key=lambda row:row.path))
        ranges = (replace(audit.changes[0], rows=rows), *audit.changes[1:])
        changed = replace(self.handoff, resolution=replace(self.resolution, audit=replace(audit, changes=ranges)))
        codec.r.validate_forward_audit(changed.resolution.audit, candidate_sha=self.resolution.candidate.sha,
            database_base_source=self.resolution.database_base_source,
            latest_record_sha=self.resolution.resolved.latest.record_sha,
            selected_web_source_sha=self.resolution.previous_web_source_sha)
        with mock.patch.object(w.common.Output, 'set_outputs', side_effect=AssertionError('partial output')), \
             mock.patch.object(w.transport.runtime, 'bounded_command', side_effect=AssertionError('provider/command')):
            with self.assertRaises(codec.HandoffRefusal): codec.encode(changed)


def xz_envelope(raw, *, compressed=None, version='5'):
    packed = (lzma.compress(raw, format=lzma.FORMAT_XZ, check=lzma.CHECK_CRC64, preset=6)
              if compressed is None else compressed)
    return envelope(raw, compressed=packed, version=version)


def frame(header, payload):
    raw = raw_json(header)
    return len(raw).to_bytes(4, 'big') + raw + payload


class XZTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        HandoffTests.setUpClass.__func__(cls)
        encoded = ''.join(cls.outputs[key] for key in codec.CHUNK_SLOTS)
        cls.frame = lzma.decompress(base64.b64decode(encoded), format=lzma.FORMAT_XZ)
        size = int.from_bytes(cls.frame[:4], 'big')
        cls.header = json.loads(cls.frame[4:4 + size])
        cls.payload = cls.frame[4 + size:]
        cls.measurements = {'baseline': {
            'raw_bytes': len(cls.frame), 'header_bytes': size,
            'compressed_bytes': int(cls.outputs['handoff_compressed_length']),
            'encoded_bytes': int(cls.outputs['handoff_encoded_length']),
            'chunks': int(cls.outputs['handoff_chunk_count']),
            'raw_sha256': cls.outputs['handoff_sha256'],
            'history_attempts': len(cls.handoff.collected.history.attempts),
            'public_roundtrip': True}}

    decode = HandoffTests.decode
    build_record = HandoffTests.build_record
    refuse = HandoffTests.refuse

    def test_v3_exact_historical_frame_has_no_observation_or_reencode_authority(self):
        budget = codec._Budget(version=3)
        node = codec._value(self.historical, w.PromotionHandoff, budget, 0, encoding=True)
        raw = codec._frame(node, budget.blobs, version=3)
        old = self.decode(xz_envelope(raw, version='3'), record_attempt=17)
        self.assertEqual(old, self.historical)
        with self.assertRaises(codec.HandoffRefusal): codec.encode(old)
        self.assertEqual(self.build_record(xz_envelope(raw, version='3')),
                         w.build_promotion(self.handoff, fixture.publication(), self.resolution.resolved))

    def test_v5_requires_complete_original_observation_without_wire_upgrade(self):
        self.assertEqual(self.decode(record_attempt=99).drills, self.handoff.drills)
        for change in (lambda fields: fields.pop('drills'), lambda fields: fields.update(drills=None),
                       lambda fields: fields['drills']['fields'].update(required=1),
                       lambda fields: fields['drills']['fields'].update(required=True),
                       lambda fields: fields['drills']['fields'].update(source_sha256='f' * 64),
                       lambda fields: fields['drills']['fields'].update(staging_restore={'type':'utc', 'value':'2026-10-11T00:00:00.000000Z'}),
                       lambda fields: fields['drills']['fields']['observed_at'].update(value='2020-01-01T00:00:00.000000Z')):
            header = copy.deepcopy(self.header); change(header['handoff']['fields'])
            self.refuse(xz_envelope(frame(header, self.payload)))
        with self.assertRaises(codec.HandoffRefusal): codec.encode(replace(self.handoff, drills=None))

    def test_v5_complete_history_public_roundtrip_and_historical_no_upgrade(self):
        restored = self.decode(record_attempt=4)
        self.assertEqual(restored, self.handoff)
        self.assertEqual(restored.collected.history, self.history.history)
        self.assertEqual(self.header['version'], 5)
        self.assertEqual(int(self.outputs['handoff_raw_length']), len(self.frame))
        self.assertEqual(self.outputs['handoff_sha256'], hashlib.sha256(self.frame).hexdigest())
        self.assertGreater(len(self.frame), 1762225)
        old = self.decode(envelope(self.raw), record_attempt=2)
        self.assertIsNone(old.drills)
        with self.assertRaises(codec.HandoffRefusal): codec.encode(old)
        # Hash-bound transport does not admit a different operation identity.
        self.refuse(run_id=self.handoff.execution.run_id + 1)

    def test_lzma_absence_is_fixed_refusal_only_for_new_wire(self):
        import builtins
        original = builtins.__import__
        def without_lzma(name, *args, **kwargs):
            if name == 'lzma': raise ImportError('PRIVATE unsupported runtime')
            return original(name, *args, **kwargs)
        with mock.patch('builtins.__import__', side_effect=without_lzma):
            self.refuse()
            with self.assertRaisesRegex(codec.HandoffRefusal, '^record-handoff-invalid$'):
                codec.encode(self.handoff)
            self.assertEqual(self.decode(envelope(self.raw)), self.historical)
            self.assertIsNone(self.decode(HandoffTests.legacy_outputs(self)).resolution.audit)
        with mock.patch.object(lzma, 'is_check_supported', return_value=False):
            self.refuse()

    def test_xz_requires_crc64_one_complete_stream_and_bounded_expansion(self):
        packed = base64.b64decode(''.join(self.outputs[key] for key in codec.CHUNK_SLOTS))
        for bad in (packed[:-1], packed + b'PRIVATE', packed + packed,
                    packed + b'\0' * 4, packed[:30] + bytes([packed[30] ^ 1]) + packed[31:],
                    zlib.compress(self.frame), b'PRIVATE'):
            with self.subTest(kind='stream'): self.refuse(xz_envelope(self.frame, compressed=bad))
        for check in (lzma.CHECK_NONE, lzma.CHECK_CRC32, lzma.CHECK_SHA256):
            self.refuse(xz_envelope(self.frame, compressed=lzma.compress(self.frame, check=check)))
        self.refuse(xz_envelope(self.frame,
            compressed=lzma.compress(self.frame, format=lzma.FORMAT_ALONE)))
        with mock.patch.object(codec, 'XZ_MEMORY_LIMIT', 1024 * 1024): self.refuse()
        bomb = xz_envelope(b'x' * 1025); bomb['handoff_raw_length'] = '1024'
        with mock.patch.object(codec, 'MAX_RAW', 1024): self.refuse(bomb)

    def test_header_prefix_json_and_total_frame_framing_are_exact(self):
        header = raw_json(self.header)
        for raw in (b'\0' * 4, b'\0\0\0', (0xffffffff).to_bytes(4, 'big') + header,
                    (len(header) - 1).to_bytes(4, 'big') + header + self.payload,
                    (len(header) + 1).to_bytes(4, 'big') + header + self.payload,
                    self.frame[:-1], self.frame + b'PRIVATE'):
            self.refuse(xz_envelope(raw))
        for bad in (b'{"version":3,"version":3,"handoff":null,"blobs":[]}\n',
                    b'{"version":NaN,"handoff":null,"blobs":[]}\n',
                    b'[' * 49 + b'0' + b']' * 49,
                    header + b' ', json.dumps(self.header, indent=2).encode()):
            self.refuse(xz_envelope(len(bad).to_bytes(4, 'big') + bad + self.payload))
        for version in (True, 1, 2, 3, 4, 6):
            changed = copy.deepcopy(self.header); changed['version'] = version
            self.refuse(xz_envelope(frame(changed, self.payload)))
        changed = dict(self.outputs); changed['handoff_version'] = '2'; self.refuse(changed)
        old = envelope(self.raw); old['handoff_version'] = '3'; self.refuse(old)

    def test_raw_blob_descriptors_and_payload_require_complete_sorted_exact_bytes(self):
        for modify in (lambda rows: rows.reverse(), lambda rows: rows.append(rows[-1]),
                       lambda rows: rows.pop(), lambda rows: rows[0].update(length=True),
                       lambda rows: rows[0].update(length=-1),
                       lambda rows: rows[0].update(length=codec.MAX_BLOB + 1),
                       lambda rows: rows[0].update(sha256='f' * 64),
                       lambda rows: rows[0].update(value='PRIVATE'),
                       lambda rows: rows.clear()):
            changed = copy.deepcopy(self.header); modify(changed['blobs'])
            self.refuse(xz_envelope(frame(changed, self.payload)))
        self.refuse(xz_envelope(frame(self.header, b'!' + self.payload[1:])))
        value, blobs = codec._unframe(self.frame)
        # A correctly hashed unused blob is still not admitted.
        blobs[hashlib.sha256(b'extra').hexdigest()] = b'extra'
        self.refuse(xz_envelope(codec._frame(value['handoff'], blobs)))
        for limit in ('MAX_BLOBS', 'MAX_BLOB', 'MAX_EXPANDED_BYTES', 'MAX_NODES'):
            with self.subTest(limit=limit), mock.patch.object(codec, limit, 1): self.refuse()

    def test_v5_replay_and_complete_audit_are_not_bypassed_by_rehashed_frames(self):
        for modify in (lambda f: f['resolution']['fields'].pop('audit'),
                       lambda f: f['resolution']['fields'].update(audit=None),
                       lambda f: f['facts']['fields'].update(compat_run_id=123),
                       lambda f: f['collected']['fields']['history']['fields'].update(enumeration='incomplete'),
                       lambda f: f['execution']['fields'].update(run_attempt=2),
                       lambda f: f.update(PRIVATE=None)):
            changed = copy.deepcopy(self.header); modify(changed['handoff']['fields'])
            self.refuse(xz_envelope(frame(changed, self.payload)))

    def test_exact_frame_and_reference_caps_refuse_without_truncation(self):
        value, blobs = codec._unframe(self.frame)
        with mock.patch.object(codec, 'MAX_RAW', len(self.frame)):
            self.assertEqual(codec._frame(value['handoff'], blobs), self.frame)
            self.assertEqual(self.decode(), self.handoff)
        with mock.patch.object(codec, 'MAX_RAW', len(self.frame) - 1):
            self.refuse()
            with self.assertRaises(codec.HandoffRefusal): codec._frame(value['handoff'], blobs)
        # Each typed occurrence consumes expanded-reference capacity, even if
        # the raw blob appears only once in the frame.
        budget = codec._Budget(blobs)
        with mock.patch.object(codec, 'MAX_EXPANDED_BYTES', sum(map(len, blobs.values()))):
            with self.assertRaises(codec.HandoffRefusal):
                codec._value(value['handoff'], w.PromotionHandoff, budget, 0, encoding=False)

    def test_v4_historical_context2_retains_original_observation_and_closed_retry(self):
        historical = fixture.historical_handoff(self.resolution, observation=fixture.drills(self.resolution))
        budget = codec._Budget(version=4)
        node = codec._value(historical, w.PromotionHandoff, budget, 0, encoding=True)
        raw = codec._frame(node, budget.blobs, version=4)
        outputs = xz_envelope(raw, version='4')
        restored = self.decode(outputs, record_attempt=5)
        self.assertEqual(restored, historical)
        self.assertIsNone(restored.resolution.pending)
        self.assertEqual(restored.drills, historical.drills)
        self.assertEqual(self.build_record(outputs, record_attempt=5),
                         w.build_promotion(self.handoff, fixture.publication(), self.resolution.resolved))
        with self.assertRaises(codec.HandoffRefusal): codec.encode(restored)
        with self.assertRaises(w.WriterRefusal):
            w.build_promotion(restored, fixture.publication(), self.resolution.resolved)
        # The envelope version is part of the validated closed graph. Neither
        # an omitted observation nor a caller-relabelled version grants v5.
        changed = dict(outputs); changed['handoff_version'] = '5'; self.refuse(changed)

    def test_v5_pending_field_is_closed_mandatory_and_raw_replayed(self):
        def pending(fields): return fields['resolution']['fields']['pending']['fields']
        for modify in (lambda f: f['resolution']['fields'].pop('pending'),
                       lambda f: f['resolution']['fields'].update(pending=None),
                       lambda f: pending(f).update(previous_sha='f' * 40),
                       lambda f: pending(f).update(candidate_sha='f' * 40),
                       lambda f: pending(f).update(PRIVATE=None),
                       lambda f: pending(f)['candidate_files']['items'].append(
                           {'type':'pending_source.PendingBlob','fields':{'path':'../PRIVATE','raw':None}})):
            changed = copy.deepcopy(self.header); modify(changed['handoff']['fields'])
            self.refuse(xz_envelope(frame(changed, self.payload)))
        with self.assertRaises(codec.HandoffRefusal):
            codec.encode(replace(self.handoff, resolution=replace(self.resolution, pending=None)))

    def retained_plan(self, *, variant=None, large=False):
        value = fixture.retained_pending_resolution()
        actions = fixture.composition.actions
        world = actions.World()
        if large:
            for index in range(400):
                world.add(run=100 + index, number=10 + index, minute=3 * index,
                          negative=index == 0)
        else:
            world.add(negative=True)
            world.add(run=101, number=11, minute=30)
            if variant == 'missing':
                world.add(run=102, number=12, minute=60, failure=True)
            elif variant == 'overlap':
                # A later-numbered complete negative executes concurrently with
                # the positive. Its actual window veto is retained in custody.
                world.add(run=102, number=12, minute=30, negative=True)
        history = fixture.composition.collected(value, world)
        return value, history

    def test_full_retained_pending_multiple_attempt_public_custody_and_record_replay(self):
        value, history = self.retained_plan()
        observation = fixture.drills(value)
        plan = w.prepare_promotion(value, history, fixture.images(value), fixture.execution(),
                                   staging='passed', drills=observation)
        outputs = codec.encode(plan)
        restored = self.decode(outputs, record_attempt=7)
        self.assertEqual(restored, plan)
        self.assertEqual(restored.resolution.pending.candidate_files, restored.resolution.pending.previous_files)
        self.assertEqual(restored.drills, observation)
        self.assertEqual(restored.collected, history)
        self.assertEqual(len(history.history.attempts), 2)
        earlier = restored.collected.history.attempts[0].measurement
        later = restored.collected.history.attempts[1].measurement
        self.assertIs(json.loads(earlier.result)['old_server_ok']['workers'], False)
        self.assertTrue(all(json.loads(later.result)['old_server_ok'].values()))
        self.assertEqual(json.loads(earlier.context)['version'], 3)
        self.assertEqual(json.loads(later.context)['pending'], json.loads(earlier.context)['pending'])
        future_main = replace(value.resolved, inventory=replace(value.resolved.inventory, main_sha='f' * 40))
        raw = self.build_record(outputs, record_attempt=7, current_resolution=future_main)
        self.assertEqual(raw, w.build_promotion(plan, fixture.publication(), value.resolved))
        self.assertEqual(w.common.loads_strict(raw)['compat_run_id'], 101)
        binary = codec._unpack(outputs)
        type(self).measurements['pending_public_custody'] = {
            'raw_bytes': len(binary), 'header_bytes': int.from_bytes(binary[:4], 'big'),
            'compressed_bytes': int(outputs['handoff_compressed_length']),
            'encoded_bytes': int(outputs['handoff_encoded_length']),
            'chunks': int(outputs['handoff_chunk_count']), 'raw_sha256': outputs['handoff_sha256'],
            'history_attempts': len(history.history.attempts), 'target_objects': 4452,
            'collected_command_count': history.command_count, 'collected_captured_bytes': history.captured_bytes,
            'both_endpoints_equal': True, 'original_drills_retained': True,
            'public_encode_decode_build_record': True,
            'pending_files': [{'path': row.path, 'bytes': len(row.raw),
                               'sha256': hashlib.sha256(row.raw).hexdigest()}
                              for row in value.pending.candidate_files]}
        self.assertLessEqual(int(outputs['handoff_encoded_length']), codec.MAX_ENCODED)
        # Exact raw projection changes cannot be papered over by recomputing
        # transport hashes or retaining only the successful history tail.
        bad = replace(plan, resolution=replace(value, pending=replace(value.pending, previous_files=())))
        with self.assertRaises(codec.HandoffRefusal): codec.encode(bad)

    def test_full_pending_missing_or_overlapping_later_attempt_refuses_before_custody(self):
        for variant in ('missing', 'overlap'):
            value, history = self.retained_plan(variant=variant)
            with self.subTest(variant=variant), \
                 mock.patch.object(w.common.Output, 'set_outputs', side_effect=AssertionError('partial output')), \
                 mock.patch.object(w.transport.runtime, 'bounded_command', side_effect=AssertionError('provider')):
                with self.assertRaises(w.WriterRefusal):
                    w.prepare_promotion(value, history, fixture.images(value), fixture.execution(),
                                        staging='passed', drills=fixture.drills(value))

    def test_real_pending_introduction_appends_dormant_expand_without_activating_target(self):
        source = fixture.composition.source
        source.ForwardPendingTests.setUpClass()
        case = source.ForwardPendingTests(); case.setUp()
        self.addCleanup(case.doCleanups)
        previous = case.add(); case.profile(previous['sha'])
        sha = case.candidate(fixture.composition.fixtures.SHA); case.install(sha)
        value = case.forward(sha)
        self.assertEqual((value.classification.kind, value.classification.dormant, value.classification.reasons),
                         ('expand', True, ()))
        self.assertEqual(value.classification.requirements, ('compat-required', 'generated-migration-closure-ci'))
        self.assertEqual(value.pending.previous_files, ())
        projection = case.replay(value)
        self.assertEqual(len(projection.plan.entries[0].objects), 4452)
        self.assertEqual(projection.binding.target_version, 1)
        active = value.resolved.latest.live.catalog
        self.assertEqual(value.candidate.catalog, active)
        history = fixture.composition.collected(value)
        observation = fixture.drills(value)
        self.assertFalse(observation.required)
        plan = w.prepare_promotion(value, history, fixture.images(value), fixture.execution(),
                                   staging='passed', drills=observation)
        outputs = codec.encode(plan)
        restored = self.decode(outputs, record_attempt=4)
        self.assertEqual(restored, plan)
        self.assertEqual(restored.drills, observation)
        self.assertEqual(restored.resolution.pending, value.pending)
        raw = self.build_record(outputs, record_attempt=4, current_resolution=value.resolved)
        manifest = w.common.loads_strict(raw)
        self.assertEqual((manifest['schema_version'], manifest['db_change'], manifest['dormant']), (2, 'expand', True))
        self.assertEqual({key: manifest[key] for key in active._fields}, active._asdict())
        self.assertEqual((manifest['catalog_version'], manifest['min_reader_version']), (0, 0))
        after = fixture.appended(value.resolved, raw, value.candidate)
        self.assertEqual(after.latest.live.catalog, active)
        self.assertEqual(after.latest.live.origin_source.catalog, active)
        self.assertNotEqual(after.latest.live.catalog.catalog_version, projection.binding.target_version)
        type(self).measurements['pending_introduction_public_custody'] = {
            'target_objects': 4452, 'target_version': 1, 'classification': 'expand', 'dormant': True,
            'previous_inventory_observed_empty': True, 'public_encode_decode_build_record': True,
            'original_drills_retained': True, 'live_catalog_version': manifest['catalog_version'],
            'live_min_reader_version': manifest['min_reader_version'], 'active_catalog_unchanged': True,
            'manifest_sha256': hashlib.sha256(raw).hexdigest(),
            'encoded_bytes': int(outputs['handoff_encoded_length']),
            'chunks': int(outputs['handoff_chunk_count']),
            'collected_command_count': history.command_count, 'collected_captured_bytes': history.captured_bytes}

    def test_complete_large_pending_history_refuses_unchanged_caps_without_truncation(self):
        value, history = self.retained_plan(large=True)
        self.assertEqual(len(history.history.attempts), 400)
        plan = w.prepare_promotion(value, history, fixture.images(value), fixture.execution(),
                                   staging='passed', drills=fixture.drills(value))
        self.assertEqual(plan.facts.compat_run_id, 499)
        with mock.patch.object(w.common.Output, 'set_outputs', side_effect=AssertionError('partial output')), \
             mock.patch.object(w.transport.runtime, 'bounded_command', side_effect=AssertionError('provider')):
            with self.assertRaises(codec.HandoffRefusal): codec.encode(plan)
        self.assertEqual(len(plan.collected.history.attempts), 400)
        type(self).measurements['pending_complete_history_over_cap'] = {
            'history_attempts': 400, 'qualification_completed': True,
            'collected_command_count': history.command_count, 'collected_captured_bytes': history.captured_bytes,
            'encode_refused': True, 'caps_unchanged': True, 'no_truncation': True}


if __name__ == '__main__':
    unittest.main()
