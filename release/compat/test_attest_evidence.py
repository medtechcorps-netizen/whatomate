"""Synthetic public bytes exercise the real signing preparer, never authority."""
import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import attest_evidence as preparer
import test_cli as normal
import test_public_evidence as fixtures


_COLD = r"""import contextlib,importlib.util,io,json,os,pathlib,sys
path=pathlib.Path(sys.argv[1]); expected=int(sys.argv[2]); repository=path.parents[2]
assert sys.flags.isolated and sys.flags.no_site and sys.dont_write_bytecode
blocked={'cli','compat','catalog','images','postgres','processes','observation','runtime',
 'record_resolver','record_transport','schema_change','ship_outage_compat','record_handoff',
 'negative_index','compat_negative_transport','test_cli','test_public_evidence'}
assert not blocked.intersection(sys.modules)
class RejectOperational:
 def find_spec(self,fullname,path=None,target=None):
  if fullname.split('.')[0] in blocked: raise AssertionError('operational import')
sys.meta_path.insert(0,RejectOperational())
def audit(event,args):
 if event in ('subprocess.Popen','os.system') or event.startswith('socket.'):
  raise AssertionError('native/network effect')
sys.addaudithook(audit)
spec=importlib.util.spec_from_file_location('attest_evidence_cold',path)
module=importlib.util.module_from_spec(spec); sys.modules[spec.name]=module
spec.loader.exec_module(module)
stdout,stderr=io.StringIO(),io.StringIO()
with contextlib.redirect_stdout(stdout),contextlib.redirect_stderr(stderr): code=module.main([])
assert code==expected and stdout.getvalue()==''
assert stderr.getvalue()==('' if expected==0 else 'attest-evidence-refused\n')
assert not blocked.intersection(sys.modules)
loaded=[]
for name,mod in tuple(sys.modules.items()):
 value=getattr(mod,'__file__',None)
 if value and pathlib.Path(value).absolute().is_relative_to(repository): loaded.append(name)
assert 'public_evidence' in loaded
print(json.dumps({'code':code,'loaded':sorted(loaded)},sort_keys=True))
"""


class PreparerTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='attest-preparer-')
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.repo = Path(preparer.__file__).resolve().parents[2]
        self.inputs = self.root / 'compat-input'; self.inputs.mkdir()
        self.output = self.root / 'outputs'; self.output.write_bytes(b'')
        self.env = normal.environment()
        self.env.update(RUNNER_TEMP=str(self.root), GITHUB_OUTPUT=str(self.output),
                        COMPAT_PREVIEW_OUTCOME='success', COMPAT_RUN_OUTCOME='success')
        self.ctx = preparer.context(self.env)
        self.snapshot = normal.snapshot(self.ctx)
        self.result = preparer.public.proof.result_bytes(normal.result(self.ctx))
        self.context = preparer.canonical(preparer.public.evidence_context(self.result, self.snapshot, self.ctx))
        self.install()

    @property
    def work(self):
        return self.root / ('compat-validate-evidence-' + self.ctx.suffix)

    def install(self):
        for name, raw in {'resolution.json': preparer.canonical(self.snapshot),
                          'compat-result.json': self.result, 'compat-context.json': self.context}.items():
            (self.inputs / name).write_bytes(raw)

    def rollback(self, **options):
        self.ctx, self.snapshot, self.result, self.context = fixtures.rollback_documents(**options)
        self.env.update(GITHUB_SHA=self.ctx.sha, GITHUB_WORKFLOW_SHA=self.ctx.sha,
            GITHUB_EVENT_NAME=self.ctx.event, GITHUB_REF=self.ctx.ref,
            GITHUB_WORKFLOW_REF=preparer.public._WORKFLOW + self.ctx.ref,
            GITHUB_RUN_ID=str(self.ctx.run_id), GITHUB_RUN_ATTEMPT=str(self.ctx.attempt),
            COMPAT_ROLLBACK_TARGET=self.ctx.target)
        self.install()

    def run_main(self, expected=0, argv=None):
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch.dict(os.environ, self.env, clear=True), \
             mock.patch.object(preparer.Path, 'cwd', return_value=self.repo), \
             contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = preparer.main([] if argv is None else argv)
        self.assertEqual(code, expected)
        self.assertEqual(stdout.getvalue(), '')
        self.assertEqual(stderr.getvalue(), '' if expected == 0 else 'attest-evidence-refused\n')
        return code

    def assert_complete(self):
        public = self.work / 'public'
        self.assertEqual({p.name for p in public.iterdir()}, {'compat-result.json', 'compat-context.json'})
        self.assertEqual((public / 'compat-result.json').read_bytes(), self.result)
        self.assertEqual((public / 'compat-context.json').read_bytes(), self.context)
        self.assertEqual(self.output.read_bytes(), b'complete=true\n')
        self.assertEqual((self.inputs / 'resolution.json').read_bytes(), preparer.canonical(self.snapshot))

    def refuse(self):
        self.run_main(1)
        self.assertEqual(self.output.read_bytes(), b'')

    def test_normal_positive_copies_exact_subjects_after_validation(self):
        self.run_main(); self.assert_complete()

    def test_complete_normal_negative_preserves_false_flags_and_marks_complete(self):
        value = json.loads(self.result)
        value['old_server_ok']['read'] = False
        value['forward'] = False
        self.result = preparer.public.proof.result_bytes(value)
        self.context = preparer.canonical(preparer.public.evidence_context(self.result, self.snapshot, self.ctx))
        self.install(); self.run_main(); self.assert_complete()
        self.assertFalse(preparer.public.accepted(json.loads(self.result)))

    def test_rollback_positive_copies_context4_and_distinct_roles(self):
        self.rollback(); self.run_main(); self.assert_complete()
        self.assertEqual(json.loads(self.context)['version'], 4)
        self.assertNotEqual(self.snapshot['sha'], self.snapshot['selected_web_source_sha'])

    def test_complete_rollback_negative_preserves_false_and_actual_catalog(self):
        self.rollback(server_false='read', migration_false='rw', forward=False, catalog_sha256='e'*64)
        self.run_main(); self.assert_complete()
        self.assertFalse(json.loads(self.result)['reversible'])
        self.assertEqual(json.loads(self.result)['catalog_sha256'], 'e'*64)

    def test_no_arguments_or_configurable_input_paths(self):
        for args in (['validate-evidence'], ['--input', str(self.inputs)], (), ['check']):
            with self.subTest(args=args): self.run_main(1, args)
        self.assertFalse(self.work.exists()); self.assertEqual(self.output.read_bytes(), b'')

    def test_upstream_failure_refuses_before_subject_creation(self):
        for key in ('COMPAT_PREVIEW_OUTCOME', 'COMPAT_RUN_OUTCOME'):
            for value in ('failure', 'cancelled', 'skipped', ''):
                with self.subTest(key=key, value=value):
                    with mock.patch.dict(self.env, {key:value}): self.refuse()
        self.assertFalse(self.work.exists())

    def test_context_identity_and_runner_mutations_refuse_before_writes(self):
        for key, value in [('GITHUB_REPOSITORY','other/repo'), ('GITHUB_SHA','f'*40),
                           ('GITHUB_WORKFLOW_SHA','f'*40), ('GITHUB_RUN_ID','654321'),
                           ('GITHUB_RUN_ATTEMPT','2'), ('RUNNER_ENVIRONMENT','self-hosted')]:
            with self.subTest(key=key), mock.patch.dict(self.env, {key:value}): self.refuse()
        self.assertFalse(self.work.exists())

    def test_snapshot_is_canonical_and_exactly_bound(self):
        path = self.inputs / 'resolution.json'; original = path.read_bytes()
        mutations = [original + b'\n', b'{}\n', original.replace(b'"run_attempt":1', b'"run_attempt":true'),
                     original.replace(self.ctx.sha.encode(), b'f'*40)]
        for raw in mutations:
            with self.subTest(digest=hashlib.sha256(raw).hexdigest()):
                path.write_bytes(raw); self.refuse()
        self.assertFalse(self.work.exists())

    def test_result_context_canonical_digest_selection_and_protocol_refuse(self):
        path = self.inputs / 'compat-context.json'; original = path.read_bytes()
        bad = json.loads(original); bad['result_sha256'] = '0'*64
        for raw in (original + b'\n', preparer.canonical(bad), b'{}\n'):
            with self.subTest(raw_hash=hashlib.sha256(raw).hexdigest()):
                path.write_bytes(raw); self.refuse()
        path.write_bytes(original)
        result = json.loads(self.result); result['previous_web_digest'] = 'sha256:'+'f'*64
        (self.inputs / 'compat-result.json').write_bytes(preparer.canonical(result)); self.refuse()
        self.assertFalse(self.work.exists())

    def test_rollback_cannot_reuse_normal_context_or_non_main_dispatch(self):
        normal_context = self.context
        self.rollback()
        (self.inputs / 'compat-context.json').write_bytes(normal_context); self.refuse()
        self.install()
        with mock.patch.dict(self.env, GITHUB_REF='refs/heads/other',
                             GITHUB_WORKFLOW_REF=preparer.public._WORKFLOW+'refs/heads/other'):
            self.refuse()
        self.assertFalse(self.work.exists())

    def test_input_inventory_requires_exact_three_files(self):
        extra = self.inputs / 'extra'; extra.write_bytes(b'ignored?'); self.refuse(); extra.unlink()
        (self.inputs / 'compat-context.json').unlink(); self.refuse()
        self.assertFalse(self.work.exists())

    def test_each_input_bound_refuses_without_scratch(self):
        for name, limit in [('resolution.json',16384), ('compat-result.json',16384), ('compat-context.json',4096)]:
            for raw in (b'', b'x'*(limit+1)):
                with self.subTest(name=name, size=len(raw)):
                    self.install(); (self.inputs / name).write_bytes(raw); self.refuse()
        self.assertFalse(self.work.exists())

    def test_input_symlink_refuses_without_following_source(self):
        source = self.root / 'linked-context'; source.write_bytes(self.context)
        path = self.inputs / 'compat-context.json'; path.unlink()
        try: path.symlink_to(source)
        except OSError as exc: self.skipTest('symlink unavailable: '+type(exc).__name__)
        self.refuse(); self.assertFalse(self.work.exists())

    def test_existing_scratch_is_not_reused_or_repaired(self):
        self.work.mkdir(); sentinel = self.work / 'owned-elsewhere'; sentinel.write_bytes(b'unchanged')
        self.refuse(); self.assertEqual(sentinel.read_bytes(), b'unchanged')
        self.assertEqual(list(self.work.iterdir()), [sentinel])

    def test_scratch_repository_overlap_refuses(self):
        self.repo = self.root
        self.refuse(); self.assertFalse(self.work.exists())

    def test_partial_second_write_never_marks_complete(self):
        original = preparer._write_public
        def partial(path, raw):
            if path.name == 'compat-context.json':
                path.write_bytes(raw[:10]); raise OSError('PRIVATE write path')
            original(path, raw)
        with mock.patch.object(preparer, '_write_public', side_effect=partial): self.refuse()
        self.assertTrue((self.work / 'public' / 'compat-result.json').exists())

    def test_readback_changed_result_refuses_and_never_marks_complete(self):
        original = preparer._write_public
        def corrupt(path, raw):
            original(path, raw)
            if path.name == 'compat-context.json':
                (path.parent / 'compat-result.json').write_bytes(b'{}\n')
        with mock.patch.object(preparer, '_write_public', side_effect=corrupt): self.refuse()

    def test_readback_extra_file_refuses_and_never_marks_complete(self):
        original = preparer._write_public
        def extra(path, raw):
            original(path, raw)
            if path.name == 'compat-context.json': (path.parent / 'extra').write_bytes(b'x')
        with mock.patch.object(preparer, '_write_public', side_effect=extra): self.refuse()

    def test_output_marker_path_must_be_owned_runner_temp(self):
        with tempfile.TemporaryDirectory() as outside:
            path = Path(outside).resolve() / 'output'; path.write_bytes(b'untouched')
            self.env['GITHUB_OUTPUT'] = str(path)
            self.refuse(); self.assertEqual(path.read_bytes(), b'untouched')

    def test_marker_failure_is_sanitized_and_not_success(self):
        with mock.patch.object(preparer.common.Output, 'set_outputs', side_effect=OSError('PRIVATE token')):
            self.refuse()

    def test_missing_optional_output_preserves_existing_local_behavior(self):
        del self.env['GITHUB_OUTPUT']
        self.run_main()
        self.assertEqual(self.output.read_bytes(), b'')
        self.assertEqual((self.work / 'public' / 'compat-result.json').read_bytes(), self.result)

    def test_rerun_of_same_identity_refuses_without_rewriting_subjects(self):
        self.run_main(); before = {p.name:p.read_bytes() for p in (self.work/'public').iterdir()}
        self.output.write_bytes(b''); self.refuse()
        self.assertEqual({p.name:p.read_bytes() for p in (self.work/'public').iterdir()}, before)

    def cold(self, expected):
        environment = {key:value for key,value in os.environ.items() if key.upper() in {
            'SYSTEMROOT','WINDIR','TEMP','TMP','USERPROFILE','LOCALAPPDATA','APPDATA',
            'PROGRAMFILES','PROGRAMFILES(X86)','COMSPEC','PATHEXT'}}
        environment.update(self.env)
        with tempfile.TemporaryDirectory(prefix='preparer-cold-') as temporary:
            logs = Path(temporary); stdout, stderr = logs/'stdout', logs/'stderr'
            with stdout.open('xb') as out, stderr.open('xb') as err:
                process = subprocess.run([sys.executable,'-I','-S','-B','-c',_COLD,
                    str(Path(preparer.__file__).resolve()),str(expected)], cwd=self.repo, env=environment,
                    stdin=subprocess.DEVNULL, stdout=out, stderr=err, timeout=30,
                    creationflags=getattr(subprocess,'CREATE_NO_WINDOW',0))
            self.assertLessEqual(stdout.stat().st_size + stderr.stat().st_size, 65536)
            self.assertEqual(process.returncode,0,stderr.read_text(encoding='utf-8'))
            self.assertEqual(stderr.read_bytes(), b'')
            report = json.loads(stdout.read_bytes())
        self.assertEqual(report['code'], expected)
        self.assertIn('public_evidence', report['loaded'])

    def test_cold_normal_entry_loads_no_operational_graph(self):
        self.cold(0); self.assert_complete()

    def test_cold_rollback_negative_entry_loads_no_operational_graph(self):
        self.rollback(server_false='read'); self.cold(0); self.assert_complete()

    def test_cold_bad_context_never_creates_subjects_or_marker(self):
        (self.inputs/'compat-context.json').write_bytes(b'{}\n')
        self.cold(1)
        self.assertFalse(self.work.exists()); self.assertEqual(self.output.read_bytes(), b'')


if __name__ == '__main__':
    unittest.main()
