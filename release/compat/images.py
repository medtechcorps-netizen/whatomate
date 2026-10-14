"""Reviewed source-built image admission for compatibility qualification.

The selected candidate is a clean immutable commit, exported from Git objects.
All four fixed roles are built from that same source, on Linux/amd64. Receipts
come only from this invocation's bounded builds and inspections. Labels alone
and historical cache receipts are never admission evidence.
No registry publication, credentials, arbitrary build arguments or public proof
dictionaries are accepted. Raw subprocess output stays private.
"""
from __future__ import annotations

from contextlib import contextmanager
from dataclasses import dataclass
import hashlib
import datetime
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
import record_resolver
import catalog

proof, common = runtime.proof, runtime.common
_READY_FOR_EXECUTION = True
_TOTAL = 4500  # Leaves300s inside the canonical80-minute workflow job.
_BUILD = 3600
_EXPERIMENT_MIN = 720
_CLEANUP = 180
_MAX_CONTEXT = 64 * runtime.MIB
_WEB_RECIPE = 'docker/release/web.Dockerfile'
_WEB_RECIPE_SHA = 'bc081fef7a7b90f268b672dd9da10e3b36146e32faab59d1659838847ec806fa'
_IGNORE_SHA = 'b77c48c1111912668497b028767ac88974d905cfbeb3d669847020d87cd879c6'
_PLUGIN_SHA = 'e56147d0d19281573362a9a0ec89a20eacdce4b8d2b25402167299fcdfa1fc67'
_LINUX_PLUGIN_SHA = '48af8a397ebd60178778bf63611dbcebe5f5e7a9be90eb9147b24b9587455778'
_BUILDKIT = 'docker.io/moby/buildkit:buildx-stable-1@sha256:040d34121c27906c4ff9ac152a30d52bf2c5d328d3bb748916bb3d2743c02528'
_BUILDER_METADATA_FLAGS = ['--debug','--allow-insecure-entitlement=network.host']
_SOURCE_URL = 'https://github.com/' + common.REPOSITORY
_RECIPES = {
    'graph': ('docker/staging/graph-stub.Dockerfile',stack.GRAPH_RECIPE_SHA256),
    'controller': ('release/compat/controller.Dockerfile','5dc4fb16e685d6569ac184431a50743617320bef47ca638b15912ef0b04ef812'),
    'fixture': ('release/compat/fixture.Dockerfile','947cf1674e35d8e6f77f1da9c7b1dd45b282bff34e77600e52225cfba95f7895'),
    'candidate': (_WEB_RECIPE,_WEB_RECIPE_SHA),
}
_GRAPH_IGNORE_SHA = '1fec754b49fe8ab21d076cd9b26f97e33f83ca8584a99a344176869caadbd060'
_SEAL = object()


def need(value, code='image-admission-invalid'):
    if not value:
        raise proof.EvidenceError(code)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


# Wording categories only, never root-cause or compatibility conclusions.
# Match private bounded bytes; publish no matching line, token, path or offset.
_BUILD_WORDING = (
    ('compiler', (b'undefined:', b'syntax error:', b'cannot use ', b'compilation terminated.', b'error ts')),
    ('resource', (b'no space left on device', b'cannot allocate memory', b'fatal error: out of memory')),
    ('signal', (b'signal: killed', b'terminated by signal', b'killed by signal')),
    ('network', (b'no such host', b'dial tcp', b'temporary failure in name resolution', b'connection refused',
                 b'tls handshake timeout', b'i/o timeout', b'network is unreachable', b'certificate signed by unknown authority')),
    ('registry', (b'toomanyrequests', b'pull rate limit', b'unauthorized: authentication required',
                  b'denied: requested access', b'pull access denied', b'failed to authorize')),
    ('package', (b'npm err!', b'npm error', b'e: unable to locate package', b'e: failed to fetch',
                 b'e: package ', b'sub-process /usr/bin/dpkg returned an error code')),
    ('buildkit', (b'failed to solve:', b'failed to export', b'failed to connect to buildkit', b'failed to dial grpc')),
    ('context', (b'failed to calculate checksum', b'failed to compute cache key', b'failed to read dockerfile',
                 b'failed to read build context')),
)
_BUILD_FAMILIES = frozenset(name for name, _ in _BUILD_WORDING)


class _BuildFailure(proof.EvidenceError):
    """Diagnostic carrier for one completed nonzero build, never an admission.

    Only primitive bounded facts survive capture. No raw bytes, Capture,
    environment, command or paths are retained by this exception.
    """
    __slots__ = ('_snapshot', '_cleanup_complete')

    def __init__(self, role, value):
        super().__init__('admission-build-failed')
        self._snapshot = None
        self._cleanup_complete = None
        if (type(role) is not str or role not in ('graph', 'controller', 'fixture', 'candidate')
                or type(value) is not runtime.Capture or type(value.returncode) is not int
                or not -2147483648 <= value.returncode <= 4294967295 or value.returncode == 0
                or type(value.stdout) is not bytes or type(value.stderr) is not bytes
                or len(value.stdout) > 16 * runtime.MIB or len(value.stderr) > 16 * runtime.MIB):
            return
        families = set()
        for raw in (value.stdout, value.stderr):
            lowered = raw.lower()
            for name, phrases in _BUILD_WORDING:
                if any(phrase in lowered for phrase in phrases):
                    families.add(name)
        self._snapshot = (role, value.returncode, len(value.stdout), sha(value.stdout),
                          len(value.stderr), sha(value.stderr), tuple(sorted(families)) or ('unknown',))

    def _finish_cleanup(self, value):
        if type(self) is _BuildFailure:
            _BuildFailure._cleanup_complete.__set__(self, value if type(value) is bool else None)

    def _public(self):
        # Exact type and slot access avoid custom attributes/properties. A
        # tampered carrier produces no projection, preserving generic refusal.
        if type(self) is not _BuildFailure:
            return None
        try:
            data = _BuildFailure._snapshot.__get__(self)
            cleanup = _BuildFailure._cleanup_complete.__get__(self)
        except AttributeError:
            return None
        if type(data) is not tuple or len(data) != 7 or cleanup is not None and type(cleanup) is not bool:
            return None
        role, code, out_n, out_sha, err_n, err_sha, families = data
        if (type(role) is not str or role not in ('graph', 'controller', 'fixture', 'candidate')
                or type(code) is not int or not -2147483648 <= code <= 4294967295 or code == 0
                or any(type(n) is not int or not 0 <= n <= 16 * runtime.MIB for n in (out_n, err_n))
                or any(type(s) is not str or re.fullmatch('[0-9a-f]{64}', s) is None for s in (out_sha, err_sha))
                or type(families) is not tuple or not 1 <= len(families) <= len(_BUILD_FAMILIES)
                or any(type(name) is not str or name not in _BUILD_FAMILIES | {'unknown'} for name in families)):
            return None
        if families != tuple(sorted(set(families))) or 'unknown' in families and families != ('unknown',):
            return None
        return {'version': 1, 'role': role, 'exit_code': code, 'stdout_bytes': out_n, 'stdout_sha256': out_sha,
                'stderr_bytes': err_n, 'stderr_sha256': err_sha, 'families': list(families),
                'family_scope': 'matched-wording-only', 'cleanup_complete': cleanup}


