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

    def test_role_contexts_bind_all_current_source_and_exact_closed_inputs(self):
        blobs,pins=source_blobs()
        with patch.multiple(m,**pins):
            values=m._role_inputs(blobs)
            self.assertEqual(len(values['graph']),16)
            self.assertEqual(len(values['controller']),4)
            self.assertEqual(values['fixture'],blobs);self.assertEqual(values['candidate'],blobs)
            changed={**blobs,'release/staging/bootstrap/new.go':b'new source'}
            self.assertIn('release/staging/bootstrap/new.go',m._role_inputs(changed)['fixture'])
            for role in ('fixture','controller','candidate'):
                with self.subTest(role=role),self.assertRaisesRegex(m.proof.EvidenceError,'ignore-override'):
                    m._role_inputs({**blobs,m._RECIPES[role][0]+'.dockerignore':b'*'})
            for path in ['.dockerignore',m._RECIPES['graph'][0]+'.dockerignore',m._RECIPES['fixture'][0]]:
                with self.subTest(path=path),self.assertRaises(m.proof.EvidenceError):
                    m._role_inputs({**blobs,path:b'changed'})
            with self.assertRaisesRegex(m.proof.EvidenceError,'graph-inputs'):
                m._role_inputs({**blobs,'release/staging/graphstub/new.go':b'new'})


class BuildTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)

    def test_fixed_command_no_push_auth_arbitrary_args(self):
        args=m._build_command(self.root/'docker-buildx.exe',self.root/'context',self.root,'a'*40,'candidate','default')
        self.assertEqual(args[:4],[str(self.root/'docker-buildx.exe'),'build','--builder','default'])
        self.assertNotIn('--config',args);self.assertNotIn('--context',args)
        self.assertIn('--load',args);self.assertIn('--provenance=false',args);self.assertIn('--pull=false',args)
        self.assertEqual([args[i+1] for i,x in enumerate(args) if x=='--build-arg'],['TARGETARCH=amd64','TARGETOS=linux'])
        self.assertFalse(any(x in args for x in ('--push','--secret','--ssh','--cache-from','--cache-to')))
        labels=[args[i+1] for i,x in enumerate(args) if x=='--label']
        self.assertIn(m.stack.SOURCE_LABEL+'='+'a'*40,labels)
        self.assertIn(m.stack.RECIPE_LABEL+'='+m._WEB_RECIPE_SHA,labels)

    def test_all_roles_have_closed_source_recipe_and_fixed_platform_arguments(self):
        for role,(recipe,digest) in m._RECIPES.items():
            args=m._build_command(self.root/'buildx',self.root/'context',self.root,'a'*40,role,'compat-'+'b'*32)
            self.assertEqual(args[args.index('--file')+1],str(self.root/'context'/recipe))
            self.assertEqual(args[args.index('--platform')+1],'linux/amd64')
            labels=[args[i+1] for i,value in enumerate(args) if value=='--label']
            self.assertIn(m.stack.ROLE_LABEL+'='+role,labels)
            self.assertIn(m.stack.RECIPE_LABEL+'='+digest,labels)
            arguments=[args[i+1] for i,value in enumerate(args) if value=='--build-arg']
            if role in ('fixture','controller'):
                self.assertEqual(arguments,['RECIPE_SHA256='+digest,'SOURCE_SHA='+'a'*40])
        for role,builder in [('foreign','default'),('graph','remote'),('candidate','cloud-example')]:
            with self.assertRaises(m.proof.EvidenceError):
                m._build_command(self.root/'buildx',self.root,self.root,'a'*40,role,builder)

    def test_linux_plugin_location_does_not_authorize_wrong_bytes_or_writable_binary(self):
        plugin=self.root/'buildx';plugin.write_bytes(b'fixed synthetic plugin')
        with patch.object(m,'_host_linux',return_value=True):
            with self.assertRaisesRegex(m.proof.EvidenceError,'builder-unproven'):m._builder_plugin('docker',plugin)
            with patch.object(m,'_LINUX_PLUGIN_SHA',m.sha(plugin.read_bytes())),patch.object(m.os,'access',return_value=True):
                original=Path.stat
                def metadata(path,*args,**kwargs):
                    row=original(path,*args,**kwargs)
                    if path==plugin:
                        return os.stat_result((0o100755,*tuple(row)[1:]))
                    return row
                with patch.object(Path,'stat',metadata):self.assertEqual(m._builder_plugin('docker',plugin),plugin)
                def writable(path,*args,**kwargs):
                    row=original(path,*args,**kwargs)
                    return os.stat_result((0o100777,*tuple(row)[1:])) if path==plugin else row
                with patch.object(Path,'stat',writable),self.assertRaisesRegex(m.proof.EvidenceError,'builder-mode'):
                    m._builder_plugin('docker',plugin)

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


