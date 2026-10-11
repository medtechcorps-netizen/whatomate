"""First-baseline local image admission, disabled pending independent review.

The selected candidate is a clean immutable commit, exported from Git objects.
The only build is the fixed release web recipe, on local Linux/amd64. Supporting
images retain their actual sources and require the three reviewed receipts plus
fresh inspections and exact build-input parity. Labels alone are not evidence.
No registry publication, credentials, arbitrary build arguments or public proof
dictionaries are accepted. Raw subprocess output stays private.
"""
from __future__ import annotations

from contextlib import contextmanager
from dataclasses import dataclass
import hashlib
import json
import math
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import uuid

import fixture
import processes
import runtime
import stack

proof, common = runtime.proof, runtime.common
_READY_FOR_EXECUTION = False
_TOTAL = 2400  # Leaves300s inside the canonical45-minute workflow job.
_BUILD = 1500
_EXPERIMENT_MIN = 720
_CLEANUP = 180
_MAX_CONTEXT = 64 * runtime.MIB
_WEB_RECIPE = 'docker/release/web.Dockerfile'
_WEB_RECIPE_SHA = 'bc081fef7a7b90f268b672dd9da10e3b36146e32faab59d1659838847ec806fa'
_IGNORE_SHA = 'b77c48c1111912668497b028767ac88974d905cfbeb3d669847020d87cd879c6'
_PLUGIN_SHA = 'e56147d0d19281573362a9a0ec89a20eacdce4b8d2b25402167299fcdfa1fc67'
_SOURCE_URL = 'https://github.com/' + common.REPOSITORY
_RECEIPTS = {
    'graph': ('graph-build.json', 'aef661c9527ed1fd7c31dabf23cd0617c2d6f860cae2c92d5fa22ed3f4bb91e0'),
    'controller': ('controller-build.json', '5bbac14fe503468e82a65dbfc64b15b2909fe4197a6f5887768f127e735e65ef'),
    'fixture': ('fixture-build.json', '98daa4f1d55e842ed8d10708f6a2eb735cdaf4cbb57a15c44316104cb1a80445'),
}
_SUPPORT = {
    'graph': ('e59b87c1fd6c22144f77303fa52258b5ef2c9ba7',
              'sha256:87402da745f507063f72ebf06c8d2561a77b970cdbe59595fc7e7256322c682e', stack.GRAPH_RECIPE_SHA256),
    'controller': ('efd3ecc7479da4c2da6e34c9a11bf0a803c7f388',
                   'sha256:02ff0d507936bef1d0c43bc84846f5ccce6639adc2ffb1977c38412af73b9956',
                   '5dc4fb16e685d6569ac184431a50743617320bef47ca638b15912ef0b04ef812'),
    'fixture': ('fe3719ce3ecdc4d747b3eec95cd3bd41ad61a43a',
                'sha256:6fd817a63e42a2a2f2b7b55bf91fe2f5f117ac7f04ccf58d8b49f1a7d8cb1eeb',
                '947cf1674e35d8e6f77f1da9c7b1dd45b282bff34e77600e52225cfba95f7895'),
}
_SEAL = object()


def need(value, code='image-admission-invalid'):
    if not value:
        raise proof.EvidenceError(code)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


class _Deadline:
    """One private clock for verification, build, experiment AND cleanup."""
    def __init__(self, *, clock=time.monotonic):
        self.clock = clock
        self.last = None
        self.end = self.now() + _TOTAL

    def now(self):
        value = self.clock()
        need(type(value) in (float, int) and math.isfinite(value) and value >= 0
             and (self.last is None or value >= self.last), 'admission-clock-invalid')
        self.last = value
        return value

    def remaining(self):
        left = self.end - self.now()
        if left <= 0:
            raise proof.InfrastructureFailure('admission-deadline')
        return left

    def build_seconds(self):
        left = self.remaining() - _EXPERIMENT_MIN - _CLEANUP
        if left <= 0:
            raise proof.InfrastructureFailure('admission-build-reserve')
        return min(_BUILD, left)

    def experiment_seconds(self):
        left = self.remaining() - _CLEANUP
        if left < _EXPERIMENT_MIN:
            raise proof.InfrastructureFailure('admission-experiment-reserve')
        return min(2400, left)

    def capture(self, argv, **options):
        options['timeout'] = min(options['timeout'], runtime.MAX_SECONDS, self.remaining())
        result = runtime.bounded_command(argv, **options)
        self.remaining()
        return result