_PROGRESS_STAGES = frozenset(('source-resolution', 'source-inputs', 'engine-check', 'pull',
    'pull-check', 'builder-start', 'builder-create', 'builder-bootstrap', 'builder-check',
    'role-precheck', 'role-export', 'role-build', 'role-verify', 'role-complete',
    'final-checks', 'builder-cleanup'))
_PROGRESS_ROLES = ('graph', 'controller', 'fixture', 'candidate')


class _BuildProgress:
    """Diagnostic-only primitives; no clock reads, authority or retained streams."""
    __slots__ = ('started', 'active', 'failure', 'completed', 'cleanup', 'resolution_end',
                 'role_started', 'role_times')

    def __init__(self, sampled):
        self.started = sampled
        self.active = ('source-resolution', 'none', sampled, None, None)
        self.failure = None
        self.completed = ()
        self.cleanup = None
        self.resolution_end = None
        self.role_started = None
        self.role_times = ()

    @staticmethod
    def _state(deadline):
        if type(deadline) is not _Deadline:
            return None
        state = vars(deadline).get('_build_progress')
        if type(state) is not _BuildProgress:
            return None
        return state

    @staticmethod
    def _number(value):
        return type(value) in (int, float) and math.isfinite(value) and value >= 0

    @staticmethod
    def _label(stage, role):
        if type(stage) is not str or stage not in _PROGRESS_STAGES or type(role) is not str:
            return False
        if stage.startswith('role-'):
            return role in _PROGRESS_ROLES
        if stage in ('pull', 'pull-check'):
            return role in ('previous', 'live', 'postgres', 'redis')
        return role == 'none'

    @staticmethod
    def mark(deadline, stage, role='none'):
        state = _BuildProgress._state(deadline)
        if state is None or not _BuildProgress._label(stage, role):
            return
        sampled = vars(deadline).get('last')
        if (not _BuildProgress._number(sampled) or not _BuildProgress._number(state.started)
                or not 0 <= sampled - state.started <= _TOTAL + _CLEANUP + 5):
            return
        state.active = (stage, role, sampled, None, None)
        elapsed = sampled - state.started
        if stage == 'source-inputs' and state.resolution_end is None:
            state.resolution_end = elapsed
        if stage == 'role-build':
            state.role_started = (role, elapsed)
        if (stage == 'role-complete' and type(state.completed) is tuple
                and all(type(name) is str for name in state.completed)
                and state.completed == _PROGRESS_ROLES[:len(state.completed)]):
            if len(state.completed) < 4 and role == _PROGRESS_ROLES[len(state.completed)]:
                state.completed += (role,)
                if (type(state.role_started) is tuple and len(state.role_started) == 2
                        and state.role_started[0] == role and _BuildProgress._number(state.role_started[1])
                        and state.role_started[1] <= elapsed and type(state.role_times) is tuple
                        and len(state.role_times) < 4):
                    state.role_times += ((role, state.role_started[1], elapsed),)

    @staticmethod
    def capture(deadline, seconds):
        state = _BuildProgress._state(deadline)
        if state is None or not _BuildProgress._number(seconds) or not 0 < seconds <= _BUILD:
            return
        active = state.active
        if type(active) is tuple and len(active) == 5:
            state.active = (*active[:3], seconds, deadline.last)

    @staticmethod
    def failed(deadline):
        state = _BuildProgress._state(deadline)
        if state is None or state.failure is not None:
            return
        active = state.active
        if type(active) is not tuple or len(active) != 5:
            return
        stage, role, entered, cap, capture_start = active
        sampled, end = deadline.last, deadline.end
        if (not _BuildProgress._label(stage, role)
                or not all(_BuildProgress._number(v) for v in
                           (state.started, entered, cap, capture_start, sampled, end))
                or not state.started <= entered <= capture_start <= sampled
                or not 0 < cap <= _BUILD or sampled - state.started > _TOTAL + _CLEANUP + 5
                or type(state.completed) is not tuple
                or not all(type(name) is str for name in state.completed)
                or state.completed != _PROGRESS_ROLES[:len(state.completed)]):
            return
        state.failure = (stage, role, sampled - state.started, sampled - entered,
                         sampled - capture_start, cap, max(0, end - sampled),
                         max(0, end - _EXPERIMENT_MIN - _CLEANUP - sampled), state.completed,
                         state.resolution_end, state.role_times)

    @staticmethod
    def finish_cleanup(deadline, complete):
        state = _BuildProgress._state(deadline)
        if state is not None:
            state.cleanup = complete if type(complete) is bool else None

    @staticmethod
    def public(deadline):
        state = _BuildProgress._state(deadline)
        if state is None:
            return None
        value, cleanup = state.failure, state.cleanup
        if (type(value) is not tuple or len(value) != 11
                or cleanup is not None and type(cleanup) is not bool):
            return None
        (stage, role, elapsed, stage_elapsed, capture_elapsed, cap, remaining, build_remaining,
         completed, resolution_end, role_times) = value
        if (not _BuildProgress._label(stage, role)
                or not all(_BuildProgress._number(v) and v <= _TOTAL + _CLEANUP + 5
                           for v in (elapsed, stage_elapsed, capture_elapsed, cap, remaining, build_remaining))
                or not capture_elapsed <= stage_elapsed <= elapsed or not 0 < cap <= _BUILD
                or build_remaining > remaining or type(completed) is not tuple
                or not all(type(name) is str for name in completed)
                or completed != _PROGRESS_ROLES[:len(completed)]
                or resolution_end is not None and (not _BuildProgress._number(resolution_end) or resolution_end > elapsed)
                or type(role_times) is not tuple or len(role_times) != len(completed)):
            return None
        for name, row in zip(completed, role_times):
            if (type(row) is not tuple or len(row) != 3 or type(row[0]) is not str or row[0] != name
                    or not all(_BuildProgress._number(v) for v in row[1:])
                    or not 0 <= row[1] <= row[2] <= elapsed):
                return None
        return {'version': 1, 'stage': stage, 'role': role,
                'elapsed_seconds': round(elapsed, 3), 'stage_elapsed_seconds': round(stage_elapsed, 3),
                'capture_elapsed_seconds': round(capture_elapsed, 3), 'capture_cap_seconds': round(cap, 3),
                'remaining_seconds': round(remaining, 3), 'build_remaining_seconds': round(build_remaining, 3),
                'completed_roles': list(completed), 'cleanup_complete': cleanup,
                'pre_admission_end_seconds': None if resolution_end is None else round(resolution_end, 3),
                'role_boundaries': [{'role': row[0], 'build_start_seconds': round(row[1], 3),
                                     'verified_end_seconds': round(row[2], 3)} for row in role_times],
                'scope': 'last-sampled-clock; diagnostic-only; capture-incomplete'}


