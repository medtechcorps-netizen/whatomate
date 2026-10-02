#!/usr/bin/env python3
"""Verify the release-graduation convergence commit (step 2) offline.

The convergence commit makes main's product tree equal the production ui
release source, while every release control stays byte for byte main's.
Run it from a full-history checkout (fetch-depth: 0) against the commit
itself, which is the pull request head, not a synthetic merge ref or a
later main commit:

    python3 -B scripts/verify_main_convergence.py [--commit REV]

Standard library and git object reads only: no network, no writes to the
repository, no environment secrets.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys

MAIN = "0825df34bea71dcd1828f581461551694f3906e2"
SOURCE = "c482dbbc287ae29ea0f6fe11081d4cc16d8ca525"
# Publisher rule in .github/workflows/publish-attest-production-crm-canary-driver.yml
# ("Derive exact driver build-input version"), as computed on MAIN.
DRIVER_VERSION_SHA256 = "a60a0abe6f0ca5d299dc2706cd79ec62a140f2259a5fb34929fac85aad15c126"

THIS_FILE = "scripts/verify_main_convergence.py"
LINT_POLICY = ".golangci.yml"
ADDED_PATHS = (LINT_POLICY, THIS_FILE)
CANARY_DRIVER = "frontend/canary-driver"
# Kept from MAIN. The release images are built with the control checkout's
# docker/release/*.Dockerfile over the source tree as context, so docker/
# stays main's bytes, exactly the files production was built from.
CONTROL_PATHS = (
    ".github", "release", "docker", "docs", "prototype",
    ".gitattributes", ".gitignore", CANARY_DRIVER,
)
# Paths where MAIN and SOURCE differ and SOURCE wins. Every other path not in
# CONTROL_PATHS is identical on both sides, and the gate proves it.
PRODUCT_TREES = ("internal", "cmd", "pkg", "frontend/src", "frontend/e2e")
# Image and compile inputs that are identical on both sides; checked by id.
PRODUCT_BLOBS = (
    "go.mod", "go.sum", "frontend/package.json", "frontend/package-lock.json",
    "config.example.toml", ".dockerignore",
)
ALLOWED_TOP_LEVEL_CHANGES = {"cmd", "frontend", "internal", "pkg", LINT_POLICY, "scripts"}
ALLOWED_FRONTEND_CHANGES = {"src", "e2e"}
# Assembled at run time so this file never contains the literal substrings
# that the release-control scanners count in workflows and release modules.
FORBIDDEN_ADDED_SUBSTRINGS = ("group: " + "rereply-" + "production", "live" + "-evidence")
POLICY_STEP_OPEN = "cat > phase-validation.golangci.yml <<'POLICY'"
VALIDATION_WORKFLOW = ".github/workflows/validate-exact-release-source.yml"
IMAGE_WORKFLOW = ".github/workflows/build-attest-exact-release-images.yml"


class GateFailure(Exception):
    pass


def git(*args: str, binary: bool = False, check: bool = True, data: bytes | None = None):
    result = subprocess.run(["git", *args], input=data, capture_output=True, check=False)
    if not check:
        return result
    if result.returncode != 0:
        detail = result.stderr.decode(errors="replace").strip()
        raise GateFailure(f"git {' '.join(args[:2])} failed: {detail}")
    return result.stdout if binary else result.stdout.decode("utf-8")


def rev(spec: str) -> str:
    return git("rev-parse", "--verify", "--quiet", spec).strip()


def blob(commit: str, path: str) -> bytes:
    return git("cat-file", "blob", f"{commit}:{path}", binary=True)


def require(condition: bool, message: str) -> None:
    if not condition:
        raise GateFailure(message)


def excluding(paths: tuple[str, ...]) -> tuple[str, ...]:
    return (":(top)",) + tuple(f":(top,exclude){path}" for path in paths)


def diff_is_empty(left: str, right: str, pathspec: tuple[str, ...]) -> bool:
    result = git("diff", "--quiet", "--no-ext-diff", left, right, "--", *pathspec, check=False)
    if result.returncode not in (0, 1):
        raise GateFailure(f"git diff failed: {result.stderr.decode(errors='replace').strip()}")
    return result.returncode == 0


def tree_without(tree: str, name: str) -> str:
    """Hash a tree with one entry removed, without writing any object."""
    raw = git("cat-file", "tree", tree, binary=True)
    width = 32 if git("rev-parse", "--show-object-format").strip() == "sha256" else 20
    kept, offset, removed = [], 0, 0
    while offset < len(raw):
        nul = raw.index(b"\0", offset)
        entry_name = raw[offset:nul].split(b" ", 1)[1].decode("utf-8")
        end = nul + 1 + width
        if entry_name == name:
            removed += 1
        else:
            kept.append(raw[offset:end])
        offset = end
    require(removed == 1, f"{name} is not exactly one entry of tree {tree[:12]}")
    return git("hash-object", "-t", "tree", "--stdin", data=b"".join(kept)).strip()


def driver_version(commit: str) -> str:
    paths = {"docker/crm-canary-driver.Dockerfile", "frontend/package.json", "frontend/package-lock.json"}
    listing = git("ls-tree", "-r", "--name-only", commit, "--", CANARY_DRIVER)
    paths.update(line for line in listing.splitlines() if line)
    rows = []
    for path in sorted(paths, key=lambda item: item.encode("utf-8")):
        entry = git("ls-tree", commit, "--", path).rstrip("\n")
        require("\t" in entry, f"driver input is missing: {path}")
        meta, entry_path = entry.split("\t", 1)
        mode, kind, obj = meta.split(" ")
        require(entry_path == path and kind == "blob" and mode in ("100644", "100755"),
                f"unexpected driver input entry: {path}")
        digest = hashlib.sha256(git("cat-file", "blob", obj, binary=True)).hexdigest()
        rows.append(f"{mode}\t{obj}\t{digest}\t{path}\n")
    require(len(rows) >= 4, "too few driver inputs")
    return hashlib.sha256("".join(rows).encode("utf-8")).hexdigest()


def validation_lint_policy(commit: str) -> str:
    lines = blob(commit, VALIDATION_WORKFLOW).decode("utf-8").split("\n")
    starts = [index for index, line in enumerate(lines) if line.strip() == POLICY_STEP_OPEN]
    require(len(starts) == 1, "validation lint policy step not found exactly once")
    start = starts[0]
    ends = [index for index in range(start + 1, len(lines)) if lines[index].strip() == "POLICY"]
    require(bool(ends), "validation lint policy heredoc is not terminated")
    indent = len(lines[start]) - len(lines[start].lstrip(" "))
    body = []
    for line in lines[start + 1:ends[0]]:
        require(line.strip() == "" or line.startswith(" " * indent), "policy heredoc indentation differs")
        body.append(line[indent:] if line.strip() else "")
    return "\n".join(body) + "\n"


def check_parents(commit: str) -> str:
    parents = git("rev-list", "--parents", "-n", "1", commit).split()[1:]
    require(parents == [MAIN, SOURCE], f"parents are {parents}, expected [main, ui source]")
    return "parents == [main 0825df34, ui source c482dbbc]"


def check_source_is_the_pinned_ui_source(commit: str) -> str:
    ui = json.loads(blob(commit, "release/exact-sources.json"))["phases"]["ui"]
    require(ui["source_sha"] == SOURCE, "release/exact-sources.json ui source differs")
    require(rev(f"{SOURCE}^{{tree}}") == ui["root_tree"], "ui root tree differs from its pin")
    require(rev(f"{SOURCE}:frontend") == ui["frontend_tree"], "ui frontend tree differs from its pin")
    require(rev(f"{SOURCE}:internal") == ui["internal_tree"], "ui internal tree differs from its pin")
    return "ui source and its root/frontend/internal trees match release/exact-sources.json"


def check_product_equals_source(commit: str) -> str:
    require(diff_is_empty(SOURCE, commit, excluding(CONTROL_PATHS + ADDED_PATHS)),
            "a product path differs from the ui source")
    for path in PRODUCT_TREES + PRODUCT_BLOBS:
        require(rev(f"{commit}:{path}") == rev(f"{SOURCE}:{path}"), f"{path} differs from the ui source")
    internal = rev(f"{commit}:internal")
    reduced = tree_without(rev(f"{commit}:frontend"), "canary-driver")
    require(reduced == rev(f"{SOURCE}:frontend"), "frontend minus canary-driver differs from the ui source")
    return (f"every non-control path equals the ui source (internal {internal[:8]}, "
            f"frontend minus canary-driver {reduced[:8]})")


def check_controls_equal_main(commit: str) -> str:
    require(diff_is_empty(MAIN, commit, excluding(PRODUCT_TREES + ADDED_PATHS)),
            "a non-product path differs from main")
    for path in CONTROL_PATHS:
        require(rev(f"{commit}:{path}") == rev(f"{MAIN}:{path}"), f"{path} differs from main")
    top = set(git("diff-tree", "--name-only", MAIN, commit).split())
    require(top <= ALLOWED_TOP_LEVEL_CHANGES, f"unexpected top-level changes: {sorted(top - ALLOWED_TOP_LEVEL_CHANGES)}")
    frontend = set(git("diff-tree", "--name-only", f"{MAIN}:frontend", f"{commit}:frontend").split())
    require(frontend <= ALLOWED_FRONTEND_CHANGES, f"unexpected frontend changes: {sorted(frontend)}")
    scripts = git("ls-tree", "-r", "--name-only", commit, "--", "scripts").split()
    require(scripts in ([], [THIS_FILE]), "scripts/ holds more than this gate")
    return "every non-product path equals main (.github, release, docker, docs, prototype, canary-driver)"


def check_driver_inputs(commit: str) -> str:
    require(driver_version(MAIN) == DRIVER_VERSION_SHA256, "main's driver version differs from the recorded value")
    head_version = driver_version(commit)
    require(head_version == DRIVER_VERSION_SHA256, f"driver version moved to {head_version[:8]}")
    return f"canary driver build-input version unchanged ({head_version[:8]})"


def check_release_dockerfiles(commit: str) -> str:
    components = json.loads(blob(commit, "release/exact-sources.json"))["release"]["components"]
    workflow = blob(commit, IMAGE_WORKFLOW).decode("utf-8")
    require(sorted(components) == ["gmail-relay", "meta-relay", "web"], "release components differ")
    for component in components.values():
        path = component["dockerfile"]
        digest = hashlib.sha256(blob(commit, path)).hexdigest()
        require(digest == component["dockerfile_sha256"], f"{path} differs from its exact-sources pin")
        approved = re.findall(rf'(?m)^\s+"{re.escape(path)}": "([0-9a-f]{{64}})",$', workflow)
        require(approved == [digest], f"{path} differs from the image workflow allowlist")
    return "release Dockerfiles are the pinned bytes the production images were built with"


def check_lint_policy(commit: str) -> str:
    text = blob(commit, LINT_POLICY).decode("utf-8")
    require("\r" not in text, "lint policy is not LF")
    marker = text.find('\nversion: "2"\n')
    require(marker != -1, "lint policy body is missing")
    require(text[marker + 1:] == validation_lint_policy(commit),
            "root lint policy differs from the reviewed validation policy")
    return "root .golangci.yml mirrors the reviewed validation lint policy verbatim"


def check_no_forbidden_additions(commit: str) -> str:
    patch = git("diff", "--no-ext-diff", "--no-color", "--text", "-U0", MAIN, commit, binary=True)
    added = [line[1:] for line in patch.decode("utf-8", errors="replace").split("\n")
             if line.startswith("+") and not line.startswith("+++")]
    for needle in FORBIDDEN_ADDED_SUBSTRINGS:
        hits = sum(needle in line for line in added)
        require(hits == 0, f"{hits} added line(s) contain a forbidden release-control substring")
    return f"no forbidden release-control substring in {len(added)} added lines"


CHECKS = (
    check_parents,
    check_source_is_the_pinned_ui_source,
    check_product_equals_source,
    check_controls_equal_main,
    check_driver_inputs,
    check_release_dockerfiles,
    check_lint_policy,
    check_no_forbidden_additions,
)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--commit", default="HEAD", help="convergence commit to verify (default HEAD)")
    args = parser.parse_args()
    try:
        os.chdir(git("rev-parse", "--show-toplevel").strip())
        commit = rev(f"{args.commit}^{{commit}}")
        for sha in (MAIN, SOURCE):
            require(git("cat-file", "-t", sha, check=False).stdout.decode().strip() == "commit",
                    f"{sha[:12]} is missing; fetch full history")
    except GateFailure as error:
        print(f"FAIL setup: {error}")
        return 1
    failures = 0
    for check in CHECKS:
        name = check.__name__[len("check_"):]
        try:
            print(f"ok   {name}: {check(commit)}")
        except (GateFailure, KeyError, ValueError) as error:
            failures += 1
            print(f"FAIL {name}: {error}")
    print(f"{'PASS' if failures == 0 else 'FAIL'} {commit} ({len(CHECKS) - failures}/{len(CHECKS)} checks)")
    return 0 if failures == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
