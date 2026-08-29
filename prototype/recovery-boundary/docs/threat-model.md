# Recovery Boundary Threat Model

Status: **Gate-C nonproduction only**. This document closes a protocol boundary for a future disposable substrate experiment. It grants no deployment or production authority and contains no live identifiers.

## Security objective

Prove that a fixed-function writer can place one internally generated marker in a disposable source store, be irreversibly removed, and that an independently rooted observer can later attest the same marker from a disposable recovery store without receiving the marker, the writer receipt, or an expected hash.

The claim scope string is exactly `gate-c-nonproduction-only`. No artifact from this prototype is consumable by a release, recovery, rollback, or deployment controller.

## Independent authority domains

The prototype has two non-overlapping authority domains.

| Domain | Public authority app | Fixed-function broker app | Durable ledger | Signing root | Data-plane reach |
| --- | --- | --- | --- | --- | --- |
| Writer | Writer OIDC verifier | Writer marker broker | Writer ledger | Writer root | Source only |
| Observer | Observer OIDC verifier | Observer read broker | Observer ledger | Observer root | Recovery only |

All eight identities are distinct. A component, ledger role, signing root, credential, challenge namespace, or audience may not be shared across domains. Authority apps never receive database credentials. Broker apps accept no public caller input. Ledgers are durable authorization/idempotency stores, not data-plane proxies.

The future substrate must prove a private authenticated authority-to-broker path. If it cannot do so without giving the public authority component the broker credential or exposing a general broker endpoint, the substrate fails.

## Assets

- One marker of at least 256 bits, generated inside the writer broker.
- Writer and observer signing roots and their generation state.
- Fresh OIDC authorization, challenges, and JWT identifiers.
- Stable operation digests and side-effect ledgers.
- Source and recovery connection credentials held only by their broker domain.
- Protected provider descriptors containing raw app, database, network, and deployment identifiers.
- Public receipts containing semantic labels, public keys, signatures, bounded
  read timestamps, and separate hashes for control, runtime source, normalized
  workflow definition, configuration, image, and app spec only.

## Adversaries and failures

- An untrusted Internet caller reaching a public authority app.
- A compromised workflow attempting to select a key, command, host, or resource.
- Replayed, expired, cross-audience, cross-environment, or malformed OIDC authority.
- A caller reusing an operation with a different request or reusing an authorization.
- Runner loss, process restart, scaling, timeout, HTTP 408, or transport ambiguity.
- A stale writer deployment retaining source reach after apparent teardown.
- Provider reconciliation producing zero, one, or multiple matching side effects.
- A compromised observer being told the expected writer value or hash.
- Secret or raw identifier disclosure through logs, errors, artifacts, fixtures, or app specs.
- A test accidentally contacting external DNS or a non-loopback network address.

## Non-negotiable invariants

1. Requests contain only an operation label, generation, and fresh challenge. They contain no key, command, host, endpoint, resource identifier, marker, writer receipt, or expected hash.
2. The writer broker implements one compiled key and one pinned atomic CAS,
   followed by one source readback. Missing and present-empty predecessor states
   are distinct. Gate C must prove the exact command/script and fork copy; this
   local model makes no provider-support claim.
3. The marker is generated internally and never leaves either broker in raw form.
4. The writer authority, not the broker, signs the broker-attested source
   readback. Writer public evidence exposes the marker SHA-256 only.
5. Writer deletion is mandatory before any fork request can be authorized.
6. Deletion means credential binding removed, a full redeploy completed, the
   phase leaf/capability/mTLS/wrapping generation revoked, the ephemeral writer
   broker app absent, no active or rollback-capable broker deployment, an
   elapsed old-instance grace period, and two delayed matching restored
   firewall/action-ledger reads. The credential-free writer authority remains
   distinct and cannot restore source access.