class _Deadline:
    """One private clock for verification, build, experiment AND cleanup."""
    def __init__(self, *, clock=time.monotonic):
        self.clock = clock
        self.last = None
        self.end = self.now() + _TOTAL
        self.build_end = None
        self._build_progress = _BuildProgress(self.last)

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
        self.remaining()
        if self.build_end is None:
            self.build_end=min(self.now()+_BUILD,self.end-_EXPERIMENT_MIN-_CLEANUP)
        left=min(self.build_end,self.end-_EXPERIMENT_MIN-_CLEANUP)-self.now()
        if left <= 0:
            raise proof.InfrastructureFailure('admission-build-reserve')
        return min(_BUILD, left)

    def build_capture(self,argv,**options):
        options['timeout']=min(options['timeout'],self.build_seconds())
        result=self.capture(argv,**options)
        self.build_seconds()
        return result

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


def _role_inputs(blobs):
    need(sha(blobs.get('.dockerignore',b''))==_IGNORE_SHA,'admission-ignore-unsupported')
    for role,(recipe,digest) in _RECIPES.items():
        need(sha(blobs.get(recipe,b''))==digest,'admission-recipe-unsupported')
        if role!='graph': need(recipe+'.dockerignore' not in blobs,'admission-ignore-override')
    graph_recipe=_RECIPES['graph'][0]
    need(sha(blobs.get(graph_recipe+'.dockerignore',b''))==_GRAPH_IGNORE_SHA,'admission-ignore-unsupported')
    graph=sorted([name for name in blobs if name.startswith('release/staging/graphstub/')
                  and name.endswith('.go') and not name.endswith('_test.go')]+[
                  'go.mod','go.sum',graph_recipe,graph_recipe+'.dockerignore'])
    need(len(graph)==16 and all(name in blobs for name in graph),'admission-graph-inputs')
    controller=['release/compat/controller.Dockerfile','release/compat/controller.py',
                'release/compat/sweep.py','release/compat/proof.py']
    need(all(name in blobs for name in controller),'admission-controller-inputs')
    return {'graph':{name:blobs[name] for name in graph},
            'controller':{name:blobs[name] for name in controller},
            'fixture':dict(blobs),'candidate':dict(blobs)}


@dataclass(frozen=True,repr=False)
class _Admitted:
    seal: object
    previous: processes.ImageBinding
    candidate: processes.ImageBinding
    controller: processes.ImageBinding
    graph: stack.GraphImage
    fixture: fixture.ExportImage
    receipt: dict


@dataclass(frozen=True, slots=True, repr=False)
class _AdmittedRollback:
    seal: object
    target: processes.ImageBinding
    live: processes.ImageBinding
    catalog_control: processes.ImageBinding
    controller: processes.ImageBinding
    graph: stack.GraphImage
    fixture: fixture.ExportImage
    receipt: dict


class _DockerPhase:
    """Private fixed-factory transport; no inherited authentication or cwd."""
    def __init__(self,binary,config,work,deadline):
        self.binary,self.config,self.work,self.deadline=binary,config,work,deadline
        self.cleanup_end=None

    def capture(self,argv,**options):
        need(argv[:5]==[self.binary,'--config',str(self.config),'--context','default'],
             'admission-docker-prefix')
        runtime._empty_directory(self.config)
        left=(self.deadline.build_seconds() if self.cleanup_end is None else
              min(self.deadline.remaining(),self.cleanup_end-self.deadline.now()))
        need(left>0,'admission-builder-cleanup-deadline')
        value=_build_capture(argv,_build_environment(self.config,self.work),
                             min(options['timeout'],left),self.deadline,cwd=str(self.work))
        need(type(value) is runtime.Capture and type(value.returncode) is int
             and type(value.stdout) is bytes and type(value.stderr) is bytes
             and len(value.stdout)<=options['stdout_limit'] and len(value.stderr)<=options['stderr_limit'],
             'admission-docker-capture')
        if self.cleanup_end is None:self.deadline.build_seconds()
        else:need(self.deadline.now()<self.cleanup_end,'admission-builder-cleanup-deadline')
        return value


def _inspect(binary, config, reference, deadline):
    value=deadline.capture([binary,'--config',str(config),'--context','default','image','inspect',reference],
        env=common.scrubbed_env(),timeout=30,stdout_limit=runtime.MIB,stderr_limit=runtime.MIB)
    need(value.returncode==0 and not value.stderr,'admission-image-unavailable')
    return stack._json(value.stdout)


def _role_inspect(binary,config,deadline,role,image,source):
    recipe=_RECIPES[role][1]
    row=_inspect(binary,config,image,deadline)
    if role in ('controller','candidate'):
        binding=processes.ImageBinding(image,image,source,recipe)
        processes.OwnedProcesses._validate_image(None,role,binding,row)
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
             'admission-role-image-invalid')
    labels=row['Config'].get('Labels') or {}
    need(labels.get(stack.RECIPE_LABEL)==recipe and labels.get(stack.ROLE_LABEL)==role,
         'admission-role-label-invalid')
    return row


def _build_command(plugin,context,work,candidate,role,builder):
    need(role in _RECIPES and (builder=='default' or re.fullmatch(r'compat-[0-9a-f]{32}',builder)))
    recipe,recipe_sha=_RECIPES[role]
    labels={stack.SOURCE_LABEL:candidate,stack.RECIPE_LABEL:recipe_sha,
            stack.ROLE_LABEL:role,'org.opencontainers.image.source':_SOURCE_URL}
    if role=='candidate':labels['org.opencontainers.image.title']='rereply-release-web'
    args=[str(plugin),'build','--builder',builder,'--platform','linux/amd64','--file',str(context/recipe),
          '--load','--pull=false','--provenance=false','--sbom=false','--progress=plain',
          '--iidfile',str(work/'image.id'),'--metadata-file',str(work/'build-metadata.json')]
    arguments=({'TARGETOS':'linux','TARGETARCH':'amd64'} if role in ('graph','candidate') else
               {'SOURCE_SHA':candidate,'RECIPE_SHA256':recipe_sha})
    for key,value in sorted(arguments.items()):args+=['--build-arg',key+'='+value]
    for key,value in sorted(labels.items()):args+=['--label',key+'='+value]
    return args+[str(context)]


def _build_environment(config, work):
    # Docker's empty config still auto-discovers credential helpers. Bind both
    # Windows search locations, not merely the config, and never inherit PATH.
    # Linux uses only the owned work directory; Docker/Buildx argv are absolute,
    # so host-installed helpers are unnecessary and must not be discoverable.
    if _host_linux():
        search=[work]
    else:
        import ctypes
        from ctypes import wintypes
        kernel=ctypes.WinDLL('kernel32',use_last_error=True)
        kernel.GetSystemDirectoryW.argtypes=[wintypes.LPWSTR,wintypes.UINT]
        kernel.GetSystemDirectoryW.restype=wintypes.UINT
        buffer=ctypes.create_unicode_buffer(32768)
        size=kernel.GetSystemDirectoryW(buffer,len(buffer))
        need(0<size<len(buffer),'admission-system-directory')
        search=[Path(buffer.value)]
    need(all(p.is_absolute() and p.is_dir() for p in search) and work.is_absolute() and work.is_dir(),
         'admission-build-search-path')
    for directory in (*search,work):
        need(not any(p.name.lower().startswith('docker-credential-') for p in directory.iterdir()),
             'admission-credential-helper-available')
    # No inherited Docker/Git/Buildx/proxy/auth overrides. Go's current-directory
    # lookup is confined to the fresh attempt directory passed as Popen.cwd.
    result=({} if _host_linux() else {key:os.environ[key] for key in ('SystemRoot','WINDIR','TEMP','TMP') if key in os.environ})
    result.update(PATH=(':'.join(map(str,search)) if _host_linux() else str(search[0])),
                  DOCKER_CONFIG=str(config),
                  DOCKER_CONTEXT='default',BUILDX_GIT_INFO='false',BUILDX_GIT_LABELS='false')
    if not _host_linux(): result['PATHEXT']='.COM;.EXE;.BAT;.CMD'
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