@contextmanager
def _verification_transport(client, deadline):
    """Keep trust/ancestry algorithms; bound only their existing transports.

    This scope is exclusive. Foreign replacements are neither invoked nor
    overwritten; the next stage cannot proceed after ambiguous restoration.
    """
    need(type(client) is runtime.ReadOnlyGh, 'admission-release-client-invalid')
    original_git, original_capture = runtime.release_record.git, client.capture
    need(original_git is _ORIGINAL_GIT and original_capture is runtime.bounded_command,
         'admission-transport-in-use')
    def git(repo, args, *, code, **extra):
        need(not extra, 'admission-git-override')
        return deadline.capture(['git', '-C', str(repo), *args],
            env=common.scrubbed_env({'GIT_TERMINAL_PROMPT':'0', 'LC_ALL':'C'}),
            timeout=30, stdout_limit=_MAX_CONTEXT, stderr_limit=runtime.MIB)
    bounded = deadline.capture
    runtime.release_record.git, client.capture = git, bounded
    try:
        deadline.remaining()
        yield
        deadline.remaining()
    finally:
        need(runtime.release_record.git is git and client.capture is bounded,
             'admission-transport-replaced')
        runtime.release_record.git, client.capture = original_git, original_capture


_ORIGINAL_GIT = runtime.release_record.git


def _regular(path, limit):
    need(isinstance(path, Path) and path.is_absolute() and path.resolve() == path
         and path.is_file() and not path.is_symlink()
         and not any(getattr(p, 'is_junction', lambda: False)() or p.is_symlink() for p in [path, *path.parents]),
         'admission-file-invalid')
    with path.open('rb') as stream:
        raw = stream.read(limit + 1)
    need(len(raw) <= limit, 'admission-file-bound')
    return raw


class _Source:
    def __init__(self, repo, candidate, deadline):
        need(isinstance(repo, Path) and repo.is_absolute() and repo.resolve() == repo
             and type(candidate) is str and stack._SHA.fullmatch(candidate))
        self.repo, self.candidate, self.deadline = repo, candidate, deadline
        self.env = common.scrubbed_env({'GIT_TERMINAL_PROMPT':'0','LC_ALL':'C'})
        self.clean()
        self.tree = self.git(['rev-parse', candidate+'^{tree}'], 1024).decode('ascii').strip()
        need(stack._SHA.fullmatch(self.tree))

    def git(self, args, limit=_MAX_CONTEXT, data=None):
        value = self.deadline.capture(['git','-C',str(self.repo),*args], env=dict(self.env), timeout=30,
            stdout_limit=limit, stderr_limit=runtime.MIB, input_bytes=data)
        need(type(value) is runtime.Capture and value.returncode == 0 and not value.stderr,
             'admission-source-read')
        return value.stdout

    def clean(self):
        need(self.git(['rev-parse','HEAD'],1024).strip() == self.candidate.encode(), 'admission-head-changed')
        need(self.git(['status','--porcelain=v1','--untracked-files=all']) == b'', 'admission-source-dirty')

    def inventory(self, commit):
        raw = self.git(['ls-tree','-rz','--full-tree',commit])
        rows, folded = {}, set()
        for record in raw.split(b'\0')[:-1]:
            meta, sep, rawpath = record.partition(b'\t')
            fields = meta.split(b' ')
            try:
                path = rawpath.decode('utf-8','strict')
            except UnicodeError:
                raise proof.EvidenceError('admission-path-invalid') from None
            need(sep and len(fields)==3 and fields[:2]==[b'100644',b'blob']
                 and re.fullmatch(rb'[0-9a-f]{40}',fields[2]), 'admission-source-mode')
            need(re.fullmatch(r'[A-Za-z0-9_.@+/-]+',path) and not path.startswith('/')
                 and all(v not in ('','.','..') and not v.endswith('.')
                         and not re.fullmatch(r'(?i)(con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\..*)?',v)
                         for v in path.split('/'))
                 and path.casefold() not in folded, 'admission-path-invalid')
            folded.add(path.casefold()); rows[path] = fields[2].decode()
        need(raw.endswith(b'\0') and 0 < len(rows) <= 10000, 'admission-tree-invalid')
        return rows

    def blobs(self, inventory):
        names = sorted(inventory)
        raw = self.git(['cat-file','--batch'],data=('\n'.join(inventory[n] for n in names)+'\n').encode())
        at, result = 0, {}
        for name in names:
            end = raw.find(b'\n',at); need(end >= at, 'admission-blob-frame')
            fields = raw[at:end].split(b' ')
            need(len(fields)==3 and fields[0].decode()==inventory[name] and fields[1]==b'blob'
                 and re.fullmatch(rb'[0-9]+',fields[2]), 'admission-blob-frame')
            size = int(fields[2]); start=end+1; at=start+size+1
            need(size <= 8*runtime.MIB and at <= len(raw) and raw[at-1:at]==b'\n', 'admission-blob-frame')
            blob=raw[start:at-1]
            need(hashlib.sha1(b'blob '+str(size).encode()+b'\0'+blob).hexdigest()==inventory[name], 'admission-blob-changed')
            result[name] = blob
        need(at==len(raw),'admission-blob-frame')
        return result


def _export(root, blobs):
    need(not root.exists(), 'admission-context-exists')
    root.mkdir()
    for name,raw in sorted(blobs.items()):
        target = root/name
        target.parent.mkdir(parents=True,exist_ok=True)
        with target.open('xb') as stream: stream.write(raw)
        target.chmod(0o644)
    _check_export(root,blobs)


