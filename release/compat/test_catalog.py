"""Source/CLI collection contract tests: local Git and fake Docker/PG only."""
from dataclasses import replace
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0,str(Path(__file__).resolve().parent))
import catalog as c
import test_processes
import test_postgres
import test_stack

REPO=Path(__file__).resolve().parents[2]
HEAD="e"*40
LATER="a"*40
BLOBS={name:(REPO/name).read_bytes() for name in c._PINS}
ENTRY=c.runtime.release_record.Entry("prod-20261011T000000Z-e0016b35","promote",c._BASE,
    {"web":test_processes.PREV.split('@',1)[1]},"1"*64,"2026-10-11T00:00:00Z",None,None,False,{})


class Git:
    def __init__(self):
        self.calls=[];self.changed=None;self.present=False;self.fail=False
        self.commits={c._BASE,LATER,HEAD};self.missing=set();self.ancestor=True
    def __call__(self,argv,**options):
        self.calls.append((argv,options));args=argv[3:]
        assert argv[:2]==["git","-C"] and options['timeout']==30
        if self.fail:return c.runtime.Capture(1,b'',b'private path')
        if args[:2]==['cat-file','-t']:
            if args[2] not in self.commits:return c.runtime.Capture(1,b'',b'private missing commit')
            raw=b'commit\n'
        elif args[:2]==['merge-base','--is-ancestor']:
            if not self.ancestor:return c.runtime.Capture(1,b'',b'')
            raw=b''
        elif args[0]=='ls-tree':raw=b'present' if self.present else b''
        elif args[:2]==['cat-file','blob']:
            sha,path=args[2].split(':',1)
            if (sha,path) in self.missing:return c.runtime.Capture(1,b'',b'private missing artifact')
            raw=BLOBS[path]
            if self.changed and self.changed(sha,path):raw+=b' '
        else:raise AssertionError('unknown source route')
        return c.runtime.Capture(0,raw,b'')


def compare(contract):
    profile=json.loads(BLOBS[c._PROFILE])
    return {'sha256':contract.target_sha256,'match':False,
        'differences':[{'kind':d['kind'],'identity':d['identity'],'status':'changed'} for d in profile['delta']],
        'unknown':[],'count_only':{name:0 for name in c._COUNT_KEYS},
        'ownership':{'migration_owner_owns_database':True,'migration_owner_owns_public_schema':True}}


class World(test_processes.World):
    def __init__(self):
        super().__init__()
        self.images[test_processes.PREV]['Config']['Labels'][c.processes.stack.SOURCE_LABEL]=c._BASE
        self.inventory=[]
        for row in json.loads(BLOBS[c._GOLDEN])['objects']:
            if row['kind']=='relations':self.inventory.append([row['identity'],'r',[copy.deepcopy(test_postgres.COLUMN)]])
        self.state=copy.deepcopy(test_postgres.STATE)
        self.data_calls=[]
    def capture(self,argv,**options):
        args=argv[5:]
        if args[:1]==['inspect'] or (args[:2]==['network','inspect'] and options['stderr_limit']==c.runtime.MAX_STREAM):
            self.calls.append((list(args),options))
            assert options['stdout_limit']<=c.runtime.MAX_STREAM and options['stderr_limit']<=c.runtime.MAX_STREAM
            value=self.containers[args[1]] if args[0]=='inspect' else self.network
            return c.runtime.Capture(0,json.dumps([value]).encode(),b'')
        if args[:2]==['exec','-i'] and '--user' not in args:
            self.calls.append((list(args),options))
            script=options['input_bytes'].decode()
            if "json_build_array('inventory'" in script:
                import re
                token=re.search(r"json_build_array\('complete','([0-9a-f]{32})'\)",script)[1]
                self.data_calls.append(len(self.calls))
                return c.runtime.Capture(0,test_postgres.output(self.state,['inventory',self.inventory],['complete',token]),b'')
            assert "json_build_array('state'" in script
            return c.runtime.Capture(0,test_postgres.output(self.state),b'')
        return super().capture(argv,**options)