def _build_capture(argv, env, seconds, deadline, *, cwd=None):
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
    _BuildProgress.capture(deadline, seconds)
    child=job=None; pipes=()
    try:
        command=argv
        if os.name=='nt':
            job=_WindowsJob()
            command=[sys.executable,'-I','-S','-B','-c',_WINDOWS_LAUNCHER,job.name,json.dumps(argv)]
        try:
            # The module-owned fixed build argv is the sole caller in admission;
            # local tests substitute harmless Python children at this private boundary.
            cwd=str(Path(argv[argv.index('--iidfile')+1]).parent) if '--iidfile' in argv else cwd
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
    except BaseException:
        _BuildProgress.failed(deadline)
        raise
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
        if unresolved:
            _BuildProgress.failed(deadline)
            raise proof.InfrastructureFailure('admission-build-teardown-unresolved')


def _save(path,value):
    raw=(json.dumps(value,sort_keys=True,indent=2)+'\n').encode()
    with path.open('xb') as stream: stream.write(raw)


def _host_linux():
    return sys.platform.startswith('linux')


def _builder_plugin(binary, supplied=None):
    if supplied is not None:
        need(isinstance(supplied,Path),'admission-builder-location')
        plugin=supplied
    else:
        need(os.name=='nt' and Path(binary).is_absolute() and len(Path(binary).parents)>=4,
             'admission-builder-location')
        plugin=Path(binary).parents[3]/'cli-plugins/docker-buildx.exe'
    need(_host_linux() or os.name=='nt','admission-platform-unsupported')
    need(sha(_regular(plugin,128*runtime.MIB))==(_LINUX_PLUGIN_SHA if _host_linux() else _PLUGIN_SHA),
         'admission-builder-unproven')
    if _host_linux(): need(os.access(plugin,os.X_OK) and plugin.stat().st_mode&0o022==0,'admission-builder-mode')
    return plugin