def _check_export(root,blobs):
    seen = set()
    for path in root.rglob('*'):
        need(not path.is_symlink() and not getattr(path,'is_junction',lambda:False)(), 'admission-context-changed')
        if path.is_file():
            name=path.relative_to(root).as_posix(); seen.add(name)
            need(name in blobs and _regular(path,8*runtime.MIB)==blobs[name], 'admission-context-changed')
    need(seen==set(blobs), 'admission-context-changed')


def _receipts(directory):
    need(isinstance(directory,Path) and directory.is_absolute() and directory.resolve()==directory
         and directory.is_dir() and not directory.is_symlink(), 'admission-receipts-invalid')
    need({p.name for p in directory.iterdir()} == {v[0] for v in _RECEIPTS.values()}, 'admission-receipts-invalid')
    result={}
    for role,(name,digest) in _RECEIPTS.items():
        raw=_regular(directory/name,runtime.MIB)
        need(sha(raw)==digest,'admission-receipt-changed')
        try: result[role]=common.loads_strict(raw)
        except common.ReleaseError: raise proof.EvidenceError('admission-receipt-invalid') from None
    return result


def _supporting(source,blobs,receipts):
    for role,receipt in receipts.items():
        old,image,recipe=_SUPPORT[role]
        if role=='graph':
            need(receipt['head']==old and receipt['graph_image_id']==image and receipt['graph_image_built'] is True
                 and receipt['build_output']['exit_code']==0,'admission-receipt-invalid')
            rows=receipt['graph_source_files']
            expected=sorted([n for n in blobs if n.startswith('release/staging/graphstub/') and not n.endswith('_test.go')]
                +['go.mod','go.sum','docker/staging/graph-stub.Dockerfile','docker/staging/graph-stub.Dockerfile.dockerignore'])
            need(len(expected)==16 and [v['path'] for v in rows]==expected,'admission-support-changed')
        elif role=='controller':
            need(receipt['source']==old and receipt['image_id']==image and receipt['recipe_sha256']==recipe
                 and receipt['containers_unchanged'] is True,'admission-receipt-invalid')
            rows=receipt['files']
            need([v['path'] for v in rows]==['release/compat/controller.Dockerfile','release/compat/controller.py',
                 'release/compat/sweep.py','release/compat/proof.py'],'admission-support-changed')
        else:
            need(receipt['source']==old and receipt['image_id']==image and receipt['recipe_sha256']==recipe
                 and receipt['exit_code']==0 and receipt['containers_unchanged'] is True,'admission-receipt-invalid')
            before=source.inventory(old); current=source.inventory(source.candidate)
            for name in set(before)|set(current):
                exempt=name=='docs/compat.md' or (name.startswith('release/compat/') and name.endswith('.py'))
                need(exempt or before.get(name)==current.get(name),'admission-fixture-input-changed')
            need(sha(blobs['release/compat/fixture.Dockerfile'])==recipe,'admission-support-changed')
            continue
        for row in rows:
            name=row['path']; raw=source.git(['cat-file','blob',old+':'+name])
            need(raw==blobs.get(name) and len(raw)==row['bytes'] and sha(raw)==row['sha256'],'admission-support-changed')


@dataclass(frozen=True,repr=False)
class _Admitted:
    seal: object
    previous: processes.ImageBinding
    candidate: processes.ImageBinding
    controller: processes.ImageBinding
    graph: stack.GraphImage
    fixture: fixture.ExportImage
    receipt: dict


def _inspect(binary, config, reference, deadline):
    value=deadline.capture([binary,'--config',str(config),'--context','default','image','inspect',reference],
        env=common.scrubbed_env(),timeout=30,stdout_limit=runtime.MIB,stderr_limit=runtime.MIB)
    need(value.returncode==0 and not value.stderr,'admission-image-unavailable')
    return stack._json(value.stdout)


def _support_inspect(binary, config, deadline):
    # Reuse the two exact metadata validators without constructing ownership
    # managers or mutating containers. Their validation methods have no I/O.
    values={}
    for role,(source,image,recipe) in _SUPPORT.items():
        row=_inspect(binary,config,image,deadline); values[role]=row
        if role=='controller':
            binding=processes.ImageBinding(image,image,source,recipe)
            processes.OwnedProcesses._validate_image(None,'controller',binding,row)
        elif role=='fixture':
            subject=object.__new__(fixture.OwnedFixture)
            subject.image=fixture.ExportImage(image,source,recipe);subject._image=row
            subject._validate_image()
        else:
            cfg=row.get('Config',{});labels=cfg.get('Labels') or {}
            need(row.get('Id')==image and row.get('Os')=='linux' and row.get('Architecture')=='amd64'
                 and labels.get(stack.SOURCE_LABEL)==source and labels.get(stack.ROLE_LABEL)=='graph'
                 and labels.get(stack.RECIPE_LABEL)==recipe and cfg.get('Entrypoint')==['/graph-stub']
                 and not cfg.get('Cmd') and cfg.get('User')=='65532:65532'
                 and cfg.get('Env') in (None,[],[stack._GRAPH_DEFAULT_PATH])
                 and not cfg.get('Volumes') and not cfg.get('OnBuild') and not cfg.get('Healthcheck'),
                 'admission-support-image-invalid')
    return values