def source_blobs():
    blobs={'main.go':b'package main','go.mod':b'module synthetic','go.sum':b'sum','.dockerignore':b'ignore'}
    recipes={role:(path, m.sha(('recipe '+role).encode())) for role,(path,_) in m._RECIPES.items()}
    blobs.update({path:('recipe '+role).encode() for role,(path,_) in recipes.items()})
    graph_ignore=recipes['graph'][0]+'.dockerignore';blobs[graph_ignore]=b'graph-ignore'
    blobs.update({'release/staging/graphstub/file'+str(i)+'.go':b'package graph' for i in range(12)})
    blobs.update({'release/compat/'+name+'.py':name.encode() for name in ('controller','sweep','proof')})
    return blobs,{'_RECIPES':recipes,'_IGNORE_SHA':m.sha(b'ignore'),'_GRAPH_IGNORE_SHA':m.sha(b'graph-ignore')}


class AdmissionRoundtripTests(unittest.TestCase):
    """Real exports/validators and mocked Git/Docker/build edges; no daemon."""
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name).resolve()
        self.config=self.root/'config';self.config.mkdir();self.work=self.root/'work';self.work.mkdir()
        self.plugin=self.root/'buildx';self.plugin.write_bytes(b'plugin')
        self.binary=self.root/'docker';self.binary.write_bytes(b'not executed')
        self.candidate='b'*40;self.old='a'*40;self.previous=SimpleNamespace(sha=self.old,images={'web':'sha256:'+'6'*64})
        self.clock=Clock();self.deadline=m._Deadline(clock=self.clock);self.builds=[];self.calls=[];self.drift=None
        self.blobs,self.pins=source_blobs();self.rows={};self.images={role:'sha256:'+str(i+1)*64 for i,role in enumerate(m._RECIPES)}
        self.previous_ref=m.common.IMAGE_REPOSITORY['web']+'@'+self.previous.images['web']
        self.rows[self.previous_ref]=self.web(self.old,'sha256:'+'7'*64,m.sha(b'previous-recipe'))
        self.rows[self.previous_ref]['RepoDigests']=[self.previous_ref]
        for ref in (m.runtime.POSTGRES,m.runtime.REDIS):
            repo='postgres' if ref==m.runtime.POSTGRES else 'redis'
            self.rows[ref]={'Id':'sha256:'+'8'*64,'Os':'linux','Architecture':'amd64',
                            'RepoDigests':[repo+'@'+ref.split('@')[1]]}
        for role,image in self.images.items():
            recipe=self.pins['_RECIPES'][role][1]
            if role=='candidate':row=self.web(self.candidate,image,recipe);row['Config']['Labels'][m.stack.ROLE_LABEL]=role
            else:
                cfg={'Labels':{m.stack.SOURCE_LABEL:self.candidate,m.stack.RECIPE_LABEL:recipe,m.stack.ROLE_LABEL:role},
                     'User':'65532:65532','Env':[]}
                if role=='fixture':cfg.update(Entrypoint=['/compat/database.test'],WorkingDir='/src/internal/database')
                elif role=='controller':cfg.update(Entrypoint=['python3','-I','-B','/compat/controller.py'],WorkingDir='/compat')
                else:cfg.update(Entrypoint=['/graph-stub'])
                row={'Id':image,'Os':'linux','Architecture':'amd64','Config':cfg}
            self.rows[image]=row
        outer=self
        class Source:
            def __init__(inner,*args):inner.tree='c'*40;inner.candidate=outer.candidate
            def inventory(inner,*args):return {}
            def blobs(inner,*args):return outer.blobs
            def clean(inner):
                if outer.builds and outer.drift=='head':raise m.proof.EvidenceError('admission-head-changed')
            def git(inner,args,*unused):return b'previous-recipe' if args[0]=='cat-file' else b'c'*40+b'\n'
        patches=[(m,'_Source',Source),(m,'_builder_plugin',lambda *a:self.plugin),(m,'_PLUGIN_SHA',m.sha(b'plugin')),
                 (m,'_host_linux',lambda:False),(m,'_build_capture',self.capture)]
        patches += [(m,key,value) for key,value in self.pins.items()]
        for target,name,value in patches:
            active=patch.object(target,name,value);active.start();self.addCleanup(active.stop)

    def web(self,source,image,recipe):
        return {'Id':image,'Os':'linux','Architecture':'amd64','Config':{
            'Labels':{m.stack.SOURCE_LABEL:source,m.stack.RECIPE_LABEL:recipe,
                      'org.opencontainers.image.source':m._SOURCE_URL,'org.opencontainers.image.title':'rereply-release-web'},
            'Entrypoint':['./rereply'],'User':'rereply','WorkingDir':'/app','Env':[]}}

    def capture(self,args,env,seconds,deadline,**kwargs):
        self.calls.append(args);self.assertIs(deadline,self.deadline)
        self.assertNotIn('DOCKER_AUTH_CONFIG',env);self.assertEqual(env['DOCKER_CONTEXT'],'default')
        self.assertEqual(env['BUILDX_GIT_INFO'],'false');self.assertEqual(env['BUILDX_GIT_LABELS'],'false')
        if args[0]==str(self.binary):
            op=args[5:7]
            if args[5]=='info':
                value={'ID':'synthetic-engine','OSType':'linux','Architecture':'amd64','ServerVersion':'29.7.2'}
                if self.builds and self.drift=='engine':value['ID']='changed'
            elif op==['image','pull']:
                self.clock.value+=2;return m.runtime.Capture(0,b'',b'')
            elif op==['image','inspect']:value=[self.rows[args[7]]]
            else:raise AssertionError('unexpected Docker operation')
            return m.runtime.Capture(0,json.dumps(value).encode(),b'')
        self.assertEqual(args[0],str(self.plugin));self.assertEqual(args[1],'build')
        work=Path(args[args.index('--iidfile')+1]).parent;role=work.name
        self.builds.append((role,seconds));self.clock.value+=100
        iid=self.images[role];(work/'image.id').write_text(iid+'\n')
        (work/'build-metadata.json').write_text(json.dumps({'containerimage.config.digest':iid}))
        if self.drift=='unknown':raise m.proof.InfrastructureFailure('admission-build-unknown')
        if self.drift=='context':(work/'context/go.mod').write_bytes(b'changed')
        if self.drift=='plugin':self.plugin.write_bytes(b'changed')
        if self.drift=='iid':(work/'image.id').write_text('sha256:'+'9'*64)
        if self.drift=='reserve':self.clock.value+=1501
        if self.drift=='label':self.rows[iid]['Config']['Labels'][m.stack.SOURCE_LABEL]='0'*40
        if self.drift=='false':return m.runtime.Capture(False,b'',b'')
        if self.drift=='controller-failure' and role=='controller':return m.runtime.Capture(3,b'',b'')
        return m.runtime.Capture(3 if self.drift=='nonzero' else 0,b'build output',b'')

    def admit(self):
        return m._admit(self.root,self.previous,self.candidate,str(self.binary),self.config,self.work,self.deadline)

    def test_complete_four_builds_same_source_and_monotonic_shared_cap(self):
        result=self.admit()
        self.assertIs(result.seal,m._SEAL)
        self.assertEqual([x[0] for x in self.builds],['graph','controller','fixture','candidate'])
        self.assertEqual([x[1] for x in self.builds],[1494,1394,1294,1194])
        self.assertEqual(result.previous.source_sha,self.old)
        for binding in (result.graph,result.controller,result.fixture,result.candidate):
            self.assertEqual(binding.source_sha,self.candidate)
        self.assertFalse(result.receipt['release_attestation']);self.assertTrue(result.receipt['cleanup_complete'])
        pulls=[args[-1] for args in self.calls if args[5:7]==['image','pull']]
        self.assertEqual(pulls,[self.previous_ref,m.runtime.POSTGRES,m.runtime.REDIS])
        with self.assertRaises(m.proof.EvidenceError):self.admit()
        self.assertEqual(len(self.builds),4)

    def test_partial_failure_never_builds_later_roles_or_retries(self):
        self.drift='controller-failure'
        with self.assertRaisesRegex(m.proof.EvidenceError,'build-failed'):self.admit()
        with self.assertRaises(m.proof.EvidenceError):self.admit()
        self.assertEqual([x[0] for x in self.builds],['graph','controller'])
        result=json.loads((self.work/('candidate-'+self.candidate)/'result.json').read_bytes())
        self.assertEqual(result['status'],'REFUSED_NO_RETRY_OR_IMAGE_ADOPTION')
        self.assertNotIn('candidate',result['roles'])

    def test_unknown_never_adopts_existing_valid_iid_or_retries(self):
        self.drift='unknown'
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'unknown'):self.admit()
        with self.assertRaises(m.proof.EvidenceError):self.admit()
        self.assertEqual(len(self.builds),1)
        result=json.loads((self.work/('candidate-'+self.candidate)/'result.json').read_bytes())
        self.assertEqual(result['failure_code'],'admission-build-unknown')
        self.assertNotIn('image_id',result['roles']['graph'])

    def test_tamper_nonzero_and_late_completion_refuse(self):
        # Each subcase is a separate fresh attempt with its own source context.
        for drift in ('nonzero','context','plugin','iid','label','false','head','engine','reserve'):
            child=AdmissionRoundtripTests('test_complete_four_builds_same_source_and_monotonic_shared_cap');child.setUp()
            try:
                child.drift=drift
                with self.subTest(drift=drift),self.assertRaises((m.proof.EvidenceError,m.proof.InfrastructureFailure)):
                    child.admit()
            finally:child.doCleanups()

    def test_raw_previous_digest_required_and_historical_directory_is_not_evidence(self):
        for bad in ('latest',self.previous_ref,False):
            self.previous.images['web']=bad
            with self.assertRaisesRegex(m.proof.EvidenceError,'previous-invalid'):self.admit()
        self.assertEqual(self.calls,[])
        self.previous.images['web']='sha256:'+'6'*64
        (self.work/'graph-build.json').write_text('{}')
        with self.assertRaises(m.proof.EvidenceError):self.admit()
        self.assertEqual(self.builds,[])

    def test_linux_factory_rechecks_backend_and_cleans_before_admission(self):
        outer=self;objects=[]
        class Backend:
            def __init__(inner,*args):
                inner.closed=False;inner.name='compat-'+'9'*32;inner.checks=0;objects.append(inner)
            def start(inner):outer.clock.value+=50
            def check(inner):inner.checks+=1;outer.clock.value+=1
            def close(inner):inner.closed=True;outer.clock.value+=5
            def summary(inner):return {'closed':inner.closed}
        env={'DOCKER_CONTEXT':'default','BUILDX_GIT_INFO':'false','BUILDX_GIT_LABELS':'false'}
        with patch.object(m,'_host_linux',return_value=True),patch.object(m,'_LinuxBuilder',Backend), \
             patch.object(m,'_LINUX_PLUGIN_SHA',m.sha(b'plugin')),patch.object(m,'_build_environment',return_value=env):
            result=self.admit()
        self.assertTrue(objects[0].closed);self.assertGreaterEqual(objects[0].checks,13)
        self.assertTrue(result.receipt['cleanup_complete']);self.assertLess(self.builds[0][1],1444)
        self.assertTrue(all(args[3]==objects[0].name for args in self.calls if args[0]==str(self.plugin)))

    def test_linux_cleanup_refusal_suppresses_all_completed_builds(self):
        class Backend:
            def __init__(inner,*args):inner.closed=False;inner.name='compat-'+'9'*32
            def start(inner):pass
            def check(inner):pass
            def close(inner):raise m.proof.InfrastructureFailure('admission-builder-ownership-unresolved')
            def summary(inner):return {'closed':False,'acknowledged_container_id':'d'*64}
        env={'DOCKER_CONTEXT':'default','BUILDX_GIT_INFO':'false','BUILDX_GIT_LABELS':'false'}
        with patch.object(m,'_host_linux',return_value=True),patch.object(m,'_LinuxBuilder',Backend), \
             patch.object(m,'_LINUX_PLUGIN_SHA',m.sha(b'plugin')),patch.object(m,'_build_environment',return_value=env):
            with self.assertRaisesRegex(m.proof.InfrastructureFailure,'ownership-unresolved'):self.admit()
        self.assertEqual(len(self.builds),4)
        result=json.loads((self.work/('candidate-'+self.candidate)/'result.json').read_bytes())
        self.assertFalse(result['cleanup_complete']);self.assertEqual(result['status'],'REFUSED_NO_RETRY_OR_IMAGE_ADOPTION')