class _LinuxBuilder:
    """One internally created pinned backend; never discovers/adopts a builder.

    Explicit volume/container creation avoids Buildx's implicit GPU probe and
    name-conflict adoption. The pinned docker-container driver is then bound to
    this exact verified container. No remote driver or caller flags are exposed.
    Ambiguous creation never grants cleanup authority over an unbound resource.
    """
    def __init__(self,binary,config,plugin,build_config,work,source,deadline):
        self.binary,self.config,self.plugin=binary,config,plugin
        self.build_config,self.work,self.deadline=build_config,work,deadline
        self.token=uuid.uuid4().hex;self.name='compat-'+self.token;self.node=self.name+'0'
        self.container='buildx_buildkit_'+self.node;self.volume=self.container+'_state'
        self.labels={stack.LABEL:self.token,stack.ROLE_LABEL:'buildkit',stack.SOURCE_LABEL:source}
        self.identifier=None;self.container_row=None;self.volume_row=None;self.image=None
        self.volume_attempted=self.container_attempted=False
        self.closed=False;self.events=[];self.cleanup_end=None
        self.transport=_DockerPhase(binary,config,work,deadline)

    def _call(self,args,*,timeout=120):
        remaining=(self.deadline.build_seconds() if self.cleanup_end is None else
                   min(self.deadline.remaining(),self.cleanup_end-self.deadline.now()))
        need(remaining>0,'admission-builder-cleanup-deadline')
        self.transport.cleanup_end=self.cleanup_end
        value=self.transport.capture([self.binary,'--config',str(self.config),'--context','default',*args],
            env=common.scrubbed_env(base={}),timeout=min(timeout,remaining),stdout_limit=4*runtime.MIB,stderr_limit=runtime.MIB)
        need(type(value) is runtime.Capture and type(value.returncode) is int,'admission-builder-capture')
        if value.returncode!=0: raise proof.InfrastructureFailure('admission-builder-command')
        return value.stdout

    def _absence(self):
        containers=self._call(['container','ls','--all','--no-trunc','--filter','name=^/'+self.container+'$',
                               '--format','{{json .ID}}'])
        volumes=self._call(['volume','ls','--filter','name=^'+self.volume+'$','--format','{{json .Name}}'])
        need(containers==b'' and volumes==b'','admission-builder-already-exists')

    def _stamp(self,value):
        need(type(value) is str and value.endswith('Z'),'admission-builder-created')
        try: parsed=datetime.datetime.fromisoformat(value[:-1]+'+00:00')
        except ValueError: raise proof.EvidenceError('admission-builder-created') from None
        need(self.started<=parsed<=datetime.datetime.now(datetime.timezone.utc),'admission-builder-created')

    def _volume(self):
        row=stack._json(self._call(['volume','inspect',self.volume]))
        need(row.get('Name')==self.volume and row.get('Driver')=='local' and row.get('Scope')=='local'
             and row.get('Labels')==self.labels and not row.get('Options'),'admission-builder-volume')
        self._stamp(row.get('CreatedAt'))
        if self.volume_row is not None: need(row==self.volume_row,'admission-builder-volume-changed')
        return row

    def _container(self,*,running=None):
        need(type(self.identifier) is str and stack._ID.fullmatch(self.identifier),'admission-builder-id')
        row=stack._json(self._call(['container','inspect',self.identifier]))
        cfg,hc,state=row.get('Config',{}),row.get('HostConfig',{}),row.get('State',{})
        need(row.get('Id')==self.identifier and row.get('Name')=='/'+self.container
             and row.get('Image')==self.image['Id'] and cfg.get('Image')==_BUILDKIT
             and cfg.get('Labels')=={**(self.image['Config'].get('Labels') or {}),**self.labels}
             and cfg.get('Entrypoint')==self.image['Config'].get('Entrypoint') and cfg.get('Cmd')==['--debug']
             and cfg.get('Env')==self.image['Config'].get('Env')
             and cfg.get('User','')==self.image['Config'].get('User','')
             and cfg.get('WorkingDir','')==self.image['Config'].get('WorkingDir','')
             and cfg.get('Healthcheck')==self.image['Config'].get('Healthcheck')
             and not cfg.get('Tty') and not cfg.get('OpenStdin')
             and hc.get('Privileged') is True and hc.get('Init') is True and hc.get('NetworkMode')=='bridge'
             and hc.get('RestartPolicy')=={'Name':'no','MaximumRetryCount':0}
             and not hc.get('Binds') and not hc.get('PortBindings') and not hc.get('Devices')
             and not hc.get('DeviceRequests') and not hc.get('VolumesFrom')
             and not hc.get('PidMode') and not hc.get('UTSMode') and not hc.get('UsernsMode')
             and not hc.get('ExtraHosts') and not hc.get('Links')
             and ('OomKillDisable' not in hc or hc['OomKillDisable'] is None or hc['OomKillDisable'] is False)
             and row.get('RestartCount',0)==0,'admission-builder-container')
        mounts=row.get('Mounts')
        need(type(mounts) is list and len(mounts)==1 and mounts[0].get('Type')=='volume'
             and mounts[0].get('Name')==self.volume and mounts[0].get('Destination')=='/var/lib/buildkit'
             and mounts[0].get('RW') is True,'admission-builder-mount')
        self._stamp(row.get('Created'))
        if running is not None:need(state.get('Running') is running,'admission-builder-state')
        if self.container_row is not None:
            for key in ('Id','Created','Image','Name','Config','HostConfig','Mounts'):
                current,prior=row.get(key),self.container_row.get(key)
                if key=='HostConfig':
                    current,prior=dict(current),dict(prior)
                    # Docker 28 defaults false at create, then clears it at
                    # start when the kernel lacks OomKillDisable support.
                    # Keep absent keys and every other inspect field exact.
                    for snapshot in (current,prior):
                        if 'OomKillDisable' in snapshot:
                            value=snapshot['OomKillDisable']
                            need(value is None or value is False,'admission-builder-container')
                            snapshot['OomKillDisable']=None
                need(current==prior,'admission-builder-container-changed')
            if self.container_row['State'].get('Running'):
                need(state.get('StartedAt')==self.container_row['State'].get('StartedAt'),'admission-builder-restarted')
        return row

    def check(self):
        need(not self.closed and self.container_row is not None,'admission-builder-unbound')
        self._volume();self._container(running=True)
        self._metadata()

    def _metadata(self):
        row=common.loads_strict(_regular(self.build_config/'buildx/instances'/self.name,runtime.MIB))
        need(type(row) is dict and set(row)=={'Name','Driver','Nodes','Dynamic'}
             and row.get('Name')==self.name and row.get('Driver')=='docker-container' and row.get('Dynamic') is False,
             'admission-builder-metadata')
        nodes=row.get('Nodes')
        # Pinned Buildx serializes Flags and automatically appends this driver
        # entitlement. The precreated daemon itself has only ['--debug']; we
        # never let Buildx's create branch apply its additional default flags.
        need(type(nodes) is list and len(nodes)==1 and type(nodes[0]) is dict
             and set(nodes[0])=={'Name','Endpoint','Platforms','DriverOpts','Flags','Files'}
             and nodes[0].get('Name')==self.node
             and nodes[0].get('Endpoint')=='unix:///var/run/docker.sock'
             and nodes[0].get('Flags')==_BUILDER_METADATA_FLAGS and not nodes[0].get('Platforms')
             and nodes[0].get('DriverOpts')=={'image':_BUILDKIT,'restart-policy':'no','provenance-add-gha':'false'}
             and not nodes[0].get('Files'),'admission-builder-node')

    def _plugin(self,args):
        env=_build_environment(self.build_config,self.work)
        value=_build_capture([str(self.plugin),*args],env,min(180,self.deadline.build_seconds()),
                             self.deadline,cwd=str(self.work))
        need(type(value) is runtime.Capture and type(value.returncode) is int and value.returncode==0,
             'admission-builder-bootstrap')
        self.deadline.build_seconds()
        return value.stdout

    def _mutation(self,args,expected):
        event={'operation':args[0]+'-'+args[1],'outcome':'unknown'};self.events.append(event)
        raw=self._call(args)
        need(raw==expected.encode()+b'\n','admission-builder-mutation-unbound')
        event['outcome']='acknowledged'

    def start(self):
        self._absence()
        # A digest-only anonymous pull, not the Buildx driver's fallback pull.
        self._call(['image','pull','--platform','linux/amd64',_BUILDKIT],timeout=180)
        image=stack._json(self._call(['image','inspect',_BUILDKIT]))
        digest=_BUILDKIT.split('@')[1]
        need(image.get('Os')=='linux' and image.get('Architecture')=='amd64'
             and stack._IMAGE.fullmatch(image.get('Id',''))
             and any(ref in ('moby/buildkit@'+digest,'docker.io/moby/buildkit@'+digest) for ref in image.get('RepoDigests',[]))
             and type(image.get('Config')) is dict and image['Config'].get('Entrypoint')
             and not image['Config'].get('OnBuild'),'admission-builder-image')
        self.image=image;self.started=datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)
        self.volume_attempted=True
        args=['volume','create','--driver','local']
        for key,value in sorted(self.labels.items()):args+=['--label',key+'='+value]
        self._mutation(args+[self.volume],self.volume);self.volume_row=self._volume()
        self.container_attempted=True
        args=['container','create','--name',self.container,'--pull=never','--privileged','--init','--restart','no',
              '--network','bridge','--mount','type=volume,source='+self.volume+',target=/var/lib/buildkit']
        for key,value in sorted(self.labels.items()):args+=['--label',key+'='+value]
        event={'operation':'container-create','outcome':'unknown'};self.events.append(event)
        raw=self._call(args+[_BUILDKIT,'--debug'])
        need(re.fullmatch(rb'[0-9a-f]{64}\n',raw),'admission-builder-mutation-unbound')
        self.identifier=raw[:-1].decode();event['outcome']='acknowledged'
        self.container_row=self._container(running=False)
        _BuildProgress.mark(self.deadline, 'builder-create')
        self._plugin(['create','--name',self.name,'--node',self.node,'--driver','docker-container',
            '--driver-opt','image='+_BUILDKIT,'--driver-opt','restart-policy=no',
            '--driver-opt','provenance-add-gha=false','--buildkitd-flags=--debug','default'])
        self._metadata();self._volume();self._container(running=False)
        _BuildProgress.mark(self.deadline, 'builder-bootstrap')
        self._plugin(['inspect','--bootstrap',self.name])
        # Do not trust inspect's textual success: its source can print node errors
        # and return zero. Actual owned container state is authoritative here.
        _BuildProgress.mark(self.deadline, 'builder-check')
        self.container_row=self._container(running=True);self.check()

    def close(self):
        if self.closed:return
        self.cleanup_end=min(self.deadline.end,self.deadline.now()+90)
        if self.container_attempted and self.container_row is None:
            raise proof.InfrastructureFailure('admission-builder-ownership-unresolved')
        if self.volume_attempted and self.volume_row is None:
            raise proof.InfrastructureFailure('admission-builder-ownership-unresolved')
        if self.container_row is not None:
            row=self._container();self._volume()
            if row['State'].get('Running'):
                self._mutation(['container','stop','--timeout','10',self.identifier],self.identifier)
            self._container(running=False)
            self._mutation(['container','rm',self.identifier],self.identifier)
            need(self._call(['container','ls','--all','--no-trunc','--filter','id='+self.identifier,
                             '--format','{{json .ID}}'])==b'','admission-builder-removal-unproven')
        if self.volume_row is not None:
            self._volume();self._mutation(['volume','rm',self.volume],self.volume)
        self._absence();self.closed=True

    def summary(self):
        return {'kind':'owned-pinned-linux-buildkit','closed':self.closed,
                'builder':self.name,'node':self.node,'container_name':self.container,
                'acknowledged_container_id':self.identifier,'volume_name':self.volume,
                'labels':dict(self.labels),'pinned_image':_BUILDKIT,
                'container_id_sha256':sha((self.identifier or '').encode()),
                'volume_name_sha256':sha(self.volume.encode()),'events':self.events,
                'ownership_bound':self.container_row is not None and self.volume_row is not None}