def _build_command(plugin, context, work, candidate):
    labels={stack.SOURCE_LABEL:candidate,stack.RECIPE_LABEL:_WEB_RECIPE_SHA,
            'org.opencontainers.image.source':_SOURCE_URL,'org.opencontainers.image.title':'rereply-release-web'}
    args=[str(plugin),'build','--builder','default',
          '--platform','linux/amd64','--file',str(context/_WEB_RECIPE),
          '--build-arg','TARGETOS=linux','--build-arg','TARGETARCH=amd64',
          '--load','--pull=false','--provenance=false','--sbom=false','--progress=plain',
          '--iidfile',str(work/'image.id'),'--metadata-file',str(work/'build-metadata.json')]
    for key,value in sorted(labels.items()): args += ['--label',key+'='+value]
    return args+[str(context)]


def _build_environment(config, work):
    # Docker's empty config still auto-discovers credential helpers. Bind both
    # Windows search locations, not merely the config, and never inherit PATH.
    import ctypes
    from ctypes import wintypes
    kernel=ctypes.WinDLL('kernel32',use_last_error=True)
    kernel.GetSystemDirectoryW.argtypes=[wintypes.LPWSTR,wintypes.UINT]
    kernel.GetSystemDirectoryW.restype=wintypes.UINT
    buffer=ctypes.create_unicode_buffer(32768)
    size=kernel.GetSystemDirectoryW(buffer,len(buffer))
    need(0<size<len(buffer),'admission-system-directory')
    system=Path(buffer.value)
    need(system.is_absolute() and system.is_dir() and work.is_absolute() and work.is_dir(),
         'admission-build-search-path')
    for directory in (system,work):
        need(not any(p.name.lower().startswith('docker-credential-') for p in directory.iterdir()),
             'admission-credential-helper-available')
    # No inherited Docker/Git/Buildx/proxy/auth overrides. Go's current-directory
    # lookup is confined to the fresh attempt directory passed as Popen.cwd.
    result={key:os.environ[key] for key in ('SystemRoot','WINDIR','TEMP','TMP') if key in os.environ}
    result.update(PATH=str(system),PATHEXT='.COM;.EXE;.BAT;.CMD',DOCKER_CONFIG=str(config),
                  DOCKER_CONTEXT='default',BUILDX_GIT_INFO='false',BUILDX_GIT_LABELS='false')
    return result


# Private fixed launcher, not a public command runner. The parent creates one
# unique Job and holds its sole long-lived, noninherited handle. This launcher
# self-assigns before Popen; it closes its short-lived Job handle before spawning
# the plugin. Thus a parent crash closes the last handle and kills the whole Job.
# https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects
_WINDOWS_LAUNCHER = r'''
import ctypes,json,os,subprocess,sys
from ctypes import wintypes as w
k=ctypes.WinDLL('kernel32',use_last_error=True)
k.OpenJobObjectW.argtypes=[w.DWORD,w.BOOL,w.LPCWSTR];k.OpenJobObjectW.restype=w.HANDLE
k.GetCurrentProcess.argtypes=[];k.GetCurrentProcess.restype=w.HANDLE
k.AssignProcessToJobObject.argtypes=[w.HANDLE,w.HANDLE];k.AssignProcessToJobObject.restype=w.BOOL
k.IsProcessInJob.argtypes=[w.HANDLE,w.HANDLE,ctypes.POINTER(w.BOOL)];k.IsProcessInJob.restype=w.BOOL
k.CloseHandle.argtypes=[w.HANDLE];k.CloseHandle.restype=w.BOOL
k.GetHandleInformation.argtypes=[w.HANDLE,ctypes.POINTER(w.DWORD)];k.GetHandleInformation.restype=w.BOOL
try:
    if len(sys.argv)!=3: raise ValueError()
    command=json.loads(sys.argv[2])
    if not isinstance(command,list) or not 0<len(command)<=128: raise ValueError()
    if not all(type(v) is str and len(v)<=8192 and not any(c in v for c in '\0\r\n') for v in command): raise ValueError()
    h=k.OpenJobObjectW(1|4,False,sys.argv[1])
    if not h: raise ValueError()
    flags=w.DWORD();assigned=False
    try:
        if not k.GetHandleInformation(h,ctypes.byref(flags)) or flags.value&1: raise ValueError()
        if not k.AssignProcessToJobObject(h,k.GetCurrentProcess()): raise ValueError()
        member=w.BOOL()
        if not k.IsProcessInJob(k.GetCurrentProcess(),h,ctypes.byref(member)) or not member.value: raise ValueError()
        assigned=True
    finally:
        if not k.CloseHandle(h): assigned=False
    if not assigned: raise ValueError()
    child=subprocess.Popen(command,stdin=subprocess.DEVNULL,stdout=sys.stdout.buffer,stderr=sys.stderr.buffer,
        shell=False,close_fds=True,creationflags=subprocess.CREATE_NO_WINDOW)
    code=child.wait()
except BaseException:
    os._exit(253)
os._exit(code if 0<=code<=0xffffffff else 253)
'''


