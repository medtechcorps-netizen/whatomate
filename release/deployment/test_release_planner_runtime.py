"""Execute the checked-in predecessor Bash step, never a hand-copied predicate.

GitHub/attestation transport and the separately unit-tested Python policy CLI are
strict local doubles. jq, archive checks, shell guards and output handoff are real.
This is a workflow rehearsal, not signed release evidence or a provider test.
"""
from __future__ import annotations

import base64
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
import zipfile


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/plan-production-rollout.yml"
CONTROL = "a" * 40
REPOSITORY = "synthetic-owner/synthetic-repository"
CANARY_PATH = ".github/workflows/verify-production-crm-canary.yml"


def predecessor_script() -> str:
    source = WORKFLOW.read_text(encoding="utf-8")
    matches = re.findall(
        r"(?ms)^      - name: Authenticate the exact latest signed predecessor phase state\n"
        r".*?^        run: \|\n(?P<body>.*?)(?=^      - name:)", source,
    )
    if len(matches) != 1:
        raise AssertionError("expected exactly one complete predecessor step")
    return textwrap.dedent(matches[0])


def literal_env(name: str) -> str:
    matches = re.findall(rf"(?m)^  {name}: (.+)$", WORKFLOW.read_text(encoding="utf-8"))
    if len(matches) != 1 or "${{" in matches[0]:
        raise AssertionError(f"missing literal workflow environment: {name}")
    return matches[0].strip('"')


MOCKS = r'''
set -euo pipefail
jq() { "$REHEARSAL_JQ" "$@"; }
# Git Bash cannot implement POSIX chmod on every Windows temporary directory.
# Assert the production mode/path, but leave permission enforcement to Linux CI.
if [[ "$REHEARSAL_WINDOWS" == true ]]; then
  mkdir() {
    [[ $# -eq 3 && "$1" == -m && "$2" == 0700 && "$3" == "$RUNNER_TEMP/predecessor" ]]
    command mkdir "$3"
  }
fi
# The extracted step can only call these enumerated synthetic GETs; no ambient
# token, home/config, provider CLI or real gh binary is made available.
gh() {
  printf '%s\n' "$*" >> "$RUNNER_TEMP/calls.log"
  if [[ "$1" == api ]]; then
    local endpoint="$2" file
    shift 2
    case "$endpoint" in
      "/repos/$REPOSITORY/actions/workflows/verify-production-crm-canary.yml") file=workflow.json ;;
      "/repos/$REPOSITORY/actions/workflows/77/runs?branch=main&event=workflow_dispatch&status=success&per_page=100") file=runs.json ;;
      "/repos/$REPOSITORY/actions/runs/101")
        file=latest.json
        if [[ $# -eq 2 && "$1" == --jq && "$2" == .run_attempt ]]; then file=latest-recheck.json; fi ;;
      "/repos/$REPOSITORY/actions/runs/101/attempts/1") file=exact.json ;;
      "/repos/$REPOSITORY/actions/runs/101/attempts/1/jobs?per_page=100") file=jobs.json ;;
      "/repos/$REPOSITORY/actions/runs/101/artifacts?per_page=100") file=artifacts.json ;;
      "/repos/$REPOSITORY/actions/artifacts/201/zip")
        [[ $# -eq 0 ]]; cat "$RUNNER_TEMP/input.zip"; return ;;
      *) printf 'unmocked API endpoint\n' >&2; return 96 ;;
    esac
    if [[ $# -eq 0 ]]; then cat "$RUNNER_TEMP/$file"
    elif [[ $# -eq 2 && "$1" == --jq ]]; then jq -r "$2" "$RUNNER_TEMP/$file"
    else printf 'unexpected API method/options\n' >&2; return 96; fi
  elif [[ "$1" == attestation && "$2" == verify ]]; then
    [[ "$3" == "$RUNNER_TEMP/predecessor/production-phase-state.json" ]]
    shift 3
    [[ "$1" == --repo && "$2" == "$RELEASE_REPOSITORY" ]]; shift 2
    [[ "$1" == --signer-workflow && "$2" == "$RELEASE_REPOSITORY/$PRODUCTION_PHASE_STATE_WORKFLOW_PATH" ]]; shift 2
    [[ "$1" == --signer-digest && "$2" == "$CONTROL_SHA" ]]; shift 2
    [[ "$1" == --source-digest && "$2" == "$CONTROL_SHA" ]]; shift 2
    [[ "$1" == --source-ref && "$2" == refs/heads/main ]]; shift 2
    [[ "$1" == --deny-self-hosted-runners ]]; shift
    [[ "$1" == --format && "$2" == json ]]; shift 2
    [[ $# -eq 2 && "$1" == --predicate-type ]]
    [[ "$AUTH_TRANSPORT_OK" == true ]] || return 95
    case "$2" in
      https://slsa.dev/provenance/v1) cat "$RUNNER_TEMP/provenance.json" ;;
      "$PRODUCTION_PHASE_STATE_PREDICATE") cat "$RUNNER_TEMP/policy.json" ;;
      *) return 95 ;;
    esac
  else printf 'unmocked gh command\n' >&2; return 96; fi
}
/usr/bin/python3() {
  printf 'policy-cli\n' >> "$RUNNER_TEMP/calls.log"
  [[ "$1" == -I && "$2" == -S && "$3" == -B ]]
  [[ "$4" == "control/$PRODUCTION_PLAN_VERIFIER_PATH" && "$5" == validate-predecessor ]]
  [[ "$6" == --contract && "$7" == "control/$PRODUCTION_CONTRACT_PATH" ]]
  [[ "$8" == --policy && "$9" == "control/$PRODUCTION_RELEASE_POLICY_PATH" ]]
  [[ "${10}" == --schema && "${11}" == "control/$PRODUCTION_CHANGE_SCHEMA_PATH" ]]
  [[ "${12}" == --normalized-input && "${13}" == "$RUNNER_TEMP/normalized-input.json" ]]
  [[ "${14}" == --rollout-plan && "${15}" == "$RUNNER_TEMP/capsule/rollout-plan.json" ]]
  [[ "${16}" == --control-sha && "${17}" == "$CONTROL_SHA" ]]
  [[ "${18}" == --predecessor-state && "${19}" == "$RUNNER_TEMP/predecessor/production-phase-state.json" ]]
  [[ "${20}" == --predecessor-sha256 && "${21}" == "$RUNNER_TEMP/predecessor/production-phase-state.sha256" ]]
  # Record the REAL argv; the harness submits it to the current Python parser.
  printf '%s\0' "${@:5}" > "$RUNNER_TEMP/policy-argv.bin"
  [[ $# -eq 23 && "${22}" == --target-images-output && "${23}" == "$RUNNER_TEMP/predecessor-target-images.json" ]]
  [[ "$POLICY_CLI_OK" == true ]]
}
curl() { return 97; }; wget() { return 97; }; git() { return 97; }
doctl() { return 97; }; docker() { return 97; }; node() { return 97; }
'''


class PlannerWorkflowRuntimeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.jq = os.environ.get("RELEASE_TEST_JQ") or shutil.which("jq")
        cls.bash = os.environ.get("RELEASE_TEST_BASH") or shutil.which("bash")
        if not cls.jq or not cls.bash:
            raise RuntimeError("workflow rehearsal requires RELEASE_TEST_JQ/jq and RELEASE_TEST_BASH/bash")
        version = subprocess.run([cls.jq, "--version"], capture_output=True, text=True, check=True).stdout.strip()
        if version != "jq-1.8.2":
            raise RuntimeError(f"workflow rehearsal requires jq-1.8.2, got {version}")
        cls.script = predecessor_script()

    def fixture(self, phase="baseline", genesis=False):
        state = {"phase": phase, "control_sha": CONTROL, "schema_version": 1}
        raw = (json.dumps(state, sort_keys=True, separators=(",", ":")) + "\n").encode()
        state_hash = hashlib.sha256(raw).hexdigest()
        run = dict(id=101, run_attempt=1, workflow_id=77,
                   repository={"full_name": REPOSITORY}, event="workflow_dispatch",
                   status="completed", conclusion="success", head_branch="main",
                   head_sha=CONTROL, path=CANARY_PATH,
                   name=literal_env("PRODUCTION_PHASE_STATE_WORKFLOW_NAME"))
        gate = dict(name=literal_env("PRODUCTION_PHASE_STATE_GATE_NAME"), run_attempt=1,
                    status="completed", conclusion="success", runner_name="Hosted Agent")
        artifact = dict(id=201, name="production-phase-state-101-1", expired=False,
                        workflow_run={"id": 101}, size_in_bytes=512)
        return dict(state=state, raw=raw, state_hash=state_hash, archive_entries=None,
                    latest=copy.deepcopy(run), exact=copy.deepcopy(run), latest_recheck=copy.deepcopy(run), jobs={"jobs": [gate]},
                    artifacts={"artifacts": [artifact]}, runs={"workflow_runs": [] if genesis else [run]},
                    provenance=[{"verificationResult": {}}],
                    policy=[{"verificationResult": {"statement": {"predicate": state}}}],
                    genesis=genesis, policy_ok=True, auth_ok=True, archive_tampered=False,
                    sidecar=state_hash + "\n")

    def execute(self, data, script=None):
        archive = io.BytesIO()
        entries = data["archive_entries"] or {
            "production-phase-state.json": data["raw"],
            "production-phase-state.sha256": data["sidecar"].encode(),
        }
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_STORED) as bundle:
            for name, content in entries.items():
                bundle.writestr(name, content)
        archive_bytes = archive.getvalue()
        archive_hash = "sha256:" + hashlib.sha256(archive_bytes).hexdigest()
        data["artifacts"]["artifacts"][0].setdefault("digest", archive_hash)
        # Repeated/incorrect metadata remain as supplied; only default the fixture digest.
        for artifact in data["artifacts"]["artifacts"][1:]:
            artifact.setdefault("digest", archive_hash)
        normalized = {"predecessor": None if data["genesis"] else dict(
            run_id="101", run_attempt=1, artifact_id="201",
            artifact_digest=archive_hash, state_sha256=data["state_hash"])}
        with tempfile.TemporaryDirectory(prefix="release-planner-rehearsal-") as name:
            temp = Path(name)
            for key in ("latest", "exact", "jobs", "artifacts", "runs", "provenance", "policy"):
                (temp / f"{key}.json").write_text(json.dumps(data[key]), encoding="utf-8")
            (temp / "latest-recheck.json").write_text(json.dumps(data["latest_recheck"]), encoding="utf-8")
            (temp / "workflow.json").write_text('{"id":77}', encoding="utf-8")
            (temp / "normalized-input.json").write_text(json.dumps(normalized), encoding="utf-8")
            (temp / "input.zip").write_bytes(archive_bytes + (b"tamper" if data["archive_tampered"] else b""))
            contract = temp / "control" / literal_env("PRODUCTION_CONTRACT_PATH")
            contract.parent.mkdir(parents=True)
            contract.write_text(json.dumps({"bootstrap_state": {"genesis_state_sha256": "b" * 64}}), encoding="utf-8")
            (temp / "capsule").mkdir()
            (temp / "capsule/rollout-plan.json").write_text("{}", encoding="utf-8")
            script_path = temp / "rehearse.sh"
            script_path.write_text(MOCKS + "\n" + (script or self.script), encoding="utf-8", newline="\n")
            env = {key: os.environ[key] for key in ("SYSTEMROOT", "WINDIR", "COMSPEC") if key in os.environ}
            # Deliberately omit inherited GH_TOKEN, provider credentials, BASH_ENV,
            # HOME/config and the ambient executable search path.
            bash_path = Path(self.bash).resolve()
            env.update(PATH=(bash_path.parent.parent / "usr/bin").as_posix() if os.name == "nt" else "/usr/bin:/bin",
                       HOME=temp.as_posix(), TEMP=temp.as_posix(), TMP=temp.as_posix(),
                       RUNNER_TEMP=temp.as_posix(), CONTROL_SHA=CONTROL,
                       REPOSITORY=REPOSITORY, RELEASE_REPOSITORY=REPOSITORY,
                       REHEARSAL_JQ=Path(self.jq).as_posix(),
                       REHEARSAL_WINDOWS=str(os.name == "nt").lower(),
                       POLICY_CLI_OK=str(data["policy_ok"]).lower(), AUTH_TRANSPORT_OK=str(data["auth_ok"]).lower())
            for key in ("PRODUCTION_PHASE_STATE_WORKFLOW_PATH", "PRODUCTION_PHASE_STATE_WORKFLOW_NAME",
                        "PRODUCTION_PHASE_STATE_GATE_NAME", "PRODUCTION_PHASE_STATE_PREDICATE",
                        "PRODUCTION_PLAN_VERIFIER_PATH", "PRODUCTION_CONTRACT_PATH",
                        "PRODUCTION_RELEASE_POLICY_PATH", "PRODUCTION_CHANGE_SCHEMA_PATH"):
                env[key] = literal_env(key)
            result = subprocess.run([self.bash, "--noprofile", "--norc", script_path.as_posix()],
                                    cwd=temp, env=env, capture_output=True, text=True, timeout=25)
            outputs = {p.name: p.read_text(encoding="utf-8") for p in temp.glob("predecessor-*") if p.is_file()}
            argv = temp / "policy-argv.bin"
            if argv.exists():
                # Parsing is real and independent of the transport mock's guards.
                # Policy semantics are exercised separately with complete fixtures.
                sys.path.insert(0, str(ROOT / "release/deployment"))
                try:
                    import verify_production_plan
                    arguments = argv.read_bytes().decode().rstrip("\0").split("\0")
                    with self.subTest(actual_policy_argv=arguments[0]):
                        verify_production_plan.parser().parse_args(arguments)
                finally:
                    sys.path.pop(0)
            return result, outputs, (temp / "calls.log").read_text(encoding="utf-8")

    def test_genesis_and_all_signed_predecessor_paths(self):
        for phase in ("genesis", "baseline", "bridge", "backend"):
            with self.subTest(phase=phase):
                data = self.fixture(phase, genesis=phase == "genesis")
                result, outputs, calls = self.execute(data)
                self.assertEqual(result.returncode, 0, result.stderr)
                if phase == "genesis":
                    self.assertEqual(outputs["predecessor-state.sha256"], "b" * 64 + "\n")
                    self.assertNotIn("attestation verify", calls)
                else:
                    self.assertEqual(outputs["predecessor-run-id.txt"], "101\n")
                    self.assertEqual(base64.b64decode(outputs["predecessor-state.b64"]), data["raw"])
                    self.assertEqual(calls.count("attestation verify"), 2)
                    self.assertIn("policy-cli", calls)

    def test_original_precedence_defect_is_reproduced(self):
        old = self.script.replace('([.jobs[] | select(.name == $gate)] | length) == 1 and',
                                  '[.jobs[] | select(.name == $gate)] | length == 1 and', 1)
        self.assertNotEqual(old, self.script)
        result, _, _ = self.execute(self.fixture(), old)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Cannot index array with string', result.stderr)

    def test_run_identity_drift_fails_before_download(self):
        mutations = dict(id=102, run_attempt=2, workflow_id=78, repository={"full_name": "wrong/repo"},
                         event="push", status="in_progress", conclusion="failure", head_branch="other",
                         head_sha="c" * 40, path=".github/workflows/other.yml", name="old workflow name")
        for target in ("latest", "exact"):
            for field, value in mutations.items():
                with self.subTest(target=target, field=field):
                    data = self.fixture(); data[target][field] = value
                    result, _, calls = self.execute(data)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn("/zip", calls)

    def test_gate_failures_stop_before_download(self):
        for mutation in ("missing", "duplicate", "attempt", "failed", "skipped", "runner"):
            with self.subTest(mutation=mutation):
                data = self.fixture(); gate = data["jobs"]["jobs"][0]
                if mutation == "missing": data["jobs"]["jobs"] = []
                elif mutation == "duplicate": data["jobs"]["jobs"].append(copy.deepcopy(gate))
                elif mutation == "attempt": gate["run_attempt"] = 2
                elif mutation == "runner": del gate["runner_name"]
                else: gate["conclusion"] = "skipped" if mutation == "skipped" else "failure"
                result, _, calls = self.execute(data)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("/zip", calls)

    def test_attempt_advancing_between_identity_and_gate_reads_is_rejected(self):
        data = self.fixture()
        data["latest_recheck"]["run_attempt"] = 2
        result, _, calls = self.execute(data)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("/jobs?", calls)
        self.assertNotIn("/zip", calls)

    def test_archive_policy_and_freshness_fail_closed(self):
        for mutation in ("expired", "wrong-digest", "zero-size", "wrong-run", "duplicate", "wrong-name",
                         "archive-tamper", "extra-file", "wrong-sidecar", "policy-cli", "auth", "empty-provenance",
                         "wrong-policy", "not-latest", "genesis-with-prior"):
            with self.subTest(mutation=mutation):
                data = self.fixture(genesis=mutation == "genesis-with-prior")
                artifact = data["artifacts"]["artifacts"][0]
                if mutation == "expired": artifact["expired"] = True
                elif mutation == "wrong-digest": artifact["digest"] = "sha256:" + "f" * 64
                elif mutation == "zero-size": artifact["size_in_bytes"] = 0
                elif mutation == "wrong-run": artifact["workflow_run"]["id"] = 102
                elif mutation == "duplicate": data["artifacts"]["artifacts"].append(copy.deepcopy(artifact))
                elif mutation == "wrong-name": artifact["name"] = "wrong"
                elif mutation == "archive-tamper": data["archive_tampered"] = True
                elif mutation == "extra-file": data["archive_entries"] = {"extra.txt": b"synthetic"}
                elif mutation == "wrong-sidecar": data["sidecar"] = "f" * 64 + "\n"
                elif mutation == "policy-cli": data["policy_ok"] = False
                elif mutation == "auth": data["auth_ok"] = False
                elif mutation == "empty-provenance": data["provenance"] = []
                elif mutation == "wrong-policy": data["policy"] = [{"verificationResult": {"statement": {"predicate": {}}}}]
                elif mutation == "not-latest": data["runs"]["workflow_runs"][0] = {**data["latest"], "id": 102}
                elif mutation == "genesis-with-prior": data["runs"]["workflow_runs"] = [data["latest"]]
                result, _, _ = self.execute(data)
                self.assertNotEqual(result.returncode, 0, mutation)


if __name__ == "__main__":
    unittest.main()
