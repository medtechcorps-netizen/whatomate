"""Advisory compatibility workflow contracts and real local publication gates.

No GitHub, Docker or network calls. YAML uses the existing stdlib subset reader;
mutations must fail the policy rather than merely match a source-file hash.
"""
from __future__ import annotations

import copy
import ast
import io
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
sys.path.insert(0, str(ROOT / 'release/deployment'))
from test_ship_workflow import MiniYaml
from test_ci_workflows import WorkflowYaml
sys.path.insert(0, str(HERE))
import cli
import test_cli

PATH = ROOT / '.github/workflows/compat.yml'
SOURCE = PATH.read_text(encoding='utf-8')
DOCUMENT = MiniYaml(SOURCE).parse()
CHECKOUT = 'actions/checkout@11d5960a326750d5838078e36cf38b85af677262'
UPLOAD = 'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02'
DOWNLOAD = 'actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093'
ATTEST = 'actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6'
SUFFIX = '${{ github.sha }}-${{ github.run_id }}-${{ github.run_attempt }}'
RESOLUTION = 'compat-resolution-' + SUFFIX
MEASURED = 'compat-measured-' + SUFFIX
CANONICAL = 'compat-result-${{ github.sha }}-${{ github.run_attempt }}'
CHECK_COMMAND = '/usr/bin/python3 -I -S -B release/compat/cli.py check'
PUBLIC_CHECK = '${{ runner.temp }}/compat-check-' + SUFFIX + '/public/compat-result.json'
PUBLIC_CONTEXT = PUBLIC_CHECK.replace('compat-result.json','compat-context.json')
PUBLIC_SIGN = PUBLIC_CHECK.replace('/compat-check-','/compat-validate-evidence-')
PUBLIC_SIGN_CONTEXT = PUBLIC_SIGN.replace('compat-result.json','compat-context.json')
READY_IF = "${{ always() && !cancelled() && steps.check.outputs.complete == 'true' && steps.canonical.outcome == 'success' }}"
ATTEST_IF = "${{ always() && !cancelled() && needs.compat.outputs.evidence_ready == 'true' && (github.event_name == 'push' || github.event_name == 'workflow_dispatch') }}"
READ_PERMISSIONS = {'contents':'read', 'actions':'read', 'attestations':'read'}


def require(value, message):
    if not value:
        raise AssertionError(message)


def step(job, *, name=None, identifier=None):
    found = [item for item in job['steps'] if (name is not None and item.get('name') == name)
             or (identifier is not None and item.get('id') == identifier)]
    require(len(found) == 1, 'one exact step required')
    return found[0]


def heredoc(script):
    require(script.count("<<'PY'\n") == 1, 'one Python heredoc')
    body, end, _ = script.split("<<'PY'\n", 1)[1].partition('\nPY\n')
    require(bool(end), 'closed Python heredoc')
    return body


def condition(expression, values, *, cancelled=False):
    """Evaluate the narrow actual job conditions, never external expressions."""
    require(expression.startswith('${{ ') and expression.endswith(' }}'), 'closed condition')
    source = expression[4:-3].replace('always()', 'True').replace('cancelled()', str(cancelled))
    names = {}
    for index, key in enumerate(sorted(values, key=len, reverse=True)):
        name = 'value_' + str(index)
        source = source.replace(key, name)
        names[name] = values[key]
    source = source.replace('&&', 'and').replace('||', 'or').replace('!', 'not ')
    parsed = ast.parse(source, mode='eval')
    allowed = (ast.Expression, ast.BoolOp, ast.And, ast.Or, ast.UnaryOp, ast.Not,
               ast.Compare, ast.Eq, ast.Name, ast.Load, ast.Constant)
    require(all(isinstance(node, allowed) for node in ast.walk(parsed)), 'closed expression grammar')
    require(all(node.id in names for node in ast.walk(parsed) if isinstance(node, ast.Name)), 'bound names')
    return bool(eval(compile(parsed, '<workflow-condition>', 'eval'), {'__builtins__': {}}, names))