class _WindowsJob:
    """Only the exact newly created Job is controlled; no PID-tree search."""
    def __init__(self):
        import ctypes
        from ctypes import wintypes as w
        self.c,self.w=ctypes,w
        class Basic(ctypes.Structure):
            _fields_=[('per_process',ctypes.c_longlong),('per_job',ctypes.c_longlong),('flags',w.DWORD),
                ('min_working',ctypes.c_size_t),('max_working',ctypes.c_size_t),('active_limit',w.DWORD),
                ('affinity',ctypes.c_size_t),('priority',w.DWORD),('scheduling',w.DWORD)]
        class IO(ctypes.Structure):
            _fields_=[(name,ctypes.c_ulonglong) for name in ('read','write','other','read_bytes','write_bytes','other_bytes')]
        class Extended(ctypes.Structure):
            _fields_=[('basic',Basic),('io',IO),('process_memory',ctypes.c_size_t),('job_memory',ctypes.c_size_t),
                      ('peak_process',ctypes.c_size_t),('peak_job',ctypes.c_size_t)]
        class Accounting(ctypes.Structure):
            _fields_=[(name,ctypes.c_longlong) for name in ('user','kernel','period_user','period_kernel')]+[
                (name,w.DWORD) for name in ('faults','total','active','terminated')]
        self.accounting=Accounting
        class ProcessList(ctypes.Structure):
            _fields_=[('assigned',w.DWORD),('listed',w.DWORD),('ids',ctypes.c_size_t*256)]
        self.process_list=ProcessList;self.process_handles={}
        k=self.k=ctypes.WinDLL('kernel32',use_last_error=True)
        k.CreateJobObjectW.argtypes=[ctypes.c_void_p,w.LPCWSTR];k.CreateJobObjectW.restype=w.HANDLE
        k.SetInformationJobObject.argtypes=[w.HANDLE,ctypes.c_int,ctypes.c_void_p,w.DWORD];k.SetInformationJobObject.restype=w.BOOL
        k.QueryInformationJobObject.argtypes=[w.HANDLE,ctypes.c_int,ctypes.c_void_p,w.DWORD,ctypes.c_void_p];k.QueryInformationJobObject.restype=w.BOOL
        k.TerminateJobObject.argtypes=[w.HANDLE,w.UINT];k.TerminateJobObject.restype=w.BOOL
        k.CloseHandle.argtypes=[w.HANDLE];k.CloseHandle.restype=w.BOOL
        k.OpenProcess.argtypes=[w.DWORD,w.BOOL,w.DWORD];k.OpenProcess.restype=w.HANDLE
        k.IsProcessInJob.argtypes=[w.HANDLE,w.HANDLE,ctypes.POINTER(w.BOOL)];k.IsProcessInJob.restype=w.BOOL
        k.WaitForSingleObject.argtypes=[w.HANDLE,w.DWORD];k.WaitForSingleObject.restype=w.DWORD
        k.GetHandleInformation.argtypes=[w.HANDLE,ctypes.POINTER(w.DWORD)];k.GetHandleInformation.restype=w.BOOL
        k.PeekNamedPipe.argtypes=[w.HANDLE,ctypes.c_void_p,w.DWORD,ctypes.c_void_p,ctypes.POINTER(w.DWORD),ctypes.c_void_p]
        k.PeekNamedPipe.restype=w.BOOL
        self.name='Local\\codex.compat.build.'+uuid.uuid4().hex
        ctypes.set_last_error(0);self.handle=k.CreateJobObjectW(None,self.name)
        if not self.handle: raise proof.InfrastructureFailure('admission-build-containment')
        try:
            need(ctypes.get_last_error()!=183,'admission-build-job-exists')
            flags=w.DWORD()
            need(k.GetHandleInformation(self.handle,ctypes.byref(flags)) and flags.value&1==0,
                 'admission-build-handle-inherited')
            limits=Extended();limits.basic.flags=0x2000  # KILL_ON_JOB_CLOSE; no breakaway flags.
            need(k.SetInformationJobObject(self.handle,9,ctypes.byref(limits),ctypes.sizeof(limits)),
                 'admission-build-containment')
        except BaseException:
            self.close();raise

    def counts(self):
        self.track()
        value=self.accounting()
        need(self.k.QueryInformationJobObject(self.handle,1,self.c.byref(value),self.c.sizeof(value),None),
             'admission-build-job-query')
        return value.total,value.active

    def track(self):
        # Discover only members of our exact Job. Retained handles prevent a
        # PID race during teardown; no system-wide process enumeration occurs.
        value=self.process_list()
        need(self.k.QueryInformationJobObject(self.handle,3,self.c.byref(value),self.c.sizeof(value),None)
             and value.assigned==value.listed and value.listed<=256,'admission-build-job-inventory')
        for pid in value.ids[:value.listed]:
            if pid in self.process_handles: continue
            handle=self.k.OpenProcess(0x100000|0x400,False,pid)
            if not handle:
                if self.c.get_last_error()==87: continue  # Already exited before OpenProcess.
                raise proof.InfrastructureFailure('admission-build-process-handle')
            member=self.w.BOOL()
            if not self.k.IsProcessInJob(handle,self.handle,self.c.byref(member)) or not member.value:
                self.k.CloseHandle(handle)
                raise proof.InfrastructureFailure('admission-build-process-membership')
            self.process_handles[pid]=handle

    def wait_handles(self,end,deadline):
        while True:
            statuses=[self.k.WaitForSingleObject(handle,0) for handle in self.process_handles.values()]
            need(all(value in (0,258) for value in statuses),'admission-build-process-wait')
            if all(value==0 for value in statuses): return
            if deadline.now()>=end: raise proof.InfrastructureFailure('admission-build-teardown-unresolved')
            time.sleep(.005)

    def terminate(self):
        need(self.k.TerminateJobObject(self.handle,254),'admission-build-job-terminate')

    def close(self):
        try:
            if self.handle:
                handle,self.handle=self.handle,None
                need(self.k.CloseHandle(handle),'admission-build-job-close')
        finally:
            for handle in self.process_handles.values(): self.k.CloseHandle(handle)
            self.process_handles.clear()

    def read(self,pipe,limit):
        import msvcrt
        available=self.w.DWORD()
        if not self.k.PeekNamedPipe(msvcrt.get_osfhandle(pipe.fileno()),None,0,None,self.c.byref(available),None):
            if self.c.get_last_error()==109: return b''  # ERROR_BROKEN_PIPE: EOF only.
            raise proof.InfrastructureFailure('admission-build-pipe')
        if not available.value: return None
        # This is the only reader. Read no more than Peek reported; no blocking
        # pipe reads or drain threads survive teardown.
        return os.read(pipe.fileno(),min(limit,available.value))