7. The observer has recovery-only reach. It performs exactly two reads of the compiled key from recovery and zero source reads.
8. The observer receives neither writer evidence nor an expected marker hash. An outer verifier compares independently signed hashes.
9. A fresh authorization digest is distinct from the stable operation digest. Exact operation replay requires new authorization and returns the recorded result; authorization replay fails.
10. Authorization is durably consumed in its own committed transaction before
    every endpoint semantic check and exact low-level side effect. A later
    failure cannot restore that JTI or challenge. Each effect is issued at most once and has explicit
    zero/one/multiple reconciliation after ambiguity.
11. Runtime receipts attest only facts observed by that runtime. Provider topology, firewall, app, and deployment facts come from independent protected GET-only observations.
12. Raw identifiers remain in protected descriptors. Public evidence uses hashes and semantic labels only.
13. Writer and observer authority apps, broker apps, ledgers, and roots are pairwise distinct.
14. Every claim is explicitly Gate-C nonproduction only and non-authoritative.
15. Base operation identity, complete mutation request, and any terminal
    authority are separate exact ledger bindings. A key-only, body-only, or
    zero/nonzero-terminal substitution cannot reuse a completed record.
16. Writer teardown effects are durably ordered: capability, leaf, mTLS, and
    wrapping-key revocation; binding and credential removal; full redeploy;
    trusted-source restoration; then deletion. Independently observed provider
    effect times and durable ledger completion times are separate, signed
    values with exact monotonic ordering. They are never replaced by a
    caller-supplied sequence or forced equal after delayed reconciliation.
17. Observer reads require a completed admission lifecycle bound to the exact
    hash-only fork/admission continuity. Final boundary acceptance additionally
    requires its completed cleanup lifecycle; neither proof exposes writer
    receipt material or expected marker state.
18. Terminal writer evidence includes two complete, delayed app, deployment,
    and provider-operation inventories with zero matching apps, deployments,
    rollback-capable deployments, or nonterminal operations. A terminal delete
    action or stable hash alone is insufficient.

## OIDC boundary

Writer and observer use distinct audiences, challenge namespaces, protected
controller workflows, and protected environments. No controller or environment
may hold both domains' lifecycle authority. Validation covers RS256, header and
key identifier, issuer and JWKS, exact audience and subject, immutable
repository and owner identities, environment, ref, workflow reference,
workflow commit, event, run identity, attempt one, runner environment, time
bounds, and JWT identifier. The one-time challenge is issued and durably
consumed by the authority boundary; it is not claimed to be a GitHub JWT claim.

Only the exact `alg`, `kid`, and `typ` header set is accepted; `jku`, `x5u`,
embedded keys, and every unexpected algorithm or header fail. The JWKS URI is
pinned, refresh is bounded, and stale or unknown keys cannot select another
endpoint. Canonical request and evidence decoders reject duplicate or unknown
JSON keys, floats, noncanonical timestamps, Unicode and number encodings,
oversize bodies, trailing data, and nil/empty predecessor substitution.

The unexposed ledger/signer receives and validates the raw JWT plus canonical
request independently; gateway-parsed claims are never signing authority. A
broker additionally validates a root-signed role/phase/generation/operation
capability bound to its exact broker identity and mTLS peer identity. The
capability commits a domain-separated hash of the validated identity, method,
canonical body/envelope bindings, hashed JTI/challenge, and exact token times,
and carries the canonical envelope-issued, token-not-before, and exclusive
token-not-after instants. Each broker-signed post-action/post-read instant must
be greater than or equal to both envelope-issued and token-not-before, and
strictly less than token-not-after. Writer CAS and
readback completion are distinct instants; terminal provider and recovery
reads use the same trusted authority clock before and after each GET. The
capability contains no bearer value.

Protected-ref state is an independent GitHub API/workflow fact. It is not inferred from a JWT claim. Workflow commit authority is also separate from normalized workflow, configuration, image, and app-spec hashes.

## Marker secrecy and copy proof