def validate(doc):
    require(doc['on'] == {'push':{'branches':['main']}, 'pull_request':None,
        'workflow_dispatch':{'inputs':{'rollback_target':{'description':'Optional signed record tag or prod-0000',
        'required':False, 'type':'string', 'default':''}}}}, 'all PRs and main pushes, no filters or privileged event')
    require(doc['permissions'] == {}, 'no ambient token permissions')
    require('concurrency' not in doc, 'no cancellation or unrelated release lock')
    require(doc['env']['COMPAT_ROLLBACK_TARGET'] == "${{ inputs.rollback_target || '' }}", 'target only via env')
    require(doc['defaults'] == {'run':{'shell':'bash'}}, 'fixed shell')
    jobs = doc['jobs']
    require(set(jobs) == {'release-preview','compat-run','compat','attest'}, 'closed job graph')
    for name, job in jobs.items():
        require(job['runs-on'] == 'ubuntu-24.04' and 'environment' not in job and not job.get('continue-on-error'),
                'hosted advisory jobs, no deployment environment or hidden job failure')
        require(job['name'] == ('compat-attest' if name == 'attest' else name), 'non-colliding explicit contexts')
        expected = READ_PERMISSIONS if name in ('release-preview','compat-run') else {'contents':'read','actions':'read'}
        if name == 'attest': expected = {**expected, 'attestations':'write', 'id-token':'write'}
        require(job['permissions'] == expected, 'least privilege per job')
        for item in job['steps']:
            if 'uses' in item:
                require(item['uses'] in {CHECKOUT,UPLOAD,DOWNLOAD,ATTEST}, 'pinned repository-precedent actions only')
            if item.get('uses') == CHECKOUT:
                require(item['with'].get('ref') == '${{ github.sha }}'
                        and item['with'].get('persist-credentials') is False, 'exact source without persisted token')
            if 'run' in item:
                require('${{' not in item['run'] and 'inputs.' not in item['run'], 'no expression interpolation in scripts')
                require(not re.search(r'(docker\s+(?:login|build|run)|buildx\s+(?:create|build)|gh\s+api)', item['run']),
                        'module owns builds and trust; workflow does not duplicate them')
            if item.get('continue-on-error'):
                require(name == 'compat' and item.get('id') == 'check', 'only measured verdict can continue')
            require('environment' not in item, 'no deploy step')
    text = json.dumps(doc)
    require('secrets.' not in text and 'GITHUB_RUN_ATTEMPT" == "1' not in text, 'no deployment secrets or attempt guard')
    require('run_attempt' in text, 'attempt-specific identities')
    preview, run, aggregate, attest = (jobs[key] for key in ('release-preview','compat-run','compat','attest'))
    require('needs' not in preview and 'if' not in preview, 'preview is first and unconditional')
    require(run['needs'] == 'release-preview' and 'if' not in run, 'experiment consumes successful preview')
    require(step(preview, identifier='preview')['run'] == CHECK_COMMAND.replace(' check',' preview'), 'one trusted preview')
    require(step(run, identifier='experiment')['run'] == CHECK_COMMAND.replace(' check',' run'), 'one bound experiment')
    for job in (preview,run):
        checkout = next(item for item in job['steps'] if item.get('uses') == CHECKOUT)
        require(checkout['with'].get('fetch-depth') == 0, 'trust/classification needs complete source history')
    expected_downloads = {'compat-run':[RESOLUTION], 'compat':[RESOLUTION,MEASURED], 'attest':[RESOLUTION,CANONICAL]}
    for name, names in expected_downloads.items():
        downloads = [item for item in jobs[name]['steps'] if item.get('uses') == DOWNLOAD]
        require([item['with']['name'] for item in downloads] == names, 'exact artifact identity and order')
        for item in downloads:
            require(item['with'] == {'name':item['with']['name'], 'repository':'${{ github.repository }}',
                'run-id':'${{ github.run_id }}', 'github-token':'${{ github.token }}',
                'path':'${{ runner.temp }}/compat-input'}, 'same-run named download, no pattern or external repository')
    require(not any(item.get('uses') == DOWNLOAD for item in preview['steps']), 'preview cannot reuse earlier evidence')
    for job, expected_name, expected_path in [
        (preview, RESOLUTION, '${{ runner.temp }}/compat-preview-'+SUFFIX+'/public/resolution.json'),
        (run, MEASURED, '${{ runner.temp }}/compat-run-'+SUFFIX+'/public/compat-result.json'),
        (aggregate, CANONICAL, PUBLIC_CHECK + "\n" + PUBLIC_CONTEXT + "\n")]:
        uploads = [item for item in job['steps'] if item.get('uses') == UPLOAD]
        require(len(uploads) == 1, 'one public file upload')
        upload = uploads[0]
        require(upload['with'] == {'name':expected_name, 'path':expected_path, 'if-no-files-found':'error',
            'retention-days':7} and upload['timeout-minutes'] == 1, 'bounded exact-file publication')
        if job is not aggregate:
            require('if' not in upload, 'partial/failed producer never uploads')
    require(aggregate.get('outputs') == {'evidence_ready':'${{ steps.ready.outputs.evidence_ready }}'},
            'readiness is from the post-upload guarded marker only')
    ready = step(aggregate,identifier='ready')
    require(ready['if'] == READY_IF and ready['run'].strip().splitlines() ==
            ['set -euo pipefail', r'''printf '%s\n' 'evidence_ready=true' >> "$GITHUB_OUTPUT"'''],
            'cancelled, incomplete or unpublished evidence never marks ready')
    require(aggregate['steps'].index(step(aggregate,identifier='canonical')) < aggregate['steps'].index(ready)
            < aggregate['steps'].index(step(aggregate,name='Require proof and publication success')),
            'publication precedes readiness, negative verdict enforcement follows')
    require(aggregate['needs'] == ['release-preview','compat-run'] and aggregate['if'] == '${{ always() }}',
            'aggregate observes both failures and successes')
    check = step(aggregate, identifier='check')
    require(check['run'] == CHECK_COMMAND and check['continue-on-error'] is True, 'retain measured negative artifact')
    expected_env = {'COMPAT_PREVIEW_OUTCOME':'${{ needs.release-preview.result }}',
                    'COMPAT_RUN_OUTCOME':'${{ needs.compat-run.result }}'}
    require(check['env'] == expected_env, 'check binds upstream results')
    canonical = step(aggregate, identifier='canonical')
    require(canonical['if'] == "${{ always() && steps.check.outputs.complete == 'true' }}", 'complete marker gates all publication')
    gate = step(aggregate, name='Require proof and publication success')
    require(gate['if'] == '${{ always() }}' and gate['env'] == {
        'PREVIEW_RESULT':'${{ needs.release-preview.result }}', 'RUN_RESULT':'${{ needs.compat-run.result }}',
        'CHECK_RESULT':'${{ steps.check.outcome }}', 'UPLOAD_RESULT':'${{ steps.canonical.outcome }}'}, 'real outcomes gate success')
    require(attest['needs'] == ['release-preview','compat-run','compat'] and attest['if'] ==
        ATTEST_IF, 'only complete published evidence can sign, including measured negatives')
    recheck = step(attest, identifier='check')
    require(recheck['run'] == CHECK_COMMAND.replace(' check',' validate-evidence') and recheck['env'] == expected_env
            and 'continue-on-error' not in recheck, 'attestation independently checks canonical identity')
    sign = step(attest, identifier='sign')
    require(sign['uses'] == ATTEST and sign['if'] ==
        "${{ steps.check.outcome == 'success' && steps.check.outputs.complete == 'true' }}" and sign['with'] == {
        'subject-path':PUBLIC_SIGN, 'push-to-registry':False}, 'attest only revalidated result bytes')
    sign_context = step(attest,identifier='sign-context')
    require(sign_context['uses'] == ATTEST and sign_context['if'] ==
        "${{ steps.check.outcome == 'success' && steps.check.outputs.complete == 'true' && steps.sign.outcome == 'success' }}"
        and sign_context['with'] == {'subject-path':PUBLIC_SIGN_CONTEXT, 'push-to-registry':False},
        'context independently signed after validated result, with fixed path')
    require(attest['timeout-minutes'] == 6 and sign['timeout-minutes'] == sign_context['timeout-minutes'] == 1,
            'three setup minutes plus two bounded signing minutes leave one minute for revalidation')
    require(not any(item.get('uses') == ATTEST for name,job in jobs.items() if name != 'attest' for item in job['steps']),
            'no other signer')
    final = step(attest, name='Require the attestation to complete')
    require(final['if'] == '${{ always() }}' and final['env'] == {
        'CHECK_RESULT':'${{ steps.check.outcome }}', 'SIGN_RESULT':'${{ steps.sign.outcome }}',
        'CONTEXT_SIGN_RESULT':'${{ steps.sign-context.outcome }}'}, 'either missing signature fails')
    require(run['timeout-minutes'] == 45 and step(run,identifier='experiment')['timeout-minutes'] == 41,
            'canonical job and one aggregate experiment budget')
    setup = [step(run,name=name) for name in ('Check out the exact candidate',"Download this run's exact resolution",
             'Install pinned anonymous build and read-only release tools')]
    require(all(item['timeout-minutes'] == 1 for item in setup), 'setup includes checkout/download/tools within180')
    require(run['steps'][0]['name'] == 'Start the aggregate setup clock' and
            run['steps'].index(step(run,name='Enforce the shared setup allocation')) < run['steps'].index(step(run,identifier='experiment')),
            'measure setup before any source/artifact/tool work')
    for job in (preview,run):
        install = next(item for item in job['steps'] if 'Install pinned' in item['name'])
        script = install['run']
        require('sha256sum --check --strict' in script and '--connect-timeout 10 --max-time 20' in script
                and 'mkdir -m 0700 "$tools"' in script and '--retry' not in script, 'bounded pinned tools in fresh scratch')
    require(doc['env']['PINNED_GH_SHA256'] == '3b8ac6b30336802fc1a858d7c084e11cdf24ac1a761ca90b68022d7d729208de'
            and doc['env']['PINNED_BUILDX_SHA256'] == '48af8a397ebd60178778bf63611dbcebe5f5e7a9be90eb9147b24b9587455778',
            'repository-precedent tool pins')