def _build_capture(argv, env, seconds, deadline):
    """Private bounded build capture with owned client-tree termination.

    Windows launcher joins the new kill-on-close Job before any child spawn;
    POSIX retains its private process group. Alternating nonblocking pipe reads
    need no reader threads. No shell/stdin, finite16MiB per stream. Terminating
    the owned clients still does not prove cancellation of daemon build work.
    Timeout/unknown never admits an IID or replays the invocation.
    """
    need(type(seconds) in (float,int) and math.isfinite(seconds) and 0<seconds<=_BUILD)
    end=min(deadline.end-5,deadline.now()+seconds)
    teardown_end=min(deadline.end,end+5)
    need(end>deadline.now(),'admission-build-reserve')
    child=job=None; pipes=()
    try:
        command=argv
        if os.name=='nt':
            job=_WindowsJob()
            command=[sys.executable,'-I','-S','-B','-c',_WINDOWS_LAUNCHER,job.name,json.dumps(argv)]
        try:
            # The module-owned fixed build argv is the sole caller in admission;
            # local tests substitute harmless Python children at this private boundary.
            cwd=str(Path(argv[argv.index('--iidfile')+1]).parent) if '--iidfile' in argv else None
            child=subprocess.Popen(command,env=env,cwd=cwd,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,bufsize=0,shell=False,close_fds=True,
                start_new_session=os.name=='posix',creationflags=subprocess.CREATE_NO_WINDOW if os.name=='nt' else 0)
        except (OSError,ValueError,subprocess.SubprocessError):
            raise proof.InfrastructureFailure('admission-build-start') from None
        pipes=(child.stdout,child.stderr);buffers=[bytearray(),bytearray()];eof=[False,False]
        if job is None:
            for pipe in pipes: os.set_blocking(pipe.fileno(),False)
        while True:
            if deadline.now()>=end: raise proof.InfrastructureFailure('admission-build-unknown')
            if job is not None: job.track()
            for i,pipe in enumerate(pipes):
                if eof[i]: continue
                size=min(65536,16*runtime.MIB-len(buffers[i])+1)
                if job is not None: part=job.read(pipe,size)
                else:
                    try: part=os.read(pipe.fileno(),size)
                    except BlockingIOError: part=None
                if part==b'': eof[i]=True
                elif part is not None:
                    if len(buffers[i])+len(part)>16*runtime.MIB:
                        raise proof.InfrastructureFailure('admission-build-output-bound')
                    buffers[i].extend(part)
            code=child.poll()
            if code is not None:
                if job is not None:
                    total,active=job.counts()
                    need(total>=2,'admission-build-containment')  # Launcher AND plugin ran in the Job.
                    if active: raise proof.InfrastructureFailure('admission-build-descendants')
                if all(eof): break
            # No sleep when a full chunk was available would improve throughput,
            # but even this fixed short poll remains below the output/time bounds.
            time.sleep(min(.001,max(0,end-deadline.now())))
        return runtime.Capture(code,bytes(buffers[0]),bytes(buffers[1]))
    finally:
        unresolved=False
        try:
            if job is not None and job.counts()[1]: job.terminate()
            if child is not None:
                if job is None:
                    try: os.killpg(child.pid,signal.SIGKILL)
                    except ProcessLookupError: pass
                elif child.poll() is None:
                    # Before self-assignment it cannot have spawned a plugin.
                    # Killing this exact launcher also handles containment failure.
                    child.kill()
                left=max(0,teardown_end-deadline.now())
                child.wait(timeout=left)
            if job is not None:
                while job.counts()[1]:
                    if deadline.now()>=teardown_end:
                        unresolved=True;break
                    time.sleep(.005)
                job.wait_handles(teardown_end,deadline)
            if deadline.now()>teardown_end: unresolved=True
        except (OSError,subprocess.SubprocessError,proof.EvidenceError):
            unresolved=True
        finally:
            for pipe in pipes: pipe.close()
            if job is not None: job.close()  # Kill-on-close is also the final fail-closed backstop.
        if unresolved: raise proof.InfrastructureFailure('admission-build-teardown-unresolved')