class LinuxWorld:
    """Mock daemon plus actual private node metadata. Never executes Docker."""
    def __init__(self,root,clock):
        self.root,self.clock=root,clock;self.calls=[];self.volume=None;self.container=None;self.mode=None
        self.image={'Id':'sha256:'+'f'*64,'Os':'linux','Architecture':'amd64',
                    'RepoDigests':['moby/buildkit@'+m._BUILDKIT.split('@')[1]],
                    'Config':{'Entrypoint':['buildkitd'],'Env':['PATH=/usr/bin'],'Labels':{'pinned':'image'}}}
        self.builder=None

    def result(self,value=None,code=0,raw=None):
        return m.runtime.Capture(code,raw if raw is not None else json.dumps(value).encode(),b'')

    def capture(self,argv,env,seconds,deadline,**kwargs):
        self.calls.append(list(argv));self.clock.value+=.1;b=self.builder
        if argv[0]==str(b.plugin):
            if argv[1]=='create':
                row={'Name':b.name,'Driver':'docker-container','Dynamic':False,'Nodes':[{
                    'Name':b.node,'Endpoint':'unix:///var/run/docker.sock','Platforms':None,
                    'DriverOpts':{'image':m._BUILDKIT,'restart-policy':'no','provenance-add-gha':'false'},
                    'Flags':list(m._BUILDER_METADATA_FLAGS),'Files':None}]}
                target=b.build_config/'buildx/instances'/b.name;target.parent.mkdir(parents=True);target.write_text(json.dumps(row))
                if self.mode=='delete-before-bootstrap':self.container=None
                return self.result(raw=b.name.encode()+b'\n')
            assert argv[1:3]==['inspect','--bootstrap']
            if self.mode=='bootstrap-fail':return self.result(code=1,raw=b'')
            self.container['State']={'Running':True,'StartedAt':'2026-10-11T01:00:00Z'}
            return self.result(raw=b'not a proof')
        args=argv[5:];op=args[:2]
        if self.mode=='list-fail' and op==['container','ls']:return self.result(code=1,raw=b'')
        if op==['container','ls']:
            return self.result(raw=(json.dumps(self.container['Id']).encode()+b'\n') if self.container else b'')
        if op==['volume','ls']:return self.result(raw=(json.dumps(self.volume['Name']).encode()+b'\n') if self.volume else b'')
        if op==['image','pull']:return self.result(raw=b'pulled')
        if op==['image','inspect']:return self.result([self.image])
        stamp=m.datetime.datetime.now(m.datetime.timezone.utc).replace(microsecond=0).isoformat().replace('+00:00','Z')
        if op==['volume','create']:
            self.volume={'Name':b.volume,'Driver':'local','Scope':'local','Labels':dict(b.labels),'Options':None,'CreatedAt':stamp}
            return self.result(raw=b'wrong\n' if self.mode=='volume-ambiguous' else b.volume.encode()+b'\n')
        if op==['volume','inspect']:return self.result([self.volume] if self.volume else [],code=0 if self.volume else 1)
        if op==['container','create']:
            self.container={'Id':'d'*64,'Name':'/'+b.container,'Image':self.image['Id'],'Created':stamp,
                'Config':{'Image':m._BUILDKIT,'Labels':{**self.image['Config']['Labels'],**b.labels},
                          'Entrypoint':['buildkitd'],'Cmd':['--debug'],'Env':['PATH=/usr/bin']},
                'HostConfig':{'Privileged':True,'Init':True,'NetworkMode':'bridge',
                              'RestartPolicy':{'Name':'no','MaximumRetryCount':0}},
                'Mounts':[{'Type':'volume','Name':b.volume,'Destination':'/var/lib/buildkit','RW':True}],
                'RestartCount':0,'State':{'Running':False,'StartedAt':'0001-01-01T00:00:00Z'}}
            return self.result(raw=b'wrong\n' if self.mode=='container-ambiguous' else b'd'*64+b'\n')
        if op==['container','inspect']:
            if not self.container or self.container['Id']!=args[2]:return self.result(code=1,raw=b'')
            return self.result([self.container])
        if op==['container','stop']:
            assert args[-1]==self.container['Id'];self.container['State']['Running']=False
            return self.result(raw=args[-1].encode()+b'\n')
        if op==['container','rm']:
            assert args[2]==self.container['Id'];self.container=None
            return self.result(raw=args[2].encode()+b'\n')
        if op==['volume','rm']:
            assert args[2]==self.volume['Name'];self.volume=None
            return self.result(raw=args[2].encode()+b'\n')
        raise AssertionError('unexpected fixed operation '+repr(args))


class LinuxBuilderTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name).resolve()
        self.config=self.root/'config';self.config.mkdir();self.build_config=self.root/'build-config';self.build_config.mkdir()
        self.clock=Clock();self.deadline=m._Deadline(clock=self.clock);self.world=LinuxWorld(self.root,self.clock)
        self.b=m._LinuxBuilder(str(self.root/'docker'),self.config,self.root/'buildx',self.build_config,self.root,'a'*40,self.deadline)
        self.world.builder=self.b
        for name,value in [('_build_capture',self.world.capture),('_build_environment',lambda *args:{'synthetic':'only'})]:
            active=patch.object(m,name,value);active.start();self.addCleanup(active.stop)

    def test_owned_create_bootstrap_metadata_and_exact_cleanup(self):
        self.b.start();self.b.check();self.b.close()
        self.assertTrue(self.b.closed);self.assertIsNone(self.world.container);self.assertIsNone(self.world.volume)
        calls=self.world.calls;ops=[call[5:7] for call in calls if call[0]==self.b.binary]
        self.assertEqual(ops.count(['container','create']),1);self.assertEqual(ops.count(['volume','create']),1)
        self.assertEqual(ops.count(['container','rm']),1);self.assertEqual(ops.count(['volume','rm']),1)
        stop=next(call for call in calls if call[5:7]==['container','stop'])
        self.assertEqual(stop[5:],['container','stop','--timeout','10','d'*64])
        self.assertFalse(any('--force' in call or '--gpus' in call or '--publish' in call for call in calls))
        create=next(call for call in calls if call[5:7]==['container','create'])
        self.assertIn(m._BUILDKIT,create);self.assertEqual(create[-1],'--debug')
        self.assertEqual(len([call for call in calls if call[0]==str(self.b.plugin)]),2)

    def test_absence_command_failure_is_not_absence_authority(self):
        self.world.mode='list-fail'
        with self.assertRaises(m.proof.InfrastructureFailure):self.b.start()
        self.assertFalse(self.b.volume_attempted);self.assertFalse(self.b.container_attempted)

    def test_existing_name_refuses_without_adoption_or_mutation(self):
        self.world.container={'Id':'1'*64}
        with self.assertRaisesRegex(m.proof.EvidenceError,'already-exists'):self.b.start()
        self.assertFalse(self.b.volume_attempted)
        self.assertFalse(any('create' in call or 'rm' in call for call in self.world.calls))

    def test_unknown_volume_creation_never_guesses_cleanup_identity(self):
        self.world.mode='volume-ambiguous'
        with self.assertRaisesRegex(m.proof.EvidenceError,'mutation-unbound'):self.b.start()
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'ownership-unresolved'):self.b.close()
        self.assertIsNotNone(self.world.volume);self.assertFalse(self.b.container_attempted)
        self.assertFalse(any('rm' in call for call in self.world.calls))

    def test_unknown_container_creation_never_guesses_cleanup_identity(self):
        self.world.mode='container-ambiguous'
        with self.assertRaisesRegex(m.proof.EvidenceError,'mutation-unbound'):self.b.start()
        with self.assertRaisesRegex(m.proof.InfrastructureFailure,'ownership-unresolved'):self.b.close()
        self.assertIsNotNone(self.world.container);self.assertFalse(any('rm' in call for call in self.world.calls))

    def test_deleted_before_bootstrap_refuses_before_driver_implicit_create(self):
        self.world.mode='delete-before-bootstrap'
        with self.assertRaises(m.proof.InfrastructureFailure):self.b.start()
        self.assertFalse(any('--bootstrap' in call for call in self.world.calls))

    def test_deleted_or_replaced_before_build_never_recreates(self):
        self.b.start();before=len(self.world.calls);self.world.container['Id']='e'*64
        with self.assertRaises(m.proof.InfrastructureFailure):self.b.check()
        with self.assertRaises(m.proof.InfrastructureFailure):self.b.close()
        self.assertFalse(any('create' in call or 'rm' in call for call in self.world.calls[before:]))

    def test_bound_wrong_source_image_mount_or_restart_refuses(self):
        self.b.start()
        import copy
        original=copy.deepcopy(self.world.container)
        changes=[lambda row:row['Config']['Labels'].update({m.stack.SOURCE_LABEL:'f'*40}),
                 lambda row:row.update(Image='sha256:'+'0'*64),
                 lambda row:row['Mounts'][0].update(Name='foreign'),
                 lambda row:row['State'].update(StartedAt='2026-10-11T02:00:00Z'),
                 lambda row:row['HostConfig'].update(PortBindings={'1/tcp':[{'HostPort':'1'}]})]
        for change in changes:
            self.world.container=copy.deepcopy(original);change(self.world.container)
            with self.assertRaises(m.proof.EvidenceError):self.b.check()
        self.world.container=original;self.b.close()

    def test_metadata_foreign_node_driver_or_flags_refuse(self):
        self.b.start();path=self.build_config/'buildx/instances'/self.b.name;original=json.loads(path.read_bytes())
        import copy
        for mutate in [lambda row:row.update(Driver='remote'),lambda row:row['Nodes'][0].update(Endpoint='tcp://foreign'),
                       lambda row:row['Nodes'][0].update(Flags=['--debug']),lambda row:row['Nodes'][0].update(Files={'arbitrary':'AA=='})]:
            row=copy.deepcopy(original);mutate(row);path.write_text(json.dumps(row))
            with self.assertRaises(m.proof.EvidenceError):self.b.check()
        path.write_text(json.dumps(original));self.b.close()

    def test_nonzero_bootstrap_can_only_cleanup_already_bound_resources(self):
        self.world.mode='bootstrap-fail'
        with self.assertRaisesRegex(m.proof.EvidenceError,'bootstrap'):self.b.start()
        self.b.close();self.assertTrue(self.b.closed)
        self.assertEqual(len([call for call in self.world.calls if '--bootstrap' in call]),1)

    def test_cleanup_consumes_same_overall_deadline_and_preserves_unknown(self):
        self.b.start();self.clock.value=self.deadline.end-0.05
        with self.assertRaises((m.proof.EvidenceError,m.proof.InfrastructureFailure)):self.b.close()
        self.assertFalse(self.b.closed);self.assertIsNotNone(self.world.container)

    def test_volume_drift_prevents_any_destructive_cleanup(self):
        self.b.start();self.world.volume['Labels']['foreign']='true';before=len(self.world.calls)
        with self.assertRaises(m.proof.EvidenceError):self.b.close()
        self.assertFalse(any('stop' in call or 'rm' in call for call in self.world.calls[before:]))


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
