"""Offline image admission tests; no Docker, network or image build.

Only tiny local Python children exercise pipe draining/deadline/reaping. Git
object responses and Docker responses are synthetic and explicitly mocked.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

sys.path.insert(0,str(Path(__file__).resolve().parent))
import images as m


class Clock:
    def __init__(self): self.value=10.
    def __call__(self): return self.value


class BudgetTests(unittest.TestCase):
    def test_one2400_budget_build1500_reserve900_cleanup180(self):
        clock=Clock();budget=m._Deadline(clock=clock)
        self.assertEqual((m._TOTAL,m._BUILD,m._CLEANUP),(2400,1500,180))
        self.assertEqual(budget.build_seconds(),1500)
        clock.value+=125
        self.assertEqual(budget.build_seconds(),1375)
        clock.value+=1375
        self.assertEqual(budget.experiment_seconds(),720)
        clock.value+=720
        self.assertEqual(budget.remaining(),180)
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'experiment-reserve'):budget.experiment_seconds()
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'build-reserve'):budget.build_seconds()
        clock.value+=180
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'deadline'):budget.remaining()

    def test_nonfinite_reversed_clock_and_late_capture_refuse(self):
        for value in (float('nan'),float('inf'),-1,True):
            with self.subTest(value=value),self.assertRaises(m.proof.EvidenceError):m._Deadline(clock=lambda:value)
        clock=Clock();budget=m._Deadline(clock=clock);clock.value-=1
        with self.assertRaisesRegex(m.proof.EvidenceError,'clock'):budget.remaining()
        clock=Clock();budget=m._Deadline(clock=clock)
        def capture(*args,**kwargs):
            self.assertEqual(kwargs['timeout'],600);clock.value+=2401
            return m.runtime.Capture(0,b'late',b'')
        with patch.object(m.runtime,'bounded_command',side_effect=capture),self.assertRaises(m.proof.InfrastructureFailure):
            budget.capture(['synthetic'],timeout=9999)

    def test_verifier_transports_restore_normal_and_exception(self):
        for failure in (False,True):
            client=object.__new__(m.runtime.ReadOnlyGh);client.capture=m.runtime.bounded_command
            original=m.runtime.release_record.git;budget=m._Deadline()
            try:
                with m._verification_transport(client,budget):
                    self.assertNotEqual(m.runtime.release_record.git,original)
                    if failure:raise ValueError('synthetic')
            except ValueError:pass
            self.assertIs(m.runtime.release_record.git,original)
            self.assertIs(client.capture,m.runtime.bounded_command)

    def test_verifier_foreign_replacement_untouched_and_no_call(self):
        client=object.__new__(m.runtime.ReadOnlyGh);client.capture=m.runtime.bounded_command
        original=m.runtime.release_record.git;calls=[]
        foreign=lambda *a,**kw:calls.append(1)
        try:
            with self.assertRaisesRegex(m.proof.EvidenceError,'transport-replaced'):
                with m._verification_transport(client,m._Deadline()):
                    client.capture=foreign
            self.assertIs(client.capture,foreign);self.assertEqual(calls,[])
        finally:m.runtime.release_record.git=original

    def test_git_bridge_clamps_without_changing_command_semantics(self):
        client=object.__new__(m.runtime.ReadOnlyGh);client.capture=m.runtime.bounded_command
        budget=m._Deadline();calls=[]
        def capture(argv,**kwargs):calls.append((argv,kwargs));return m.runtime.Capture(1,b'',b'')
        with patch.object(budget,'capture',side_effect=capture):
            with m._verification_transport(client,budget):
                result=m.runtime.release_record.git(Path('.'),['merge-base','--is-ancestor','a'*40,'b'*40],code='synthetic')
                self.assertEqual(result.returncode,1)
        self.assertEqual(calls[0][0][:3],['git','-C','.']);self.assertEqual(calls[0][1]['timeout'],30)


class SourceTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name).resolve()

    @staticmethod
    def reader(raw):
        source=object.__new__(m._Source);source.git=lambda *a,**kw:raw;return source

    def test_tree_closed_regular_paths_no_case_or_device_collisions(self):
        record=lambda name,mode='100644':mode.encode()+b' blob '+b'a'*40+b'\t'+name.encode()+b'\0'
        source=self.reader(record('a/file.go'))
        self.assertEqual(source.inventory('x'),{'a/file.go':'a'*40})
        for raw in (record('../outside'),record('a/../../x'),record('CON.txt'),record('a.'),
                    record('a','100755'),record('link','120000'),record('a')+record('A'),record('a')[:-1]):
            with self.subTest(raw=raw),self.assertRaises(m.proof.EvidenceError):self.reader(raw).inventory('x')

    def test_exact_blob_framing_and_git_content_identity(self):
        data=b'public source\n';oid=hashlib.sha1(b'blob '+str(len(data)).encode()+b'\0'+data).hexdigest()
        raw=oid.encode()+b' blob '+str(len(data)).encode()+b'\n'+data+b'\n'
        self.assertEqual(self.reader(raw).blobs({'a':oid}),{'a':data})
        for changed in (raw+b'junk',raw[:-1],raw.replace(b'public',b'changed'),raw.replace(b' blob ',b' tree ')):
            with self.subTest(changed=changed),self.assertRaises(m.proof.EvidenceError):self.reader(changed).blobs({'a':oid})

    def test_context_create_only_extra_missing_changed_refuse(self):
        root=self.root/'context';blobs={'a/b.py':b'a\n','c':b'c'}
        m._export(root,blobs)
        with self.assertRaises(m.proof.EvidenceError):m._export(root,blobs)
        (root/'c').write_bytes(b'changed')
        with self.assertRaises(m.proof.EvidenceError):m._check_export(root,blobs)
        (root/'c').write_bytes(b'c');(root/'extra').write_bytes(b'x')
        with self.assertRaises(m.proof.EvidenceError):m._check_export(root,blobs)
        (root/'extra').unlink();(root/'c').unlink()
        with self.assertRaises(m.proof.EvidenceError):m._check_export(root,blobs)

    def test_receipts_exact_bytes_names_and_duplicate_json(self):
        root=self.root/'receipts';root.mkdir();pins={}
        for role in ('graph','controller','fixture'):
            raw=json.dumps({'role':role}).encode();name=role+'-build.json';(root/name).write_bytes(raw)
            pins[role]=(name,m.sha(raw))
        with patch.object(m,'_RECEIPTS',pins):
            self.assertEqual(set(m._receipts(root)),set(pins))
            (root/'extra').write_bytes(b'{}')
            with self.assertRaises(m.proof.EvidenceError):m._receipts(root)
            (root/'extra').unlink();(root/'fixture-build.json').write_bytes(b'{}')
            with self.assertRaisesRegex(m.proof.EvidenceError,'receipt-changed'):m._receipts(root)
            raw=b'{"role":1,"role":2}';(root/'fixture-build.json').write_bytes(raw);pins['fixture']=('fixture-build.json',m.sha(raw))
            with self.assertRaisesRegex(m.proof.EvidenceError,'receipt-invalid'):m._receipts(root)

    def test_fixture_parity_allows_only_python_docs_not_go_or_recipe(self):
        old,image,recipe=m._SUPPORT['fixture']
        receipt={'source':old,'image_id':image,'recipe_sha256':recipe,'exit_code':0,'containers_unchanged':True}
        before={'a.go':'a','release/compat/x.py':'p','docs/compat.md':'d'}
        current={**before,'release/compat/x.py':'q','release/compat/new.py':'n','docs/compat.md':'e'}
        class Source:
            candidate='new'
            def inventory(self,commit):return before if commit==old else current
        with patch.object(m,'sha',return_value=recipe):
            m._supporting(Source(),{'release/compat/fixture.Dockerfile':b'fixture'},{'fixture':receipt})
            for name in ('a.go','release/staging/bootstrap/profile.go','go.mod','release/compat/fixture.Dockerfile'):
                saved=current.copy();current[name]='changed'
                with self.subTest(name=name),self.assertRaisesRegex(m.proof.EvidenceError,'fixture-input-changed'):
                    m._supporting(Source(),{'release/compat/fixture.Dockerfile':b'fixture'},{'fixture':receipt})
                current.clear();current.update(saved)

    def test_controller_every_build_input_requires_old_current_hash_equality(self):
        old,image,recipe=m._SUPPORT['controller']
        names=['release/compat/controller.Dockerfile','release/compat/controller.py',
               'release/compat/sweep.py','release/compat/proof.py']
        blobs={name:('synthetic '+name).encode() for name in names}
        rows=[{'path':name,'bytes':len(blobs[name]),'sha256':m.sha(blobs[name])} for name in names]
        receipt={'source':old,'image_id':image,'recipe_sha256':recipe,'containers_unchanged':True,'files':rows}
        source=SimpleNamespace(git=lambda args:('synthetic '+args[-1].split(':',1)[1]).encode())
        m._supporting(source,blobs,{'controller':receipt})
        for name in names:
            changed=dict(blobs);changed[name]+=b'changed'
            with self.subTest(name=name),self.assertRaisesRegex(m.proof.EvidenceError,'support-changed'):
                m._supporting(source,changed,{'controller':receipt})
        receipt['files']=rows[:-1]
        with self.assertRaises(m.proof.EvidenceError):m._supporting(source,blobs,{'controller':receipt})


class BuildTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)

    def test_fixed_command_no_push_auth_arbitrary_args(self):
        args=m._build_command(self.root/'docker-buildx.exe',self.root/'context',self.root,'a'*40)
        self.assertEqual(args[:4],[str(self.root/'docker-buildx.exe'),'build','--builder','default'])
        self.assertNotIn('--config',args);self.assertNotIn('--context',args)
        self.assertIn('--load',args);self.assertIn('--provenance=false',args);self.assertIn('--pull=false',args)
        self.assertEqual([args[i+1] for i,x in enumerate(args) if x=='--build-arg'],['TARGETOS=linux','TARGETARCH=amd64'])
        self.assertFalse(any(x in args for x in ('--push','--secret','--ssh','--cache-from','--cache-to')))
        labels=[args[i+1] for i,x in enumerate(args) if x=='--label']
        self.assertIn(m.stack.SOURCE_LABEL+'='+'a'*40,labels)
        self.assertIn(m.stack.RECIPE_LABEL+'='+m._WEB_RECIPE_SHA,labels)

    def test_real_small_child_both_pipes_and_nonzero_preserved(self):
        value=m._build_capture([sys.executable,'-I','-S','-c',
            'import sys;sys.stdout.write("o"*100000);sys.stderr.write("e"*100000);sys.exit(7)'],
            m.common.scrubbed_env(),5,m._Deadline())
        self.assertEqual((value.returncode,len(value.stdout),len(value.stderr)),(7,100000,100000))

    def test_real_timeout_kills_reaps_once_without_iid_adoption(self):
        children=[];real=subprocess.Popen
        def launch(*args,**kwargs):
            child=real(*args,**kwargs);children.append(child);return child
        with patch.object(m.subprocess,'Popen',side_effect=launch):
            with self.assertRaisesRegex(m.proof.InfrastructureFailure,'build-unknown'):
                m._build_capture([sys.executable,'-I','-S','-c','import time;time.sleep(20)'],
                    m.common.scrubbed_env(),.05,m._Deadline())
        self.assertEqual(len(children),1);self.assertIsNotNone(children[0].poll())


class AdmissionRoundtripTests(unittest.TestCase):
    """Real filesystem/validators; synthetic immutable Git and Docker edges."""
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve();self.config=self.root/'runtime';self.config.mkdir()
        self.receipts=self.root/'receipts';self.receipts.mkdir();self.plugin=self.root/'buildx';self.plugin.write_bytes(b'plugin')
        self.candidate='c'*40;self.old='e'*40;self.iid='sha256:'+'9'*64;self.builds=0;self.drift=None
        self.calls=[];self.clock=Clock();self.deadline=m._Deadline(clock=self.clock)
        self.previous=SimpleNamespace(sha=self.old,images={'web':'sha256:'+'8'*64})
        self.blobs={m._WEB_RECIPE:b'recipe','.dockerignore':b'.git\n','main.go':b'package main\n'}
        self.pin_recipe=m.sha(self.blobs[m._WEB_RECIPE]);self.pin_ignore=m.sha(self.blobs['.dockerignore'])
        self.receipt_pins={}
        for role in m._SUPPORT:
            raw=json.dumps({'role':role}).encode();name=role+'-build.json';(self.receipts/name).write_bytes(raw)
            self.receipt_pins[role]=(name,m.sha(raw))
        outer=self
        class Source:
            def __init__(inner,*args):inner.tree='a'*40;inner.candidate=outer.candidate
            def inventory(inner,*args):return {}
            def blobs(inner,*args):return outer.blobs
            def clean(inner):
                if outer.drift=='head':raise m.proof.EvidenceError('admission-head-changed')
            def git(inner,args,*unused):
                return b'recipe' if args[0]=='cat-file' else b'a'*40+b'\n'
        self.rows={}
        previous_ref=m.common.IMAGE_REPOSITORY['web']+'@'+self.previous.images['web']
        self.rows[previous_ref]=self.web(self.old,'sha256:'+'7'*64)
        self.rows[previous_ref]['RepoDigests']=[previous_ref]
        self.rows[self.iid]=self.web(self.candidate,self.iid)
        for role,(source,image,recipe) in m._SUPPORT.items():
            cfg={'Labels':{m.stack.SOURCE_LABEL:source,m.stack.RECIPE_LABEL:recipe,m.stack.ROLE_LABEL:role},
                 'User':'65532:65532','Env':[]}
            if role=='fixture':cfg.update(Entrypoint=['/compat/database.test'],WorkingDir='/src/internal/database')
            elif role=='controller':cfg.update(Entrypoint=['python3','-I','-B','/compat/controller.py'],WorkingDir='/compat')
            else:cfg.update(Entrypoint=['/graph-stub'])
            self.rows[image]={'Id':image,'Os':'linux','Architecture':'amd64','Config':cfg}
        for target,name,value in [(m,'_Source',Source),(m,'_supporting',lambda *args:None),
                (m,'_builder_plugin',lambda binary:self.plugin),(m,'_PLUGIN_SHA',m.sha(b'plugin')),
                (m,'_WEB_RECIPE_SHA',self.pin_recipe),(m,'_IGNORE_SHA',self.pin_ignore),(m,'_RECEIPTS',self.receipt_pins),
                (m,'_build_capture',self.build),(self.deadline,'capture',self.capture)]:
            active=patch.object(target,name,value);active.start();self.addCleanup(active.stop)

    def web(self,source,iid):
        return {'Id':iid,'Os':'linux','Architecture':'amd64','Config':{
            'Labels':{m.stack.SOURCE_LABEL:source,m.stack.RECIPE_LABEL:self.pin_recipe,
                      'org.opencontainers.image.source':m._SOURCE_URL,'org.opencontainers.image.title':'rereply-release-web'},
            'Entrypoint':['./rereply'],'User':'rereply','WorkingDir':'/app','Env':[]}}

    def capture(self,args,**kwargs):
        self.calls.append(args)
        if args[5]=='info':
            value={'ID':'synthetic-engine','OSType':'linux','Architecture':'amd64','ServerVersion':'29.7.2'}
            if self.builds and self.drift=='engine':value['ID']='changed-engine'
        elif args[5:7]==['image','inspect']:
            value=[self.rows[args[7]]]
        else:raise AssertionError('unexpected command')
        return m.runtime.Capture(0,json.dumps(value).encode(),b'')

    def build(self,args,env,seconds,deadline):
        self.builds+=1;self.assertIs(deadline,self.deadline);self.assertLessEqual(seconds,1500)
        self.assertNotIn('DOCKER_AUTH_CONFIG',env)
        self.assertEqual(args[0],str(self.plugin))
        self.assertEqual(env['DOCKER_CONTEXT'],'default')
        self.assertEqual(env['BUILDX_GIT_INFO'],'false');self.assertEqual(env['BUILDX_GIT_LABELS'],'false')
        work=Path(args[args.index('--iidfile')+1]).parent
        (work/'image.id').write_text(self.iid+'\n')
        (work/'build-metadata.json').write_text(json.dumps({'containerimage.config.digest':self.iid}))
        if self.drift=='unknown':raise m.proof.InfrastructureFailure('admission-build-unknown')
        if self.drift=='context':(work/'context/main.go').write_bytes(b'changed')
        if self.drift=='receipt':(self.receipts/'fixture-build.json').write_bytes(b'{}')
        if self.drift=='plugin':self.plugin.write_bytes(b'changed')
        if self.drift=='iid':(work/'image.id').write_text('sha256:'+'1'*64)
        if self.drift=='reserve':self.clock.value+=1501
        if self.drift=='label':self.rows[self.iid]['Config']['Labels'][m.stack.SOURCE_LABEL]='0'*40
        if self.drift=='false':return m.runtime.Capture(False,b'',b'')
        return m.runtime.Capture(3 if self.drift=='nonzero' else 0,b'build output',b'')

    def admit(self):
        return m._admit(self.root,self.previous,self.candidate,'docker',self.config,self.receipts,self.deadline)

    def test_complete_local_candidate_binding_retains_actual_support_sources(self):
        result=self.admit()
        self.assertIs(result.seal,m._SEAL);self.assertEqual(result.candidate.source_sha,self.candidate)
        self.assertEqual(result.candidate.reference,self.iid);self.assertEqual(result.previous.source_sha,self.old)
        self.assertEqual(result.graph.source_sha,m._SUPPORT['graph'][0])
        self.assertFalse(result.receipt['release_attestation']);self.assertFalse(result.receipt['registry_publication'])
        self.assertEqual(list(self.config.iterdir()),[])
        with self.assertRaisesRegex(m.proof.EvidenceError,'already-attempted'):self.admit()
        self.assertEqual(self.builds,1)

    def test_nonzero_unknown_stale_iid_never_qualifies_and_is_not_replayed(self):
        self.drift='unknown'
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'unknown'):self.admit()
        with self.assertRaisesRegex(m.proof.EvidenceError,'already-attempted'):self.admit()
        self.assertEqual(self.builds,1)
        value=json.loads((self.receipts.parent/('candidate-'+self.candidate)/'result.json').read_bytes())
        self.assertEqual(value['status'],'REFUSED_NO_RETRY_OR_IMAGE_ADOPTION')
        self.assertEqual(value['build_outcome'],'unknown')
        self.assertEqual(value['failure_code'],'admission-build-unknown')
        self.assertNotIn('image_id',value)

    def test_nonzero_cannot_use_valid_looking_iid(self):
        self.drift='nonzero'
        with self.assertRaisesRegex(m.proof.EvidenceError,'build-failed'):self.admit()
        self.assertEqual(self.builds,1)

    def test_post_build_context_change_refuses(self):
        self.drift='context'
        with self.assertRaisesRegex(m.proof.EvidenceError,'context-changed'):self.admit()

    def test_false_exit_code_cannot_be_numeric_success(self):
        self.drift='false'
        with self.assertRaisesRegex(m.proof.EvidenceError,'build-capture'):self.admit()

    def test_post_build_head_change_refuses(self):
        self.drift='head'
        with self.assertRaisesRegex(m.proof.EvidenceError,'head-changed'):self.admit()

    def test_recipe_specific_ignore_override_refuses_before_build(self):
        self.blobs[m._WEB_RECIPE+'.dockerignore']=b'override'
        with self.assertRaisesRegex(m.proof.EvidenceError,'ignore-override'):self.admit()
        self.assertEqual(self.builds,0)

    def test_post_build_receipt_change_refuses(self):
        self.drift='receipt'
        with self.assertRaisesRegex(m.proof.EvidenceError,'receipt-changed'):self.admit()

    def test_post_build_plugin_change_refuses(self):
        self.drift='plugin'
        with self.assertRaisesRegex(m.proof.EvidenceError,'builder-changed'):self.admit()

    def test_post_build_iid_metadata_mismatch_refuses(self):
        self.drift='iid'
        with self.assertRaisesRegex(m.proof.EvidenceError,'iid-unbound'):self.admit()

    def test_post_build_engine_change_refuses(self):
        self.drift='engine'
        with self.assertRaisesRegex(m.proof.EvidenceError,'engine-changed'):self.admit()

    def test_post_build_source_label_change_refuses(self):
        self.drift='label'
        with self.assertRaisesRegex(m.proof.EvidenceError,'source-invalid'):self.admit()

    def test_after_inspection_minimum_experiment_reserve_is_required(self):
        self.drift='reserve'
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'experiment-reserve'):self.admit()


class CaptureFailureTests(unittest.TestCase):
    def test_real_stream_limit_refuses_and_reaps(self):
        with self.assertRaises(m.proof.InfrastructureFailure):
            m._build_capture([sys.executable,'-I','-S','-c','import sys;sys.stdout.write("x"*(17*1024*1024))'],
                m.common.scrubbed_env(),5,m._Deadline())

    def test_start_failure_is_fixed_refusal(self):
        with patch.object(m.subprocess,'Popen',side_effect=OSError('private synthetic detail')):
            with self.assertRaisesRegex(m.proof.InfrastructureFailure,'^admission-build-start$'):
                m._build_capture([sys.executable,'-I','-S','-c','pass'],m.common.scrubbed_env(),5,m._Deadline())


@unittest.skipUnless(os.name=='nt','Windows Job Object/pipe APIs')
class WindowsContainmentTests(unittest.TestCase):
    def setUp(self):
        import ctypes
        from ctypes import wintypes as w
        self.c=ctypes;self.w=w
        self.k=ctypes.WinDLL('kernel32',use_last_error=True)
        self.k.OpenProcess.argtypes=[w.DWORD,w.BOOL,w.DWORD];self.k.OpenProcess.restype=w.HANDLE
        self.k.WaitForSingleObject.argtypes=[w.HANDLE,w.DWORD];self.k.WaitForSingleObject.restype=w.DWORD
        self.k.CloseHandle.argtypes=[w.HANDLE];self.k.CloseHandle.restype=w.BOOL
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve();self.pids=self.root/'owned-pids.json'
        self.handles=[];self.jobs=[];self.children=[];self.closed_counts=[]
        outer=self;realjob=m._WindowsJob;realpopen=subprocess.Popen
        class TrackedJob(realjob):
            def __init__(inner):
                super().__init__();outer.jobs.append(inner)
            def counts(inner):
                if not outer.handles and outer.pids.exists():
                    try: values=json.loads(outer.pids.read_bytes())
                    except (OSError,ValueError): values=[]
                    for pid in values:
                        handle=outer.k.OpenProcess(0x100000|0x1000,False,pid)
                        # On the normal-parent-exit case it may already have
                        # exited. The descendant remains alive until job teardown.
                        if handle: outer.handles.append(handle)
                return super().counts()
            def close(inner):
                if inner.handle: outer.closed_counts.append(inner.counts())
                return super().close()
        def launch(*args,**kwargs):
            self.assertEqual(kwargs['creationflags'],subprocess.CREATE_NO_WINDOW)
            self.assertFalse(kwargs['shell']);self.assertTrue(kwargs['close_fds'])
            child=realpopen(*args,**kwargs);self.children.append(child);return child
        for name,value in (('_WindowsJob',TrackedJob),):
            active=patch.object(m,name,value);active.start();self.addCleanup(active.stop)
        active=patch.object(m.subprocess,'Popen',side_effect=launch);active.start();self.addCleanup(active.stop)
        self.addCleanup(self.close_handles)

    def close_handles(self):
        for handle in self.handles:self.k.CloseHandle(handle)

    def command(self,mode):
        code=('import json,os,subprocess,sys,time;from pathlib import Path;'
            'c=subprocess.Popen([sys.executable,"-I","-S","-c","import time;time.sleep(30)"],'
            'creationflags=subprocess.CREATE_NO_WINDOW);'
            f'Path({str(self.pids)!r}).write_text(json.dumps([os.getpid(),c.pid]));')
        if mode=='timeout':code+='time.sleep(30)'
        elif mode=='overflow':code+='sys.stdout.write("x"*(17*1024*1024));sys.stdout.flush();time.sleep(30)'
        elif mode=='linger':code+='sys.exit(0)'
        return [sys.executable,'-I','-S','-B','-c',code]

    def assert_closed(self,*,spawned=True):
        self.assertEqual(len(self.children),1)
        self.assertIsNotNone(self.children[0].poll())
        self.assertTrue(self.children[0].stdout.closed);self.assertTrue(self.children[0].stderr.closed)
        self.assertTrue(all(job.handle is None for job in self.jobs))
        self.assertTrue(all(active==0 for total,active in self.closed_counts))
        if spawned:
            self.assertTrue(self.handles)
            self.assertTrue(any(total>=3 for total,active in self.closed_counts))
            for handle in self.handles:self.assertEqual(self.k.WaitForSingleObject(handle,0),0)

    def test_actual_descendant_timeout_kills_entire_owned_job_without_readers(self):
        threads={t.ident for t in threading.enumerate()};started=time.monotonic()
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'build-unknown'):
            m._build_capture(self.command('timeout'),m.common.scrubbed_env(),1,m._Deadline())
        self.assertLess(time.monotonic()-started,6)
        self.assert_closed();self.assertEqual({t.ident for t in threading.enumerate()},threads)

    def test_parent_success_with_lingering_descendant_is_refused_and_terminated(self):
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'build-descendants'):
            m._build_capture(self.command('linger'),m.common.scrubbed_env(),5,m._Deadline())
        self.assert_closed()

    def test_output_overflow_also_terminates_descendant_and_closes_pipes(self):
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'build-output-bound'):
            m._build_capture(self.command('overflow'),m.common.scrubbed_env(),5,m._Deadline())
        self.assert_closed()

    def test_self_assignment_failure_never_spawns_target(self):
        old=m._WINDOWS_LAUNCHER
        broken=old.replace('k.AssignProcessToJobObject(h,k.GetCurrentProcess())','k.AssignProcessToJobObject(None,k.GetCurrentProcess())')
        self.assertNotEqual(old,broken)
        with patch.object(m,'_WINDOWS_LAUNCHER',broken),self.assertRaisesRegex(m.proof.EvidenceError,'build-containment'):
            m._build_capture(self.command('timeout'),m.common.scrubbed_env(),5,m._Deadline())
        self.assertFalse(self.pids.exists());self.assert_closed(spawned=False)

    def test_parent_job_creation_failure_launches_nothing(self):
        with patch.object(m,'_WindowsJob',side_effect=m.proof.InfrastructureFailure('admission-build-containment')):
            with self.assertRaisesRegex(m.proof.InfrastructureFailure,'build-containment'):
                m._build_capture(self.command('timeout'),m.common.scrubbed_env(),5,m._Deadline())
        self.assertEqual(self.children,[]);self.assertFalse(self.pids.exists())

    def test_launcher_start_failure_closes_empty_job(self):
        with patch.object(m.subprocess,'Popen',side_effect=OSError('private')):
            with self.assertRaisesRegex(m.proof.InfrastructureFailure,'build-start'):
                m._build_capture(self.command('timeout'),m.common.scrubbed_env(),5,m._Deadline())
        self.assertFalse(self.pids.exists());self.assertEqual(self.closed_counts,[(0,0)])
        self.assertTrue(all(job.handle is None for job in self.jobs))

    def test_peek_treats_only_broken_pipe_as_eof(self):
        with tempfile.TemporaryFile() as stream:
            job=object.__new__(m._WindowsJob);job.w=self.w
            job.k=SimpleNamespace(PeekNamedPipe=lambda *args:False)
            job.c=SimpleNamespace(byref=self.c.byref,get_last_error=lambda:109)
            self.assertEqual(job.read(stream,100),b'')
            job.c.get_last_error=lambda:5
            with self.assertRaisesRegex(m.proof.InfrastructureFailure,'build-pipe'):job.read(stream,100)

    def test_fixed_environment_has_no_helper_search_or_inherited_overrides(self):
        config=self.root/'config';config.mkdir()
        with patch.dict(os.environ,{'PATH':'hostile','DOCKER_HOST':'hostile','DOCKER_AUTH_CONFIG':'secret',
                                   'BUILDX_GIT_INFO':'true','BUILDX_GIT_LABELS':'true'}):
            env=m._build_environment(config,self.root)
        self.assertEqual(env['DOCKER_CONFIG'],str(config));self.assertEqual(env['DOCKER_CONTEXT'],'default')
        self.assertEqual(env['BUILDX_GIT_INFO'],'false');self.assertEqual(env['BUILDX_GIT_LABELS'],'false')
        self.assertNotIn('DOCKER_HOST',env);self.assertNotIn('DOCKER_AUTH_CONFIG',env)
        self.assertEqual(Path(env['PATH']).name.lower(),'system32')
        original=Path.iterdir;system=Path(env['PATH'])
        def entries(path):
            return iter([path/'docker-credential-wincred.exe']) if path==system else original(path)
        with patch.object(Path,'iterdir',entries),self.assertRaisesRegex(m.proof.EvidenceError,'credential-helper-available'):
            m._build_environment(config,self.root)
        (self.root/'DOCKER-CREDENTIAL-wincred.EXE').write_bytes(b'not executed')
        with self.assertRaisesRegex(m.proof.EvidenceError,'credential-helper-available'):
            m._build_environment(config,self.root)


if __name__=='__main__':unittest.main()