def _save(path,value):
    raw=(json.dumps(value,sort_keys=True,indent=2)+'\n').encode()
    with path.open('xb') as stream: stream.write(raw)


def _builder_plugin(binary):
    need(os.name=='nt' and Path(binary).is_absolute() and len(Path(binary).parents)>=4,
         'admission-local-platform-unsupported')
    plugin=Path(binary).parents[3]/'cli-plugins/docker-buildx.exe'
    need(sha(_regular(plugin,128*runtime.MIB))==_PLUGIN_SHA,'admission-builder-unproven')
    return plugin


def _admit(repo,previous,candidate,binary,config,receipt_dir,deadline):
    """Internal factory. Historical cache receipts support only this baseline.

    This is not fresh-runner CI admission. A future hosted adapter must build
    its supporting images from its own verified source. No local path or
    caller-supplied image identity is embedded in this interface.
    """
    source=_Source(repo,candidate,deadline)
    blobs=source.blobs(source.inventory(candidate))
    need(sha(blobs.get(_WEB_RECIPE,b''))==_WEB_RECIPE_SHA and sha(blobs.get('.dockerignore',b''))==_IGNORE_SHA,
         'admission-recipe-unsupported')
    need(_WEB_RECIPE+'.dockerignore' not in blobs, 'admission-ignore-override')
    receipts=_receipts(receipt_dir)
    _supporting(source,blobs,receipts)
    # Runtime config is separately required to be empty; build config gets
    # only the verified plugin directory, never credentials or Desktop auth.
    runtime._empty_directory(config)
    plugin=_builder_plugin(binary)
    info=deadline.capture([binary,'--config',str(config),'--context','default','info','--format','{{json .}}'],
        env=common.scrubbed_env(),timeout=30,stdout_limit=runtime.MIB,stderr_limit=runtime.MIB)
    need(info.returncode==0 and not info.stderr,'admission-engine-unavailable')
    engine=common.loads_strict(info.stdout)
    need(engine.get('OSType')=='linux' and engine.get('Architecture') in ('x86_64','amd64')
         and type(engine.get('ID')) is str and engine['ID'],'admission-engine-invalid')
    cache=_support_inspect(binary,config,deadline)
    digest=previous.images.get('web')
    need(type(digest) is str and common.DIGEST_RE.fullmatch(digest),'admission-previous-invalid')
    reference=common.IMAGE_REPOSITORY['web']+'@'+digest
    row=runtime.Docker(binary,config,capture=deadline.capture).inspect(reference)
    previous_recipe=sha(source.git(['cat-file','blob',previous.sha+':'+_WEB_RECIPE]))
    previous_binding=processes.ImageBinding(reference,row['Id'],previous.sha,previous_recipe)
    processes.OwnedProcesses._validate_image(None,'previous',previous_binding,row)
    # Persistent create-only attempt: failed/unknown builds preserve everything,
    # never retry or adopt an image by a friendly name.
    work=receipt_dir.parent/('candidate-'+candidate)
    need(not work.exists(),'admission-already-attempted')
    work.mkdir(); context=work/'context'; build_config=work/'build-config';build_config.mkdir()
    _save(build_config/'config.json',{'cliPluginsExtraDirs':[str(plugin.parent)]})
    _export(context,blobs)
    manifest=[{'path':name,'mode':'100644','bytes':len(raw),'sha256':sha(raw),
               'git_blob':hashlib.sha1(b'blob '+str(len(raw)).encode()+b'\0'+raw).hexdigest()}
              for name,raw in sorted(blobs.items())]
    _save(work/'source.json',{'source':candidate,'tree':source.tree,'files':manifest})
    seconds=deadline.build_seconds()
    receipt={'source':candidate,'tree':source.tree,'recipe_sha256':_WEB_RECIPE_SHA,
             'source_manifest_sha256':sha((work/'source.json').read_bytes()),
             'engine_id_sha256':sha(engine['ID'].encode()),'plugin_sha256':_PLUGIN_SHA,
             'supporting_receipts':{role:digest for role,(_,digest) in _RECEIPTS.items()},
             'build_seconds_cap':seconds,'release_attestation':False,'registry_publication':False}
    receipt['scope']='initial local cache-backed admission; not fresh-runner CI or signed publication'
    receipt['source_mode']='Git inputs all regular100644; host directory mode translation is not independently attested'
    receipt['build_invocations']=1
    _save(work/'intent.json',receipt)
    try:
        command=_build_command(plugin,context,work,candidate)
        build_env=_build_environment(build_config,work)
        value=_build_capture(command,build_env,seconds,deadline)
        need(type(value) is runtime.Capture and type(value.returncode) is int
             and type(value.stdout) is bytes and type(value.stderr) is bytes,
             'admission-build-capture')
        receipt.update(exit_code=value.returncode,stdout_bytes=len(value.stdout),stderr_bytes=len(value.stderr),
                       stdout_sha256=sha(value.stdout),stderr_sha256=sha(value.stderr),build_outcome='completed')
        need(value.returncode==0,'admission-build-failed')
        deadline.remaining(); _check_export(context,blobs); source.clean()
        need(source.tree==source.git(['rev-parse',candidate+'^{tree}'],1024).decode().strip(),'admission-source-changed')
        need(sha(_regular(plugin,128*runtime.MIB))==_PLUGIN_SHA,'admission-builder-changed')
        need(common.loads_strict(_regular(build_config/'config.json',4096))=={'cliPluginsExtraDirs':[str(plugin.parent)]},
             'admission-build-config-changed')
        need(_build_environment(build_config,work)==build_env,'admission-build-environment-changed')
        need(_receipts(receipt_dir)==receipts,'admission-receipts-changed')
        iid=_regular(work/'image.id',128).decode('ascii','strict').strip()
        need(stack._IMAGE.fullmatch(iid),'admission-iid-invalid')
        metadata=common.loads_strict(_regular(work/'build-metadata.json',runtime.MIB))
        need(type(metadata) is dict and metadata.get('containerimage.config.digest')==iid,'admission-iid-unbound')
        row=_inspect(binary,config,iid,deadline)
        binding=processes.ImageBinding(iid,iid,candidate,_WEB_RECIPE_SHA)
        processes.OwnedProcesses._validate_image(None,'candidate',binding,row)
        need((row['Config'].get('Labels') or {}).get(stack.RECIPE_LABEL)==_WEB_RECIPE_SHA,'admission-recipe-label-missing')
        need(_support_inspect(binary,config,deadline)==cache,'admission-support-image-changed')
        final_info=deadline.capture([binary,'--config',str(config),'--context','default','info','--format','{{json .}}'],
            env=common.scrubbed_env(),timeout=30,stdout_limit=runtime.MIB,stderr_limit=runtime.MIB)
        need(final_info.returncode==0 and not final_info.stderr,'admission-engine-unavailable')
        final_engine=common.loads_strict(final_info.stdout)
        need(all(final_engine.get(key)==engine.get(key) for key in ('ID','OSType','Architecture','ServerVersion')),
             'admission-engine-changed')
        runtime._empty_directory(config)
        deadline.experiment_seconds()
        receipt.update(status='BUILT_AND_ADMITTED_LOCAL_UNATTESTED',image_id=iid,
                       metadata_sha256=sha((work/'build-metadata.json').read_bytes()),
                       remaining_seconds=deadline.remaining())
        _save(work/'result.json',receipt)
        graph,control,fix=(_SUPPORT[role] for role in ('graph','controller','fixture'))
        return _Admitted(_SEAL,previous_binding,binding,
            processes.ImageBinding(control[1],control[1],control[0],control[2]),
            stack.GraphImage(graph[1],graph[0]),fixture.ExportImage(fix[1],fix[0],fix[2]),receipt)
    except Exception as error:
        receipt['status']='REFUSED_NO_RETRY_OR_IMAGE_ADOPTION'
        receipt.setdefault('build_outcome','unknown')
        receipt['failure_code']='admission-build-unknown' if error.args==('admission-build-unknown',) else 'admission-incomplete'
        if not (work/'result.json').exists(): _save(work/'result.json',receipt)
        raise