@dataclass(frozen=True, slots=True)
class _ResolvedPrevious:
    digest: str
    source_sha: str
    recipe_sha256: str


@dataclass(frozen=True, slots=True)
class _ResolvedRollback:
    producer_sha: str
    target: _ResolvedPrevious
    live: _ResolvedPrevious
    target_release: str
    live_release: str


def _resolved_rollback(resolution):
    """Cross-check authenticated resolver data; typed values are not authority.

    The private caller must obtain the resolution from the concrete source
    operation. The shared catalog validator checks the complete projection;
    these additional checks bind each logical signed image and captured recipe.
    No recipe is reread from Git after that operation has closed.
    """
    contract = catalog.CatalogContract.from_rollback_resolution(resolution)
    r = record_resolver
    resolved = resolution.resolved
    bindings = resolved.image_bindings
    need(type(bindings) is tuple and len(bindings) == len(resolved.chain.entries)
         and all(type(row) is r.transport.VerifiedImages for row in bindings),
         'admission-resolution-image')
    endpoints = []
    for selected, recipe, source, reference in (
            (resolved.selected, resolution.target_recipe, contract.previous_sha, contract.previous_web),
            (resolved.latest, resolution.live_recipe, contract.candidate_sha, contract.candidate_web)):
        binding = bindings[selected.index]
        for rows, pattern in ((binding.images, common.DIGEST_RE), (binding.source_shas, common.SHA1_RE)):
            need(type(rows) is tuple and len(rows) == len(common.COMPONENTS)
                 and all(type(row) is tuple and len(row) == 2 and type(row[0]) is str
                         and type(row[1]) is str and pattern.fullmatch(row[1]) for row in rows)
                 and tuple(row[0] for row in rows) == common.COMPONENTS, 'admission-resolution-image')
        need(binding.release == selected.release and binding.record_sha == selected.record_sha
             and binding.images == selected.images
             and dict(binding.source_shas)['web'] == selected.actual_web_source_sha == source
             and common.IMAGE_REPOSITORY['web']+'@'+dict(binding.images)['web'] == reference,
             'admission-resolution-image')
        need(type(recipe) is r.WebRecipe and recipe.path == _WEB_RECIPE and recipe.source_sha == source
             and type(recipe.raw) is bytes and 0 < len(recipe.raw) <= r.policy.MAX_SOURCE_FILE_BYTES
             and type(recipe.sha256) is str and common.SHA256_RE.fullmatch(recipe.sha256)
             and sha(recipe.raw) == recipe.sha256, 'admission-resolution-recipe')
        endpoints.append(_ResolvedPrevious(dict(binding.images)['web'], source, recipe.sha256))
    target, live = endpoints
    need(target.digest != live.digest or target == live, 'admission-rollback-image-conflict')
    return _ResolvedRollback(contract.catalog_control_sha, target, live,
                             resolved.selected.release, resolved.latest.release)


def _admit_rollback_resolved(repo,resolution,binary,config,image_work_dir,deadline,*,buildx_binary=None):
    """Private endpoint admission; no public rollback or wire activation.

    All four local builds retain producer H. The two signed endpoint images
    retain their actual source identities, and never become local build claims.
    """
    need(type(deadline) is _Deadline, 'admission-resolution-deadline')
    deadline.remaining()
    rollback = _resolved_rollback(resolution)
    deadline.remaining()
    return _admit_core(repo,None,rollback.producer_sha,binary,config,image_work_dir,deadline,
                       buildx_binary=buildx_binary,rollback=rollback)


def _resolved_previous(resolution):
    """Check cross-bindings, not cryptography or deployment authorization.

    The future operational caller must obtain this exact projection internally
    from resolve_forward. Constructing its dataclasses cannot authenticate it.
    Catalog measurement/policy remain separate from image-source admission.
    """
    r = record_resolver
    need(type(resolution) is r.ForwardResolution, 'admission-resolution-invalid')
    candidate, material = resolution.candidate, resolution.catalog_material
    need(type(candidate) is r.policy.SourceFacts and candidate.mode == 'reviewed-v0'
         and type(candidate.sha) is str and common.SHA1_RE.fullmatch(candidate.sha)
         and type(material) is r.CatalogMaterial and material.candidate_sha == candidate.sha,
         'admission-resolution-source')
    try:
        r.policy._source(candidate)
    except r.policy.PolicyRefusal:
        raise proof.EvidenceError('admission-resolution-source') from None
    pins = dict(r.policy.V0_PINS)
    blobs = material.candidate_blobs
    need(type(blobs) is tuple and len(blobs) == len(pins)
         and all(type(row) is r.policy.SourceBlob and type(row.raw) is bytes
                 and 0 < len(row.raw) <= r.policy.MAX_SOURCE_FILE_BYTES for row in blobs)
         and tuple(row.path for row in blobs) == tuple(sorted(pins))
         and sum(len(row.raw) for row in blobs) <= r.policy.MAX_SOURCE_BYTES,
         'admission-resolution-material')
    artifacts = dict(candidate.artifacts)
    need(all(sha(row.raw) == pins[row.path] == artifacts[row.path] for row in blobs),
         'admission-resolution-material')
    resolved = resolution.resolved
    need(type(resolved) is r.ResolvedChain and type(resolved.chain) is r.policy.PolicyChain,
         'admission-resolution-selection')
    chain, selected = resolved.chain, resolved.selected
    need(chain.runtime_source_supported is True and chain.baseline is None
         and type(chain.entries) is tuple and 1 <= len(chain.entries) <= 501
         and all(type(row) is r.policy.PolicyEntry and type(row.record) is r.policy.RecordInput
                 for row in chain.entries)
         and type(selected) is r.ReleaseSelection and type(selected.index) is int
         and 0 <= selected.index < len(chain.entries)
         and type(resolved.latest) is r.ReleaseSelection
         and resolved.latest == r._selection(chain, len(chain.entries)-1)
         and selected == r._selection(chain, selected.index), 'admission-resolution-selection')
    need(type(resolved.image_bindings) is tuple and len(resolved.image_bindings) == len(chain.entries)
         and all(type(row) is r.transport.VerifiedImages for row in resolved.image_bindings),
         'admission-resolution-image')
    binding = resolved.image_bindings[selected.index]
    images, sources = binding.images, binding.source_shas
    for rows, pattern in ((images, common.DIGEST_RE), (sources, common.SHA1_RE)):
        need(type(rows) is tuple and len(rows) == len(common.COMPONENTS)
             and all(type(row) is tuple and len(row) == 2 and type(row[0]) is str
                     and type(row[1]) is str and pattern.fullmatch(row[1]) for row in rows)
             and tuple(row[0] for row in rows) == common.COMPONENTS,
             'admission-resolution-image')
    need(binding.release == selected.release and binding.record_sha == selected.record_sha
         and images == selected.images and dict(sources)['web'] == selected.actual_web_source_sha
         and resolution.previous_web_source_sha == selected.actual_web_source_sha,
         'admission-resolution-image')
    recipe = resolution.previous_recipe
    need(type(recipe) is r.WebRecipe and recipe.path == _WEB_RECIPE
         and recipe.source_sha == selected.actual_web_source_sha
         and type(recipe.raw) is bytes and 0 < len(recipe.raw) <= r.policy.MAX_SOURCE_FILE_BYTES
         and type(recipe.sha256) is str and common.SHA256_RE.fullmatch(recipe.sha256)
         and sha(recipe.raw) == recipe.sha256, 'admission-resolution-recipe')
    return _ResolvedPrevious(dict(images)['web'], selected.actual_web_source_sha, recipe.sha256)


