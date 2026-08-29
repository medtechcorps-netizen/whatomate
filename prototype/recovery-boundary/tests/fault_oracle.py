"""Fault-injecting test oracle only; never a runtime or durable ledger.

The in-memory model exists solely to force tests through issue-once and
zero/one/multiple reconciliation. It must not be imported by future runtime
code or cited as evidence of provider durability.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Dict, Optional, Set


SIDE_EFFECTS = (
    "authority-app-create",
    "authority-app-update",
    "authority-app-delete",
    "broker-app-create",
    "broker-app-update",
    "full-redeploy",
    "broker-app-delete",
    "trusted-source-add",
    "trusted-source-delete",
    "binding-install",
    "binding-remove",
    "credential-install",
    "credential-remove",
    "leaf-issue",
    "leaf-revoke",
    "capability-issue",
    "capability-revoke",
    "mtls-issue",
    "mtls-revoke",
    "wrapping-key-revoke",
    "marker-cas-v2",
    "fork-post",
    "evidence-publish",
    "cleanup-delete",
)


class OracleError(RuntimeError):
    pass


@dataclass
class EffectRecord:
    request_digest: str
    issue_count: int = 1
    state: str = "issued-ambiguous"
    observation_digest: Optional[str] = None
    observed: Optional[int] = None
    result: Optional[bytes] = None


@dataclass
class OperationRecord:
    generation: int
    body_digest: str
    authorizations: Set[str] = field(default_factory=set)
    effects: Dict[str, EffectRecord] = field(default_factory=dict)
    request_effects: Dict[str, str] = field(default_factory=dict)
    terminal_response: Optional[bytes] = None
    quarantined: bool = False


class FaultLedger:
    """Pure-memory oracle with explicit fresh-auth and ambiguity semantics."""

    def __init__(self) -> None:
        self._operations: Dict[str, OperationRecord] = {}
        self._consumed_authorizations: Set[str] = set()

    def _burn_authorization(self, authorization: str) -> None:
        if authorization in self._consumed_authorizations:
            raise OracleError("authorization replay")
        self._consumed_authorizations.add(authorization)

    @staticmethod
    def _require_authorization_separated(
        authorization: str, *bound_digests: str
    ) -> None:
        if authorization in bound_digests:
            raise OracleError(
                "authorization digest must differ from every bound digest"
            )

    @classmethod
    def _require_record_authorization_separated(
        cls, record: OperationRecord, authorization: str
    ) -> None:
        bound_digests = [record.body_digest]
        for effect in record.effects.values():
            bound_digests.append(effect.request_digest)
            if effect.observation_digest is not None:
                bound_digests.append(effect.observation_digest)
        if authorization in bound_digests:
            cls._quarantine(
                record,
                "authorization digest must differ from every bound digest",
            )

    @staticmethod
    def _require_identity(record: OperationRecord, generation: int, body_digest: str) -> None:
        if record.generation != generation or record.body_digest != body_digest:
            # A stable operation identifier presented with a different
            # canonical body is a durable terminal quarantine, not merely a
            # rejected request that may be retried with another envelope.
            record.quarantined = True
            raise OracleError("operation generation/body mismatch; operation quarantined")
        if record.quarantined:
            raise OracleError("operation quarantined")

    @staticmethod
    def _quarantine(record: OperationRecord, message: str) -> None:
        record.quarantined = True
        raise OracleError(f"{message}; operation quarantined")

    def authorize(
        self, operation: str, generation: int, body_digest: str, authorization: str
    ) -> Optional[bytes]:
        # This is deliberately ordered like the required durable authority:
        # consume the one-time authentication envelope before interpreting any
        # caller-controlled operation semantics.  A malformed body, digest
        # conflict, or other later failure therefore cannot make the JTI /
        # challenge reusable.
        self._burn_authorization(authorization)

        self._require_authorization_separated(authorization, body_digest)
        if generation < 1:
            raise OracleError("operation generation must be positive")

        record = self._operations.get(operation)
        if record is None:
            record = OperationRecord(generation=generation, body_digest=body_digest)
            self._operations[operation] = record
        else:
            self._require_identity(record, generation, body_digest)
            self._require_record_authorization_separated(record, authorization)

        record.authorizations.add(authorization)
        return record.terminal_response

    def status(
        self,
        operation: str,
        generation: int,
        body_digest: str,
        side_effect: str,
        effect_request_digest: str,
        authorization: str,
    ) -> tuple[str, Optional[bytes]]:
        """Read an existing operation with fresh auth and no creation/rewrite.

        Authentication is durably consumed before even checking whether the
        operation exists or whether its immutable body digest matches.  This
        method cannot issue effects, create an operation, or alter a terminal
        response; it is the only modeled ambiguity-reconciliation read path.
        """

        self._burn_authorization(authorization)
        self._require_authorization_separated(authorization, body_digest)
        record = self._operations.get(operation)
        if record is None:
            raise OracleError("status for unknown operation")
        self._require_identity(record, generation, body_digest)
        effect = record.effects.get(side_effect)
        if effect is None:
            raise OracleError("status for unknown effect")
        self._require_record_authorization_separated(record, authorization)
        if effect.request_digest != effect_request_digest:
            self._quarantine(record, "status effect request mismatch")
        record.authorizations.add(authorization)
        return effect.state, record.terminal_response

    def issue_once(
        self,
        operation: str,
        generation: int,
        body_digest: str,
        side_effect: str,
        effect_request_digest: str,
        authorization: str,
    ) -> str:
        self._burn_authorization(authorization)
        self._require_authorization_separated(
            authorization, body_digest, effect_request_digest
        )
        if side_effect not in SIDE_EFFECTS:
            raise OracleError("unknown side effect")
        record = self._operations.get(operation)
        if record is None:
            raise OracleError("effect issue for unknown operation")
        self._require_identity(record, generation, body_digest)
        self._require_record_authorization_separated(record, authorization)
        if effect_request_digest == body_digest:
            self._quarantine(record, "effect request digest is not independently bound")
        prior_effect = record.request_effects.get(effect_request_digest)
        if prior_effect is not None and prior_effect != side_effect:
            self._quarantine(record, "effect request reused across effects")
        effect = record.effects.get(side_effect)
        if effect is not None:
            if effect.request_digest != effect_request_digest:
                self._quarantine(record, "effect request rewrite")
            record.authorizations.add(authorization)
            return effect.state
        record.request_effects[effect_request_digest] = side_effect
        record.effects[side_effect] = EffectRecord(request_digest=effect_request_digest)
        record.authorizations.add(authorization)
        return "issued-ambiguous"

    def reconcile(
        self,
        operation: str,
        generation: int,
        body_digest: str,
        side_effect: str,
        effect_request_digest: str,
        observation_digest: str,
        observed: int,
        authorization: str,
    ) -> str:
        self._burn_authorization(authorization)
        self._require_authorization_separated(
            authorization, body_digest, effect_request_digest, observation_digest
        )
        record = self._operations.get(operation)
        if record is None:
            raise OracleError("reconciliation for unknown operation")
        self._require_identity(record, generation, body_digest)
        self._require_record_authorization_separated(record, authorization)
        effect = record.effects.get(side_effect)
        if effect is None or effect.issue_count != 1:
            raise OracleError("reconciliation without exactly one issue")
        if effect.request_digest != effect_request_digest:
            self._quarantine(record, "reconciliation effect request mismatch")
        if observation_digest in {body_digest, effect_request_digest}:
            self._quarantine(record, "observation digest is not independently bound")
        if effect.observation_digest is not None:
            if effect.observation_digest != observation_digest or effect.observed != observed:
                self._quarantine(record, "reconciliation outcome rewrite")
            record.authorizations.add(authorization)
            return effect.state
        if observed < 0:
            self._quarantine(record, "negative reconciliation cardinality")
        if observed == 1:
            effect.state = "reconciled-one"
        elif observed == 0:
            effect.state = "reconciled-zero-terminal"
            record.quarantined = True
        else:
            effect.state = "reconciled-multiple-terminal"
            record.quarantined = True
        effect.observation_digest = observation_digest
        effect.observed = observed
        record.authorizations.add(authorization)
        return effect.state

    def commit_response(
        self,
        operation: str,
        generation: int,
        body_digest: str,
        side_effect: str,
        effect_request_digest: str,
        response: bytes,
    ) -> None:
        record = self._operations[operation]
        self._require_identity(record, generation, body_digest)
        effect = record.effects.get(side_effect)
        if effect is None:
            self._quarantine(record, "terminal response lacks an issued effect")
        if effect.request_digest != effect_request_digest:
            self._quarantine(record, "terminal response effect request mismatch")
        if effect.state != "reconciled-one" or effect.observed != 1:
            self._quarantine(record, "terminal response lacks reconciled-one effect")
        if record.terminal_response is not None and record.terminal_response != response:
            self._quarantine(record, "terminal response changed")
        record.terminal_response = response

    def issue_count(self, operation: str, side_effect: str) -> int:
        return self._operations[operation].effects[side_effect].issue_count

    def has_operation(self, operation: str) -> bool:
        return operation in self._operations

    def is_quarantined(self, operation: str) -> bool:
        record = self._operations.get(operation)
        return record is not None and record.quarantined


TRANSITIONS = {
    "EMPTY": {"WRITER_ADMITTED", "QUARANTINED"},
    "WRITER_ADMITTED": {"MARKER_COMMITTED", "QUARANTINED"},
    "MARKER_COMMITTED": {"WRITER_REVOKING", "QUARANTINED"},
    "WRITER_REVOKING": {"WRITER_DELETED", "QUARANTINED"},
    "WRITER_DELETED": {"SOURCE_STABLE", "QUARANTINED"},
    "SOURCE_STABLE": {"FORK_ISSUED", "QUARANTINED"},
    "FORK_ISSUED": {"FORK_RECONCILED", "QUARANTINED"},
    "FORK_RECONCILED": {"RECOVERY_ADMISSION_AUTHORIZED", "QUARANTINED"},
    "RECOVERY_ADMISSION_AUTHORIZED": {"RECOVERY_ADMISSION_PUBLISHED", "QUARANTINED"},
    "RECOVERY_ADMISSION_PUBLISHED": {"OBSERVER_ADMITTED", "QUARANTINED"},
    "OBSERVER_ADMITTED": {"RECOVERY_READ_ONE", "QUARANTINED"},
    "RECOVERY_READ_ONE": {"RECOVERY_READ_TWO", "QUARANTINED"},
    "RECOVERY_READ_TWO": {"EVIDENCE_COMPLETE", "QUARANTINED"},
    "EVIDENCE_COMPLETE": {"CLEANING"},
    "QUARANTINED": {"CLEANING"},
    "CLEANING": {"CLEAN", "QUARANTINED"},
    "CLEAN": set(),
}


class BoundaryStateOracle:
    """Test-only transition oracle for deletion and recovery-read guards."""

    def __init__(self) -> None:
        self.state = "EMPTY"
        self.source_reads = 0
        self.recovery_reads = 0

    def transition(self, target: str) -> None:
        if target not in TRANSITIONS[self.state]:
            raise OracleError(f"invalid transition {self.state} -> {target}")
        self.state = target

    def read_source(self) -> None:
        self.source_reads += 1
        raise OracleError("observer source read is forbidden")

    def read_recovery(self) -> None:
        expected = {
            "OBSERVER_ADMITTED": "RECOVERY_READ_ONE",
            "RECOVERY_READ_ONE": "RECOVERY_READ_TWO",
        }
        target = expected.get(self.state)
        if target is None:
            raise OracleError("recovery read outside exact two-read window")
        self.recovery_reads += 1
        self.transition(target)