class WorkflowTests(unittest.TestCase):
    def test_current_workflow_contract(self):
        validate(DOCUMENT)

    def test_canonical_ci_discovers_compat_alongside_existing_suites(self):
        ci = WorkflowYaml((ROOT/'.github/workflows/test.yml').read_text(encoding='utf-8')).parse()
        job = ci['jobs']['release-tests']
        test_step = step(job, name='Test the Release workflow, its modules and the CI workflows')
        expected = ['set -euo pipefail'] + [
            "python3 -B -m unittest discover -s release/%s -p 'test_*.py' -v" % name
            for name in ('deployment', 'staging', 'compat')]
        self.assertEqual(test_step['run'].strip().splitlines(), expected)
        self.assertNotIn('if', test_step)
        self.assertNotIn('continue-on-error', test_step)
        self.assertNotIn('if', job)
        self.assertNotIn('continue-on-error', job)
        self.assertIn('release-tests', ci['jobs']['test']['needs'])

    def test_action_and_tool_pins_match_existing_workflows(self):
        source = (ROOT/'.github/workflows/ship.yml').read_text(encoding='utf-8')
        for action in (CHECKOUT,DOWNLOAD,UPLOAD,ATTEST): self.assertIn(action, source)
        for key in ('PINNED_GH_VERSION','PINNED_GH_URL','PINNED_GH_SHA256','PINNED_BUILDX_URL','PINNED_BUILDX_SHA256'):
            self.assertIn(str(DOCUMENT['env'][key]), source)

    def test_internal_budget_fits_without_cutting_off_core_cleanup(self):
        import images
        self.assertEqual(images._TOTAL,2400)
        self.assertEqual(45*60-(180+images._TOTAL+5+60),55)
        self.assertGreater(41*60,images._TOTAL+5)

    def test_security_and_identity_mutations_refuse(self):
        mutations = [
            lambda d:d['on']['pull_request'].update(paths=['internal/**']) if d['on']['pull_request'] else d['on'].update(pull_request={'paths':['internal/**']}),
            lambda d:d['on'].update(pull_request_target=None),
            lambda d:d.update(permissions={'contents':'write'}),
            lambda d:d['jobs']['compat-run'].update(environment='production'),
            lambda d:d['jobs']['compat-run'].update(**{'continue-on-error':True}),
            lambda d:d['jobs']['compat-run']['permissions'].update(packages='write'),
            lambda d:d['jobs']['compat-run'].update(needs=[]),
            lambda d:d['jobs']['compat'].update(**{'if':'${{ success() }}'}),
            lambda d:d['jobs']['compat']['steps'][0].update(uses='actions/checkout@main'),
            lambda d:d['jobs']['compat']['steps'][0]['with'].update(**{'persist-credentials':True}),
            lambda d:d['jobs']['compat']['steps'][1]['with'].update(**{'run-id':'123'}),
            lambda d:d['jobs']['compat']['steps'][1]['with'].update(pattern='compat-*'),
            lambda d:d['jobs']['compat']['steps'][2]['with'].update(name='compat-measured-latest'),
            lambda d:step(d['jobs']['compat'],identifier='canonical').update(**{'if':'${{ always() }}'}),
            lambda d:step(d['jobs']['compat'],identifier='canonical')['with'].update(path='${{ runner.temp }}'),
            lambda d:step(d['jobs']['compat'],identifier='check').update(**{'continue-on-error':False}),
            lambda d:step(d['jobs']['compat'],name='Require proof and publication success')['env'].update(CHECK_RESULT='${{ steps.check.conclusion }}'),
            lambda d:d['jobs']['attest'].update(**{'if':'${{ success() }}'}),
            lambda d:step(d['jobs']['attest'],identifier='check').update(**{'continue-on-error':True}),
            lambda d:step(d['jobs']['attest'],identifier='check').update(run=CHECK_COMMAND),
            lambda d:d['jobs']['attest'].update(**{'if':"${{ needs.compat.result == 'success' }}"}),
            lambda d:d['jobs']['compat']['outputs'].update(evidence_ready='true'),
            lambda d:step(d['jobs']['compat'],identifier='ready').update(**{'if':"${{ always() }}"}),
            lambda d:step(d['jobs']['attest'],identifier='sign-context')['with'].update(**{'subject-path':PUBLIC_SIGN}),
            lambda d:step(d['jobs']['attest'],identifier='sign')['with'].update(**{'subject-path':'${{ runner.temp }}/compat-input/compat-result.json'}),
            lambda d:d['jobs']['compat-run'].update(**{'timeout-minutes':90}),
            lambda d:step(d['jobs']['compat-run'],identifier='experiment').update(run='echo ${{ inputs.rollback_target }}'),
            lambda d:step(d['jobs']['compat-run'],name='Check out the exact candidate').update(**{'timeout-minutes':2}),
            lambda d:step(d['jobs']['compat-run'],name='Upload only the complete measured result').update(**{'if':'${{ always() }}'}),
        ]
        for index, mutate in enumerate(mutations):
            document = copy.deepcopy(DOCUMENT); mutate(document)
            with self.subTest(index=index), self.assertRaises(AssertionError): validate(document)

    def test_actual_aggregate_script_rejects_any_failed_missing_or_skipped_outcome(self):
        script = heredoc(step(DOCUMENT['jobs']['compat'],name='Require proof and publication success')['run'])
        names = ('PREVIEW_RESULT','RUN_RESULT','CHECK_RESULT','UPLOAD_RESULT')
        values = dict.fromkeys(names,'success')
        with patch.dict(os.environ,values): exec(compile(script,'<aggregate>','exec'),{})
        for name in names:
            for value in ('failure','cancelled','skipped',''):
                with self.subTest(name=name,value=value),patch.dict(os.environ,{**values,name:value}),self.assertRaises(SystemExit):
                    exec(compile(script,'<aggregate>','exec'),{})

    def test_actual_attestation_script_requires_both_signatures_and_revalidation(self):
        script = heredoc(step(DOCUMENT['jobs']['attest'],name='Require the attestation to complete')['run'])
        names = ('CHECK_RESULT','SIGN_RESULT','CONTEXT_SIGN_RESULT')
        values = dict.fromkeys(names,'success')
        for value in ('success','failure','cancelled','skipped',''):
            for name in names:
                with self.subTest(name=name,value=value), patch.dict(os.environ,{**values,name:value}), \
                     self.assertRaises(SystemExit) as raised:
                    exec(compile(script,'<attestation>','exec'),{})
                self.assertEqual(raised.exception.code,0 if value == 'success' else 1)

    def test_complete_negative_can_sign_but_keeps_compat_and_workflow_failed(self):
        aggregate = DOCUMENT['jobs']['compat']; attest = DOCUMENT['jobs']['attest']
        gate = heredoc(step(aggregate,name='Require proof and publication success')['run'])
        ready_if = step(aggregate,identifier='ready')['if']
        for verdict in ('success','failure'):
            values = {'steps.check.outputs.complete':'true','steps.canonical.outcome':'success'}
            self.assertTrue(condition(ready_if,values))
            for event in ('push','workflow_dispatch','pull_request'):
                self.assertEqual(condition(attest['if'],{
                    'needs.compat.outputs.evidence_ready':'true','github.event_name':event}),
                    event != 'pull_request')
            env = dict(PREVIEW_RESULT='success',RUN_RESULT='success',CHECK_RESULT=verdict,UPLOAD_RESULT='success')
            if verdict == 'failure':
                with patch.dict(os.environ,env), self.assertRaises(SystemExit) as raised:
                    exec(compile(gate,'<aggregate>','exec'),{})
                self.assertNotEqual(raised.exception.code,0)
            else:
                with patch.dict(os.environ,env): exec(compile(gate,'<aggregate>','exec'),{})
        # The aggregate job failure is not hidden, even though its signing dependent can run.
        self.assertNotIn('continue-on-error',aggregate)
        self.assertNotIn('continue-on-error',attest)

    def test_cancellation_incomplete_write_upload_failure_or_missing_marker_never_signs(self):
        aggregate = DOCUMENT['jobs']['compat']; attest = DOCUMENT['jobs']['attest']
        ready_if = step(aggregate,identifier='ready')['if']
        for complete in ('true','false',''):
            for upload in ('success','failure','cancelled','skipped',''):
                for cancelled in (False,True):
                    with self.subTest(complete=complete,upload=upload,cancelled=cancelled):
                        ready = condition(ready_if,{'steps.check.outputs.complete':complete,
                            'steps.canonical.outcome':upload},cancelled=cancelled)
                        self.assertEqual(ready,complete == 'true' and upload == 'success' and not cancelled)
                        self.assertEqual(condition(attest['if'],{
                            'needs.compat.outputs.evidence_ready':'true' if ready else '',
                            'github.event_name':'push'},cancelled=cancelled),ready)
        self.assertFalse(condition(attest['if'],{'needs.compat.outputs.evidence_ready':'true',
            'github.event_name':'workflow_dispatch'},cancelled=True))

    def test_actual_setup_clock_boundaries(self):
        script = heredoc(step(DOCUMENT['jobs']['compat-run'],name='Enforce the shared setup allocation')['run'])
        for elapsed in (0,179.9,180):
            with patch.dict(os.environ,{'COMPAT_SETUP_STARTED':str(1000-elapsed)}),patch('time.monotonic',return_value=1000):
                exec(compile(script,'<setup>','exec'),{})
        for elapsed in (-1,180.001,float('nan'),float('inf')):
            with patch.dict(os.environ,{'COMPAT_SETUP_STARTED':str(1000-elapsed)}),patch('time.monotonic',return_value=1000),self.assertRaises(SystemExit):
                exec(compile(script,'<setup>','exec'),{})

    def test_real_cli_complete_marker_controls_workflow_publication(self):
        for case in ('positive','negative','truncated','foreign-identity','partial-write','upstream-failed'):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as name:
                root=Path(name).resolve();repo=root/'repo';repo.mkdir();runner=root/'runner';runner.mkdir()
                inputs=runner/'compat-input';inputs.mkdir();output=runner/'step-output';output.touch()
                env=test_cli.environment();env.update(RUNNER_TEMP=str(runner),GITHUB_OUTPUT=str(output),
                    COMPAT_PREVIEW_OUTCOME='success',COMPAT_RUN_OUTCOME='success')
                ctx=cli.context(env);snapshot=test_cli.snapshot(ctx);value=test_cli.result(ctx)
                if case=='negative':value['old_server_ok']['read']=False
                if case=='foreign-identity':value['run_id']+=1
                raw=cli.proof.result_bytes(value)
                if case=='truncated':raw=raw[:-5]
                if case=='upstream-failed':env['COMPAT_RUN_OUTCOME']='failure'
                (inputs/'resolution.json').write_bytes(cli.canonical(snapshot));(inputs/'compat-result.json').write_bytes(raw)
                previous=Path.cwd()
                try:
                    os.chdir(repo)
                    with patch.dict(os.environ,env,clear=True),patch.object(cli.images,'_READY_FOR_EXECUTION',True),patch('sys.stderr',io.StringIO()),patch('sys.stdout',io.StringIO()):
                        if case=='partial-write':
                            def partial_write(path, data):
                                with path.open('xb') as stream: stream.write(data[:1])
                                raise OSError('synthetic partial write')
                            with patch.object(cli,'_write_public',side_effect=partial_write):status=cli.main(['check'])
                        else:status=cli.main(['check'])
                finally:os.chdir(previous)
                complete='complete=true\n' in output.read_text()
                self.assertEqual(complete,case in ('positive','negative'))
                self.assertEqual(status,0 if case=='positive' else 1)
                if complete:
                    public=runner/('compat-check-'+ctx.suffix)/'public/compat-result.json'
                    self.assertEqual(public.read_bytes(),raw)
                    context_raw=public.with_name('compat-context.json').read_bytes()
                    cli.validate_evidence(context_raw,raw,snapshot,ctx)
                    self.assertEqual({p.name for p in public.parent.iterdir()},
                                     {'compat-result.json','compat-context.json'})
                if case=='partial-write':
                    public=runner/('compat-check-'+ctx.suffix)/'public/compat-result.json'
                    self.assertEqual(public.read_bytes(),raw[:1])


if __name__ == '__main__':
    unittest.main()