The writer receipt and observer receipt are verified with unrelated public roots. The observer request schema cannot carry writer receipt material or an expected hash. Matching public marker hashes therefore prove an independent recovery read rather than a client-directed comparison.

Only after durable terminal writer proof and exactly one reconciled fork may
the writer-side controller issue-once publish a hash-only
`RecoveryAdmissionAuthorization` through the writer ledger. The outer
protected verifier validates its writer-root signature and exact writer-ledger
publication, then projects only hash-bound recovery continuity into an
`ObserverAdmissionRequest`. The observer runtime has no writer verification
key, root, ledger handle, signature, receipt, marker hash, or preimage. The
observer authority validates that projection, signs the distinct
`RecoveryAdmission` with the observer root, and issue-once publishes the exact
signature through the observer ledger. Ambiguity at either boundary permits
only status reconciliation in that boundary's ledger; it never repeats a
publication or signature. Neither object reveals a writer receipt, expected
marker hash, or preimage. Observer evidence publication is a third, separate
issue-once observer-ledger operation with the same status-only rule.
Observer evidence carries separate observation and publication signatures;
the outer verifier also requires its exact completed observer-ledger record.
A signed but unpublished receipt is invalid.

The observer performs two recovery reads separated by a bounded delay and requires byte equality before hashing. It never connects to source.

## Durable idempotency

Each domain ledger stores authorization consumption, operation digest, generation, side-effect state, canonical response digest, and terminal result. Authentication consumption is an independently committed transaction and cannot be rolled back with a later operation callback. In-memory state is never authority. A restart or scale event must converge on the same ledger record and signing generation.

Same operation plus same digest and fresh authorization is an idempotent replay. Same operation plus different digest, or reused authorization, is a hard failure.
The terminal authority (including its zero/nonzero state) is part of the
durable record and cannot change on an otherwise identical replay. Shallow
copies of a ledger test oracle are invalid instances and can never satisfy
writer/observer ledger separation.

## Ambiguous side effects

Broker-app create/update/delete, trusted-source add/delete, binding
install/remove, credential install/remove, leaf issue/revoke, capability
issue/revoke, mTLS issue/revoke, wrapping-key revoke, marker CAS, fork POST,
evidence publication, and each cleanup deletion separately follow issue-once
then reconcile. After a timeout, transport loss, or ambiguous response, the
protected controller performs GET-only reconciliation and never blindly
repeats the mutation. Zero and multiple observations are terminal/quarantine
outcomes unless the exact state machine explicitly defines zero as a safe
pre-side-effect state.

The terminal deletion proof binds two independently complete paginated app,
deployment, and provider-operation inventories, not merely zero returned
records. Its delete-action digest must match the exact durable broker-delete
request, provider result, and completion witness. Key-service sealer revocation
and provider-side wrapping-key configuration revocation are separate mutation
requests with fresh authorizations, distinct operation identities and results,
and separate writer-root-signed completion witnesses. The provider request may
start only after the sealer request completes. Ambiguity is reconciled against
the status of that exact request; neither witness substitutes for the other and
status reconciliation never invokes either revocation.

## Logging and public evidence

Logs and errors are fixed and non-reflective. They contain no request body, JWT, marker, credential, signing material, database address, raw provider identifier, or environment dump. Public JSON is closed-schema, canonically encoded, signed, and limited to hashes and semantic labels.

Gate-A memory zeroing and record deletion are fault-oracle assertions only.
They do not prove that every process copy, backup, or storage medium was erased;
that cryptographic-erasure property belongs to the separately authorized
role-separated Gate-C durable backends.

## Residual risk

A managed-Valkey credential is broader than the fixed key. Gate C may test that
credential only inside an isolated fixed broker against disposable synthetic
data and only after explicit user risk acceptance of that residual broad
credential authority. This acceptance is a prerequisite for the disposable
experiment only and is not acceptance for production. Failure of network
identity, private broker reachability, credential injection, key persistence,
teardown, or log sanitation ends the App Platform hypothesis.
