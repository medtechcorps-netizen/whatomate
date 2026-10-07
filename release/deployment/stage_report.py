#!/usr/bin/env python3
"""Turn the private canary's exact 13 passes into a run/receipt-bound report."""
import argparse
import base64
import copy
import os
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ship_common as common
import stage_contract as contract
import stage_fixture as fixture

# This ordered inventory is compared against PR7 checks.ts by the tests.
CHECKS = (
    "klinik_whatsapp_outbound", "klinik_whatsapp_inbound",
    "omnichannel_outbound_realtime_without_reload", "omnichannel_inbound_realtime_without_reload",
    "navbar_unread_increment", "navbar_unread_clear", "omnichannel_conversation_switch_autoscroll",
    "omnichannel_late_layout_autoscroll", "native_chat_realtime_without_reload",
    "native_chat_conversation_switch_autoscroll", "native_chat_late_layout_autoscroll",
    "non_klinik_send_denied", "cross_organization_send_denied",
)
require = contract.require


def exact_passes(report):
    code = "input-invalid:stage-report"
    common.exact_keys(report, {"specs", "errors"}, code)
    require(report["errors"] == [] and type(report["specs"]) is list and len(report["specs"]) == 13, code)
    names = []
    for spec in report["specs"]:
        common.exact_keys(spec, {"title", "tests"}, code)
        require(type(spec["title"]) is str and spec["title"] in CHECKS, code)
        require(type(spec["tests"]) is list and len(spec["tests"]) == 1, code)
        test = common.exact_keys(spec["tests"][0], {"expectedStatus", "status", "results"}, code)
        require(test["expectedStatus"] == "passed" and test["status"] == "expected" and type(test["results"]) is list and len(test["results"]) == 1, code)
        result = common.exact_keys(test["results"][0], {"status", "retry"}, code)
        require(result["status"] == "passed" and type(result["retry"]) is int and result["retry"] == 0, code)
        names.append(spec["title"])
    require(set(names) == set(CHECKS), code)


def bound_report(report, saved, *, receipt_b64, receipt_sha256, run_id, candidate_sha256,
                 app_id_sha256, production_origin_sha256, drill="none"):
    # A deliberate drill can never yield a promotion-eligible passing report,
    # even if a bug or catch-all route causes every UI assertion to pass.
    require(drill == "none", "input-invalid:stage-report-drill")
    common.exact_keys(saved, {"schema_version", "origin_sha256", "canary"}, "input-invalid:stage-runtime-fixture")
    common.exact_keys(saved["canary"], fixture.CANARY_KEYS | {"stub_control_key"}, "input-invalid:stage-runtime-fixture")
    fixture.secret(saved["canary"]["stub_control_key"], minimum=32)
    exported = copy.deepcopy(saved)
    del exported["canary"]["stub_control_key"]
    fixture.validate_fixture(exported, production_origin_sha256)
    receipt = contract.decode_receipt(receipt_b64, receipt_sha256, run_id=run_id, candidate_sha256=candidate_sha256,
        origin=saved["canary"]["origin"], production_origin_sha256=production_origin_sha256, app_id_sha256=app_id_sha256)
    require(receipt["drill"] == "none" and receipt["ingress_sha256"] == saved["origin_sha256"], "input-invalid:stage-report-drill-or-origin")
    exact_passes(report)
    value = {"receipt_sha256": receipt_sha256, "run_id": run_id, "candidate_sha256": candidate_sha256,
        "origin_sha256": saved["origin_sha256"], "checks": list(CHECKS), "passed": 13}
    common.sanitize_public(value)
    return value


def main(argv=None, env=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--private-file", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args(argv)
    env = os.environ if env is None else env
    out = common.Output(stdout=sys.stdout, output_path=env.get("GITHUB_OUTPUT"))
    try:
        contract.validate_environment(env)
        require(not any(key.startswith("STAGING_") for key in env), "context-invalid:stage-report-secrets")
        sys.path.insert(0, str(contract.ROOT / "release/staging"))
        import setup
        state, report = setup.private_path(args.private_file), setup.private_path(args.report)
        require(state != report and state.parent == report.parent, "input-invalid:stage-report-path")
        for path in (state, report):
            setup.check_private_state(path, setup.Runner())
            require(path.is_file() and path.stat().st_size <= 256 * 1024, "input-invalid:stage-report-file")
        production = common.load_json(Path(__file__).with_name("ship-target.json"), "target-invalid:production")
        pins = common.load_json(Path(__file__).with_name("ship-target-staging.json"), "target-invalid:staging-pins")
        contract.validate_pins(pins, production)
        result = bound_report(common.loads_strict(report.read_bytes()), common.loads_strict(state.read_bytes()),
            receipt_b64=env.get("STAGE_RECEIPT_B64"), receipt_sha256=env.get("STAGE_RECEIPT_SHA256"),
            run_id=env.get("GITHUB_RUN_ID"), candidate_sha256=env.get("STAGE_CANDIDATE_SHA256"),
            app_id_sha256=pins["app_id_sha256"], production_origin_sha256=production["default_ingress_sha256"],
            drill=env.get("CANARY_DRILL", "none"))
        raw = common.canonical_file_bytes(result)
        out.set_outputs({"report_b64": base64.b64encode(raw).decode("ascii"), "report_sha256": common.sha256_bytes(raw), "passed": "true"})
        out.emit(result)
        return 0
    except Exception:
        out.text("stage-report: refused")
        return 1


if __name__ == "__main__": sys.exit(main())