class CatalogTests(unittest.TestCase):
    def setUp(self):
        self.git=Git()
        self.contract=c.CatalogContract(REPO,ENTRY,HEAD,base_env={},capture=self.git)
        self.good=compare(self.contract)

    def encoded(self,value):return (json.dumps(value)+'\n').encode()

    def test_real_manifest_entry_digest_composes_exact_repository(self):
        # Use the real record validator/Entry constructor, not a fabricated
        # repository-qualified Entry.images value that production never emits.
        record=c.runtime.release_record
        raw=record.build_manifest({'kind':'promote','release':ENTRY.release,'sha':c._BASE,
            'images':{'web':ENTRY.images['web'],'meta-relay':'sha256:'+'8'*64,'gmail-relay':'sha256:'+'9'*64},
            'previous':c.runtime.common.BOOTSTRAP_TAG,'rolled_back_from':None,'reconciled':False,
            'built_at':ENTRY.deployed_at,'deployed_at':ENTRY.deployed_at,'workflow_sha':c._BASE,
            'environment_values_sha256':'2'*64,'non_source_projection_sha256':'3'*64})
        manifest=record.validate_manifest(c.runtime.common.loads_strict(raw))
        previous=record.manifest_entry(manifest,raw)
        actual=c.CatalogContract(REPO,previous,HEAD,base_env={},capture=Git())
        self.assertEqual(previous.images['web'],ENTRY.images['web'])
        self.assertEqual(actual.previous_web,c.runtime.common.IMAGE_REPOSITORY['web']+'@'+ENTRY.images['web'])
        self.assertEqual(actual.previous_web,test_processes.PREV)

    def test_malformed_or_qualified_entry_digest_refuses_before_git(self):
        for digest in (test_processes.PREV,'sha256:'+'A'*64,'sha256:'+'1'*63,
                       ENTRY.images['web']+'\n','sha512:'+'1'*64,None,True):
            with self.subTest(digest=digest):
                fake=Git()
                with self.assertRaises(c.proof.EvidenceError):
                    c.CatalogContract(REPO,ENTRY._replace(images={'web':digest}),HEAD,base_env={},capture=fake)
                self.assertEqual(fake.calls,[])

    def test_actual_source_bound_127_tables_and_first_baseline_nop(self):
        self.assertEqual(len(self.contract.expected_preexisting_tables),127)
        self.assertEqual(self.contract.expected_preexisting_tables,tuple(sorted(set(self.contract.expected_preexisting_tables))))
        self.assertIn(c.postgres.identity('organizations'),self.contract.expected_preexisting_tables)
        self.assertEqual(self.contract.public()['base_overlay'],'explicit-empty-first-baseline')
        self.assertEqual(self.contract.target_sha256,'1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7')
        self.assertTrue(all('HEAD' not in a for a,_ in self.git.calls))

    def test_real_local_git_immutable_bytes_not_working_tree(self):
        # Git reads only; no branch/ref/worktree mutation and no remote calls.
        head=subprocess.check_output(['git','-C',str(REPO),'rev-parse','HEAD'],text=True).strip()
        env={k:v for k,v in os.environ.items() if k in {'PATH','SYSTEMROOT','SystemRoot','TEMP','TMP','COMSPEC','ComSpec'}}
        actual=c.CatalogContract(REPO,ENTRY,head,base_env=env)
        self.assertEqual(actual.expected_preexisting_tables,self.contract.expected_preexisting_tables)
        later=c.CatalogContract(REPO,ENTRY._replace(sha=head),head,base_env=env)
        self.assertEqual(later.expected_preexisting_tables,actual.expected_preexisting_tables)
        self.assertEqual(later.previous_sha,head)
        self.assertEqual(later.public()['base_overlay'],'equal-reviewed-v0-nop')

    def test_later_equal_v0_preserves_actual_previous_identity_and_provenance(self):
        # The ordinary manifest validator supplies the Entry shape. Signature
        # verification is still the caller's obligation, not this source test.
        record=c.runtime.release_record
        raw=record.build_manifest({'kind':'promote','release':'prod-20261011T000000Z-'+LATER[:8],'sha':LATER,
            'images':{'web':ENTRY.images['web'],'meta-relay':'sha256:'+'8'*64,'gmail-relay':'sha256:'+'9'*64},
            'previous':ENTRY.release,'rolled_back_from':None,'reconciled':False,
            'built_at':ENTRY.deployed_at,'deployed_at':ENTRY.deployed_at,'workflow_sha':LATER,
            'environment_values_sha256':'2'*64,'non_source_projection_sha256':'3'*64})
        previous=record.manifest_entry(record.validate_manifest(c.runtime.common.loads_strict(raw)),raw)
        fake=Git()
        actual=c.CatalogContract(REPO,previous,HEAD,base_env={},capture=fake)
        value=actual.public()
        self.assertEqual((value['previous_sha'],value['candidate_sha']),(LATER,HEAD))
        self.assertEqual(actual.previous_web,self.contract.previous_web)
        self.assertEqual(value['base_overlay'],'equal-reviewed-v0-nop')
        self.assertEqual(value['source_sha256'],dict(c._PINS))
        self.assertEqual(actual.target_sha256,self.contract.target_sha256)
        self.assertEqual(actual.expected_preexisting_tables,self.contract.expected_preexisting_tables)
        self.assertEqual(json.loads(BLOBS[c._PROFILE])['released_source'],c._BASE)
        args=[argv[3:] for argv,_ in fake.calls]
        self.assertIn(['merge-base','--is-ancestor',LATER,HEAD],args)
        self.assertEqual([a[2] for a in args if a[:2]==['cat-file','blob'] and a[2].startswith(LATER+':')],
                         [LATER+':'+path for path in c._PINS])
        self.assertFalse(any(a[0]=='ls-tree' for a in args))

    def test_head_mutation_or_existing_historical_base_overlay_refuses(self):
        mutations=[(HEAD,path) for path in c._PINS]+[(c._BASE,path) for path in c._PINS if path not in {c._PROFILE,c._OVERLAY}]
        for sha,path in mutations:
            with self.subTest(sha=sha,path=path):
                fake=Git();fake.changed=lambda a,b:a==sha and b==path
                with self.assertRaises(c.proof.EvidenceError):c.CatalogContract(REPO,ENTRY,HEAD,base_env={},capture=fake)
        fake=Git();fake.present=True
        with self.assertRaises(c.proof.EvidenceError):c.CatalogContract(REPO,ENTRY,HEAD,base_env={},capture=fake)

    def test_later_missing_or_modified_artifact_never_uses_historical_exception(self):
        for path in c._PINS:
            for kind in ('missing','modified'):
                with self.subTest(path=path,kind=kind):
                    fake=Git()
                    if kind=='missing':fake.missing.add((LATER,path))
                    else:fake.changed=lambda sha,name:sha==LATER and name==path
                    with self.assertRaises(c.proof.EvidenceError) as got:
                        c.CatalogContract(REPO,ENTRY._replace(sha=LATER),HEAD,base_env={},capture=fake)
                    self.assertEqual(str(got.exception),'catalog-source-invalid' if kind=='missing' else 'catalog-fallback-not-equal')
                    self.assertNotIn('private',str(got.exception))
                    self.assertFalse(any(argv[3]=='ls-tree' for argv,_ in fake.calls))

    def test_invalid_or_foreign_commit_and_nonancestor_refuse(self):
        for value in (None,True,'main','a'*39,'A'*40,LATER+'\n',LATER+':private'):
            for field in ('previous','candidate'):
                with self.subTest(value=value,field=field):
                    fake=Git()
                    with self.assertRaises(c.proof.EvidenceError):
                        c.CatalogContract(REPO,ENTRY._replace(sha=value if field=='previous' else LATER),
                            value if field=='candidate' else HEAD,base_env={},capture=fake)
                    self.assertEqual(fake.calls,[])
        for field in ('previous','candidate'):
            fake=Git()
            with self.assertRaises(c.proof.EvidenceError) as got:
                c.CatalogContract(REPO,ENTRY._replace(sha='b'*40 if field=='previous' else LATER),
                    'b'*40 if field=='candidate' else HEAD,base_env={},capture=fake)
            self.assertEqual(str(got.exception),'catalog-source-invalid')
        fake=Git();fake.ancestor=False
        with self.assertRaises(c.proof.EvidenceError):
            c.CatalogContract(REPO,ENTRY._replace(sha=LATER),HEAD,base_env={},capture=fake)
        self.assertFalse(any(argv[3:5]==['cat-file','blob'] for argv,_ in fake.calls))

    def test_source_read_failure_not_empty_overlay(self):
        fake=Git();fake.fail=True
        with self.assertRaises(c.proof.EvidenceError) as got:c.CatalogContract(REPO,ENTRY,HEAD,base_env={},capture=fake)
        self.assertNotIn('private',str(got.exception))

    def test_compare_false_72_is_valid_without_rehashing_catalog(self):
        self.assertEqual(len(self.good['differences']),72)
        self.assertEqual(self.contract.parse_compare(self.encoded(self.good)),self.contract.target_sha256)
        changed=copy.deepcopy(self.good);changed['sha256']='a'*64
        self.assertEqual(self.contract.parse_compare(self.encoded(changed)),'a'*64)
        equal=copy.deepcopy(self.good);equal['match']=True;equal['differences']=[]
        self.assertEqual(self.contract.parse_compare(self.encoded(equal)),self.contract.target_sha256)

    def test_strict_json_and_closed_known_difference_vocabulary(self):
        original=self.encoded(self.good)
        cases=[original+b'{}', original.replace(b'"match": false',b'"match": false,"match": false'),
               original.replace(b'"match": false',b'"match": NaN'),b'notjson\n',original[:-1]]
        for key,value in (('sha256','bad'),('match',1),('match',True),('unknown',[{'kind':'functions','count':1}]),
                          ('seeds',[]),('count_only',{}),('ownership',{'migration_owner_owns_database':True,'migration_owner_owns_public_schema':1})):
            data=copy.deepcopy(self.good);data[key]=value;cases.append(self.encoded(data))
        for difference in ({'kind':'columns','identity':'private.unknown','status':'changed'},
                           {'kind':'columns','identity':'organizations.id','status':'anything'},
                           {'kind':'columns','identity':'organizations.id','status':{}}):
            data=copy.deepcopy(self.good);data['differences']=[difference];cases.append(self.encoded(data))
        data=copy.deepcopy(self.good);data['differences'].append(data['differences'][0]);cases.append(self.encoded(data))
        data=copy.deepcopy(self.good);data['count_only']['oids']=False;cases.append(self.encoded(data))
        for raw in cases:
            with self.subTest(size=len(raw)):
                with self.assertRaises(c.proof.EvidenceError):self.contract.parse_compare(raw)

    def test_inventory_cannot_come_from_incomplete_capture(self):
        good=dict.fromkeys(self.contract.expected_preexisting_tables,'0'*64)
        for values in ({},dict(list(good.items())[:-1]),{**good,'a'*64:'0'*64}):
            capture=c.postgres.TableCapture(object(),{},values,True)
            with self.assertRaises(c.proof.EvidenceError):self.contract.check_initial_tables(capture)
        with self.assertRaises(c.proof.EvidenceError):self.contract.check_initial_tables(c.postgres.TableCapture(object(),{},good,False))

    def setup_owned(self):
        tmp=tempfile.TemporaryDirectory();self.addCleanup(tmp.cleanup)
        self.world=World()
        self.owner=c.processes.stack.OwnedStack('docker',Path(tmp.name),test_stack.SOURCE,
            c.processes.stack.GraphImage(test_stack.GRAPH,test_stack.SOURCE),token=test_stack.TOKEN,capture=self.world.capture)
        self.owner.start()
        self.manager=c.processes.OwnedProcesses(self.owner,
            c.processes.ImageBinding(test_processes.PREV,'sha256:'+'4'*64,c._BASE,test_processes.RECIPE),
            c.processes.ImageBinding(test_processes.CAND,test_processes.CAND,HEAD,test_processes.RECIPE),
            c.processes.ImageBinding(test_processes.CONTROL,test_processes.CONTROL,'f'*40,test_processes.RECIPE))
        self.pg=c.postgres.SyntheticPostgres('docker',Path(tmp.name),self.owner._ids['pg'],self.owner.token,'compat_runtime',capture=self.world.capture)
        self.world.catalog_response=self.encoded(self.good)
        return c.OwnedCatalog(self.manager,self.pg,self.contract)

    def test_actual_classes_cli_then_digests_and_fixed_projection(self):
        observer=self.setup_owned()
        seen,before=observer.initial(test_processes.INPUTS)
        self.assertEqual(seen.sha256,self.contract.target_sha256)
        self.assertEqual(before.identities(),list(self.contract.expected_preexisting_tables))
        self.assertFalse(self.manager._ids)
        first_data=self.world.data_calls[0]
        calls=[args for args,_ in self.world.calls]
        log_index=next(i for i,args in enumerate(calls) if args[:1]==['logs'])
        self.assertLess(log_index,first_data)
        self.world.inventory[0][2].append(['added','pg_catalog','text',-1,2])
        _,forward=observer.after_phase(test_processes.INPUTS,original_columns=True)
        self.assertEqual(forward.public(),before.public())
        _,d0=observer.after_phase(test_processes.INPUTS)
        self.assertNotEqual(d0.public(),before.public())
        self.assertEqual(set(d0.identities()),set(before.identities()))
        with self.assertRaises(c.proof.EvidenceError):observer.initial(test_processes.INPUTS)
        self.manager.close();self.owner.cleanup()

    def test_active_phase_and_missing_inventory_refuse(self):
        observer=self.setup_owned();self.pg._phase=object();before=len(self.world.mutations)
        with self.assertRaises(c.proof.EvidenceError):observer.initial(test_processes.INPUTS)
        self.assertEqual(len(self.world.mutations),before)
        observer=self.setup_owned();self.world.inventory.pop()
        with self.assertRaises(c.proof.EvidenceError):observer.initial(test_processes.INPUTS)
        self.assertTrue(self.manager._protocol_failed)

    def test_cli_nonzero_stderr_malformed_or_catalog_mismatch_never_initial_success(self):
        for kind in ('nonzero','stderr','malformed','mismatch'):
            with self.subTest(kind=kind):
                observer=self.setup_owned()
                if kind=='nonzero':self.world.catalog_exit=1
                if kind=='stderr':self.world.catalog_stderr=b'fixed-error'
                if kind=='malformed':self.world.catalog_response=b'{}\n'
                if kind=='mismatch':
                    changed=copy.deepcopy(self.good);changed['sha256']='0'*64;self.world.catalog_response=self.encoded(changed)
                with self.assertRaises(c.proof.EvidenceError):observer.initial(test_processes.INPUTS)
                self.assertTrue(self.manager._protocol_failed)
                self.assertFalse(self.manager._ids)
                self.assertFalse(self.world.data_calls)

    def test_quiescence_and_image_binding_refuse(self):
        observer=self.setup_owned();self.world.state[9]=1
        with self.assertRaises(c.proof.EvidenceError):observer.initial(test_processes.INPUTS)
        self.assertFalse(self.world.data_calls)
        observer=self.setup_owned();self.manager.bindings['candidate']=replace(self.manager.bindings['candidate'],source_sha='a'*40)
        with self.assertRaises(c.proof.EvidenceError):c.OwnedCatalog(self.manager,self.pg,self.contract)

    def test_only_exact_fixed_cli_diagnostics_refine_refusal(self):
        cases = [
            (2, b'catalog-snapshot:invalid-arguments; use -help\n', 'catalog-cli-invalid-arguments'),
            (1, b'catalog-snapshot:configuration-unavailable\n', 'catalog-cli-configuration-unavailable'),
            (1, b'catalog-snapshot:capture-failed\n', 'catalog-cli-capture-failed'),
            (1, b'catalog-snapshot:output-failed\n', 'catalog-cli-output-failed'),
            (0, b'catalog-snapshot:capture-failed\n', 'catalog-cli-failed'),
            (2, b'catalog-snapshot:capture-failed\n', 'catalog-cli-failed'),
            (1, b'catalog-snapshot:capture-failed\nprivate DSN', 'catalog-cli-failed'),
            (1, b'private DSN\ncatalog-snapshot:capture-failed\n', 'catalog-cli-failed'),
            (1, b'', 'catalog-cli-failed'),
        ]
        for exit_code, diagnostic, expected in cases:
            with self.subTest(exit_code=exit_code, expected=expected):
                observer = self.setup_owned()
                self.world.catalog_exit = exit_code
                self.world.catalog_stderr = diagnostic
                with self.assertRaises(c.proof.EvidenceError) as got:
                    observer.initial(test_processes.INPUTS)
                self.assertEqual(str(got.exception), expected)
                self.assertTrue(observer._failed)
                self.assertTrue(self.manager._protocol_failed)
                self.assertFalse(self.manager._ids)
                self.assertFalse(self.world.data_calls)


if __name__=='__main__':unittest.main(verbosity=2)
