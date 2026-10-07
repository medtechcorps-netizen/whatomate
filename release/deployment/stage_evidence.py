"""Pure validation of public staging evidence before a production promote.

No staging credential, raw origin, private fixture or provider request is
needed here. The workflow supplies these canonical outputs from the same run.
"""

import base64

import ship_common as common
import stage_contract as contract
from stage_report import CHECKS


require = contract.require
CODE = "candidate-invalid:staging-evidence"
REPORT_KEYS = {"receipt_sha256", "run_id", "candidate_sha256", "origin_sha256", "checks", "passed"}


def decode(encoded, expected_hash, *, maximum):
    common.require_sha256(expected_hash, CODE)
    require(type(encoded) is str and 0 < len(encoded) <= maximum, CODE)
    try:
        raw = base64.b64decode(encoded, validate=True)
    except ValueError:
        common.fail(CODE)
    require(common.sha256_bytes(raw) == expected_hash, CODE)
    value = common.loads_strict(raw, code=CODE)
    require(raw == common.canonical_file_bytes(value), CODE)
    return value


def verify(env, *, control, candidate, pins, production):
    """Require all 13 checks for this run's exact stage deployment candidate."""
    contract.validate_pins(pins, production)
    require(env.get("SHIP_DRILL", "none") == "none", CODE)
    candidate_hash = common.require_sha256(env.get("CANDIDATE_SHA256"), CODE)
    require(common.sha256_bytes(common.canonical_file_bytes(candidate)) == candidate_hash, CODE)
    receipt_hash = common.require_sha256(env.get("STAGE_RECEIPT_SHA256"), CODE)
    receipt = contract.validate_receipt(decode(env.get("STAGE_RECEIPT_B64"), receipt_hash, maximum=16384))
    require(type(receipt["schema_version"]) is int, CODE)
    report = common.exact_keys(decode(env.get("STAGE_REPORT_B64"), env.get("STAGE_REPORT_SHA256"), maximum=16384), REPORT_KEYS, CODE)
    require(receipt["run_id"] == control["run_id"] == report["run_id"], CODE)
    require(receipt["candidate_sha256"] == candidate_hash == report["candidate_sha256"], CODE)
    require(receipt["candidate_source_sha"] == candidate["sha"] == control["sha"] and
            receipt["candidate_images"] == candidate["images"], CODE)
    require(receipt["app_id_sha256"] == pins["app_id_sha256"], CODE)
    require(receipt["ingress_sha256"] == report["origin_sha256"] and
            report["origin_sha256"] != production["default_ingress_sha256"], CODE)
    require(report["receipt_sha256"] == receipt_hash and receipt["drill"] == "none", CODE)
    require(type(report["passed"]) is int and report["passed"] == 13 and report["checks"] == list(CHECKS), CODE)
    common.sanitize_public(report)
    return report