def _admit_resolved(repo,resolution,binary,config,image_work_dir,deadline,*,buildx_binary=None):
    """Preparatory path; no caller switch or authentication from typed data.

    Reuse the caller's original deadline and source-built ownership lifecycle.
    Previous recipe bytes came from the closed source operation; no Git read
    or legacy verifier bridge recovers them after that operation has closed.
    """
    need(type(deadline) is _Deadline, 'admission-resolution-deadline')
    deadline.remaining()
    previous = _resolved_previous(resolution)
    deadline.remaining()
    return _admit_core(repo,None,resolution.candidate.sha,binary,config,image_work_dir,deadline,
                       buildx_binary=buildx_binary,resolved_previous=previous)


def _admit(repo,previous,candidate,binary,config,image_work_dir,deadline,*,buildx_binary=None):
    """Active legacy entrypoint; retain its original validation and I/O order."""
    return _admit_core(repo,previous,candidate,binary,config,image_work_dir,deadline,buildx_binary=buildx_binary)


def _admit_core(repo,previous,candidate,binary,config,image_work_dir,deadline,*,buildx_binary=None,
                resolved_previous=None,rollback=None):
    """Build four fixed roles once from one immutable, clean source selection.

    A location is not evidence: all build receipts originate here. Persistent
    attempts are never resumed, even when the last daemon outcome is unknown.
    Linux driver composition still needs its first real hosted validation.
    """
    if rollback is not None:
        need(type(rollback) is _ResolvedRollback and previous is None and resolved_previous is None
             and candidate == rollback.producer_sha
             and type(rollback.target) is _ResolvedPrevious and type(rollback.live) is _ResolvedPrevious,
             'admission-rollback-input')
        need(rollback.target.digest != rollback.live.digest or rollback.target == rollback.live,
             'admission-rollback-image-conflict')
        resolved_previous=rollback.target
    _BuildProgress.mark(deadline, 'source-inputs')
    source=_Source(repo,candidate,deadline)
    blobs=source.blobs(source.inventory(candidate));contexts=_role_inputs(blobs)
    digest=previous.images.get('web') if resolved_previous is None else resolved_previous.digest
    need(type(digest) is str and common.DIGEST_RE.fullmatch(digest),'admission-previous-invalid')
    need(Path(binary).is_absolute() and Path(binary).is_file(),'admission-docker-location')
    reference=common.IMAGE_REPOSITORY['web']+'@'+digest
    runtime._empty_directory(config);runtime._empty_directory(image_work_dir)
    plugin=_builder_plugin(binary,buildx_binary)
    plugin_sha=_LINUX_PLUGIN_SHA if _host_linux() else _PLUGIN_SHA
    work=image_work_dir/('candidate-'+candidate)
    need(not work.exists(),'admission-already-attempted')
    work.mkdir();build_config=work/'build-config';build_config.mkdir()
    config_value={'cliPluginsExtraDirs':[str(plugin.parent)]}
    _save(build_config/'config.json',config_value)
    transport=_DockerPhase(binary,config,work,deadline)
    docker=runtime.Docker(binary,config,capture=transport.capture)
    receipt={'source':candidate,'tree':source.tree,'roles':{},'release_attestation':False,
             'registry_publication':False,'scope':'same-source four-role build; hosted composition not yet proven',
             'source_mode':'Git inputs regular100644; host directory export, not Git executable-mode emulation',
             'plugin_sha256':plugin_sha,'build_phase_cap_seconds':_BUILD,'overall_cap_seconds':_TOTAL,
             'cleanup_complete':False}
    if rollback is not None:
        receipt.update(purpose='rollback-v0',signed_endpoints={
            role:{'release':release,'reference':common.IMAGE_REPOSITORY['web']+'@'+endpoint.digest,
                  'source':endpoint.source_sha,'recipe_sha256':endpoint.recipe_sha256}
            for role,release,endpoint in (('target',rollback.target_release,rollback.target),
                                         ('live',rollback.live_release,rollback.live))})
    _save(work/'intent.json',receipt)
    backend=None;error=None;bindings={};rows={};engine=None;build_failure=None

    def engine_info():
        value=transport.capture([binary,'--config',str(config),'--context','default','info','--format','{{json .}}'],
            timeout=30,stdout_limit=runtime.MIB,stderr_limit=runtime.MIB)
        need(value.returncode==0 and not value.stderr,'admission-engine-unavailable')
        result=common.loads_strict(value.stdout)
        need(result.get('OSType')=='linux' and result.get('Architecture') in ('x86_64','amd64')
             and type(result.get('ID')) is str and result['ID'],'admission-engine-invalid')
        return result

    def recheck():
        source.clean()
        need(source.tree==source.git(['rev-parse',candidate+'^{tree}'],1024).decode().strip(),
             'admission-source-changed')
        need(sha(_regular(plugin,128*runtime.MIB))==plugin_sha,'admission-builder-changed')
        need(common.loads_strict(_regular(build_config/'config.json',4096))==config_value,
             'admission-build-config-changed')
        runtime._empty_directory(config)
        if backend is not None:backend.check()

    try:
        deadline.build_seconds()  # Includes all immutable pulls, backend setup and all four builds.
        _BuildProgress.mark(deadline, 'engine-check')
        engine=engine_info()
        infrastructure={}
        pull_images=(reference,runtime.POSTGRES,runtime.REDIS)
        if rollback is not None:
            live_reference=common.IMAGE_REPOSITORY['web']+'@'+rollback.live.digest
            pull_images=tuple(dict.fromkeys((reference,live_reference,runtime.POSTGRES,runtime.REDIS)))
        for image in pull_images:
            pull_role = ('previous' if image == reference else 'postgres' if image == runtime.POSTGRES
                         else 'redis' if image == runtime.REDIS else 'live')
            _BuildProgress.mark(deadline, 'pull', pull_role)
            docker.pull(image)
            _BuildProgress.mark(deadline, 'pull-check', pull_role)
            infrastructure[image]=docker.inspect(image)
        previous_row=infrastructure[reference]
        if resolved_previous is None:
            previous_recipe=sha(source.git(['cat-file','blob',previous.sha+':'+_WEB_RECIPE]))
            previous_source=previous.sha
        else:
            previous_recipe=resolved_previous.recipe_sha256
            previous_source=resolved_previous.source_sha
        previous_binding=processes.ImageBinding(reference,previous_row['Id'],previous_source,previous_recipe)
        processes.OwnedProcesses._validate_image(None,'previous',previous_binding,previous_row)
        if rollback is not None:
            live_row=infrastructure[live_reference]
            live_binding=processes.ImageBinding(live_reference,live_row['Id'],rollback.live.source_sha,
                                                rollback.live.recipe_sha256)
            processes.OwnedProcesses._validate_image(None,'candidate',live_binding,live_row)
        if _host_linux():
            backend=_LinuxBuilder(binary,config,plugin,build_config,work,candidate,deadline)
            _BuildProgress.mark(deadline, 'builder-start')
            backend.start()
        for role in ('graph','controller','fixture','candidate'):
            _BuildProgress.mark(deadline, 'role-precheck', role)
            recheck()
            role_work=work/role;role_work.mkdir();context=role_work/'context'
            _BuildProgress.mark(deadline, 'role-export', role)
            _export(context,contexts[role])
            manifest=[{'path':name,'mode':'100644','bytes':len(raw),'sha256':sha(raw),
                       'git_blob':hashlib.sha1(b'blob '+str(len(raw)).encode()+b'\0'+raw).hexdigest()}
                      for name,raw in sorted(contexts[role].items())]
            _save(role_work/'source.json',{'source':candidate,'tree':source.tree,'files':manifest})
            seconds=deadline.build_seconds()
            role_receipt={'source':candidate,'role':role,'recipe_sha256':_RECIPES[role][1],
                          'source_manifest_sha256':sha((role_work/'source.json').read_bytes()),
                          'build_seconds_cap':seconds,'build_invocations':1,'outcome':'unknown'}
            receipt['roles'][role]=role_receipt
            _save(role_work/'intent.json',role_receipt)
            build_env=_build_environment(build_config,role_work)
            command=_build_command(plugin,context,role_work,candidate,role,backend.name if backend else 'default')
            # Last identity check immediately before Buildx. Missing/replaced
            # Linux resources must never enter the driver's implicit create path.
            _BuildProgress.mark(deadline, 'role-precheck', role)
            recheck()
            _BuildProgress.mark(deadline, 'role-build', role)
            value=_build_capture(command,build_env,deadline.build_seconds(),deadline)
            need(type(value) is runtime.Capture and type(value.returncode) is int
                 and type(value.stdout) is bytes and type(value.stderr) is bytes,'admission-build-capture')
            role_receipt.update(exit_code=value.returncode,stdout_bytes=len(value.stdout),stderr_bytes=len(value.stderr),
                                stdout_sha256=sha(value.stdout),stderr_sha256=sha(value.stderr),outcome='completed')
            if value.returncode != 0:
                build_failure=_BuildFailure(role,value)
                raise build_failure
            _BuildProgress.mark(deadline, 'role-verify', role)
            deadline.build_seconds();_check_export(context,contexts[role]);recheck()
            need(_build_environment(build_config,role_work)==build_env,'admission-build-environment-changed')
            iid=_regular(role_work/'image.id',128).decode('ascii','strict').strip()
            need(stack._IMAGE.fullmatch(iid),'admission-iid-invalid')
            metadata=common.loads_strict(_regular(role_work/'build-metadata.json',runtime.MIB))
            need(type(metadata) is dict,'admission-iid-unbound')
            # Pinned Buildx getImageID prefers the config key when present;
            # containerd's prefer-image-digest exporter deliberately omits it.
            identity_key=('containerimage.config.digest' if 'containerimage.config.digest' in metadata
                          else 'containerimage.digest')
            need(metadata.get(identity_key)==iid,'admission-iid-unbound')
            rows[role]=_role_inspect(binary,config,transport,role,iid,candidate)
            bindings[role]=iid
            role_receipt.update(image_id=iid,metadata_sha256=sha((role_work/'build-metadata.json').read_bytes()))
            _save(role_work/'result.json',role_receipt)
            _BuildProgress.mark(deadline, 'role-complete', role)
        _BuildProgress.mark(deadline, 'final-checks')
        recheck()
        for role,iid in bindings.items():
            _check_export(work/role/'context',contexts[role])
            need(_role_inspect(binary,config,transport,role,iid,candidate)==rows[role],'admission-built-image-changed')
        for image,row in infrastructure.items():
            need(docker.inspect(image)==row,'admission-infrastructure-image-changed')
        final_engine=engine_info()
        need(all(final_engine.get(key)==engine.get(key) for key in ('ID','OSType','Architecture','ServerVersion')),
             'admission-engine-changed')
        deadline.build_seconds()
    except BaseException as caught:
        error=caught
    finally:
        if backend is not None:
            _BuildProgress.mark(deadline, 'builder-cleanup')
            try:backend.close()
            except BaseException as caught:
                if error is not None and caught is not error:caught.__cause__=error
                error=caught
            receipt['builder']=backend.summary()
        receipt['cleanup_complete']=backend is None or backend.closed
        _BuildProgress.finish_cleanup(deadline, receipt['cleanup_complete'])
        if build_failure is not None:
            _BuildFailure._finish_cleanup(build_failure,receipt['cleanup_complete'])
    try:
        if error is not None:raise error
        need(receipt['cleanup_complete'],'admission-builder-cleanup-unresolved')
        deadline.experiment_seconds();source.clean();runtime._empty_directory(config)
        receipt.update(status='BUILT_AND_ADMITTED_UNATTESTED',remaining_seconds=deadline.remaining(),
                       engine_id_sha256=sha(engine['ID'].encode()))
        _save(work/'result.json',receipt)
        if rollback is not None:
            return _AdmittedRollback(_SEAL,previous_binding,live_binding,
                processes.ImageBinding(bindings['candidate'],bindings['candidate'],candidate,_RECIPES['candidate'][1]),
                processes.ImageBinding(bindings['controller'],bindings['controller'],candidate,_RECIPES['controller'][1]),
                stack.GraphImage(bindings['graph'],candidate),
                fixture.ExportImage(bindings['fixture'],candidate,_RECIPES['fixture'][1]),receipt)
        return _Admitted(_SEAL,previous_binding,
            processes.ImageBinding(bindings['candidate'],bindings['candidate'],candidate,_RECIPES['candidate'][1]),
            processes.ImageBinding(bindings['controller'],bindings['controller'],candidate,_RECIPES['controller'][1]),
            stack.GraphImage(bindings['graph'],candidate),
            fixture.ExportImage(bindings['fixture'],candidate,_RECIPES['fixture'][1]),receipt)
    except BaseException as caught:
        receipt['status']='REFUSED_NO_RETRY_OR_IMAGE_ADOPTION'
        receipt['failure_code']=('admission-build-unknown' if caught.args==('admission-build-unknown',)
                                 else 'admission-incomplete')
        if not (work/'result.json').exists():_save(work/'result.json',receipt)
        raise
