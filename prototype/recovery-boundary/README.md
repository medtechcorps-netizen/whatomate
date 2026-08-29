# Recovery Boundary Gate-A Prototype

This subtree is a **local-only, synthetic Gate-A model**. It is not production
recovery authority, is not a deployable workflow, and must not be consumed by
the production v1 or v2 recovery validators.

The model closes two design decisions:

- Raw app, deployment, network, and database identifiers stay in protected
  descriptors. Public evidence contains semantic labels and canonical hashes.
- The source-credential-bearing writer broker is created for one phase and
  generation, then revoked, detached, and permanently deleted before a fork is
  allowed. Scrubbing an active spec is not an alternative.

## Isolation model

Writer and observer use separate authority apps, broker apps, audiences,
durable-ledger interfaces, signing roots, and leaf keys. An authority has no
Valkey credential or provider mutation token. An ephemeral broker is the only
unit that may receive a provider-bound Valkey credential in a later authorized
nonproduction exercise.

Gate A uses only deterministic fixtures and loopback mocks. Its fault-injecting
store is a crash-model test oracle, not accepted durable storage. Gate A has no
production-enabled constructor: its exported adapter interfaces describe later
requirements but cannot mint production authority, even when an implementation
self-reports the requested properties. Gate C must supply and review concrete,
transactional, role-separated durable adapters and provider-confined signers.
Any zeroing performed by the local oracle is a logical state-machine assertion,
not proof of physical or cryptographic erasure across copies, backups, or media.

## Evidence flow

1. The writer broker generates exactly 32 random bytes internally.
2. It performs one fixed-key v2 CAS and returns a fixed readback attestation to
   the writer authority; the authority signs the broker-attested readback hash.
3. The ephemeral writer broker binding, leaf, capability, mTLS identity, and
   wrapping generation are revoked; source trust is restored; the broker app
   is deleted and absence is proven twice. The persistent credential-free
   writer authority remains and cannot restore source access.
4. Fork creation consumes one durable authorization and sends at most one POST.
   Only after terminal writer proof and exactly one reconciled fork are durable
   may the writer-side fork controller issue-once publish an opaque, hash-only
   `RecoveryAdmissionAuthorization` through the writer ledger. The writer root
   signs the exact canonical publication request inside that ledger operation,
   and consumers require both the signature and the matching completed ledger
   record. It is not an observer signature or admission.
5. The outer protected verifier independently validates the writer-root
   authorization and its exact completed writer-ledger record, then derives a
   one-way hash-only `ObserverAdmissionRequest`. The recovery domain receives
   only that projection: it has no writer public key, root, ledger handle,
   signature, completion witness, receipt, marker hash, or marker preimage.
   The observer authority validates the projected recovery binding, signs a
   `RecoveryAdmission` with the observer root, and issue-once publishes that
   exact admission through the distinct observer ledger. Ambiguity at either
   publication permits status reconciliation only.
6. Before either read, the observer domain must complete and sign its exact
   recovery-only admission lifecycle. That proof is bound to the hash-only
   observer admission request, the recovery admission, the fork continuity,
   and the observer-domain identity; it contains no writer receipt, key,
   marker hash, or preimage.
7. A recovery-only observer broker consumes that admission and reads the
   recovery marker twice, revalidating exclusive authority immediately before
   each read. Its evidence publication is itself issue-once and
   status-reconciled through the observer ledger. Its
   authority signs the broker-attested hash and both bounded read timestamps.
   A second domain-separated observer-root signature binds the exact completed
   observer-ledger publication request; the outer verifier requires both that
   signature and the matching ledger record.
8. The outer verifier validates the independent signature chains, fork GET
   provenance, zero-nonterminal provider-operation proof, admission lifecycle,
   cleanup lifecycle, and cross-domain chronology before comparing the writer
   source-readback hash with the observer recovery-read hash.

The observer never receives source trust, source credentials, the writer
receipt, the expected marker hash, the marker preimage, or the writer key.

## Authentication and idempotency

The immutable canonical operation body is separate from a fresh authentication
envelope. Every mutation or status call consumes a new JTI and challenge in a
standalone durable burn before any endpoint semantic or operation transaction.
Later signing, storage, audit, erasure, or crash failure cannot roll that burn
back.
Identical operation/body reuse is idempotent; differing reuse quarantines.
Status reconciliation is a separate method and cannot create a mutation.
Status cannot synthesize a missing terminal receipt, admission, or observer
receipt. A before-effect marker ambiguity is reconcilable only when sealed
writer material proves that the observed 32-byte value is the newly issued
marker, never merely an unchanged 32-byte predecessor.

Terminal abort evidence binds the exact key-service revocation action, binding,
result, status, and chronology before the oracle drops its sealed material.
Gate C must separately prove cryptographic erasure in both durable backends
while retaining only the public digest and signed receipt.

The authority derives a domain-separated validated-authorization digest from
the independently verified workload identity, method, body/envelope bindings,
hashed JTI and separately issued challenge, and exact token times. The
challenge is not represented as a GitHub OIDC claim. A broker capability carries
that digest plus the canonical envelope-issued, token-not-before, and exclusive
token-not-after instants. Every signed broker action or read completion must
fall inside `[max(envelope-issued, token-not-before), token-not-after)`.
For the writer this includes distinct post-CAS and post-readback completion
instants; a pre-call clock sample is not evidence that the call completed
inside the authority window. Neither object contains the raw JWT, JTI, or
challenge.

No OIDC claim is treated as evidence of branch protection. A later protected
workflow must independently check exact protected main before requesting OIDC
and again after receiving signed evidence.

Receipts bind distinct control, runtime-source, normalized workflow-definition,
configuration, image, and app-spec hashes. None may be substituted for another.

## Hard boundary

Files in this subtree may not make external network calls, contain live
identifiers or credentials, create provider resources, publish images, or
trigger GitHub Actions. Workflow examples are inert `*.tmpl` fixtures with
`on: []`, exact `contents: read`, and no OIDC or write permission.
Public JSON fixtures contain no private signing seed, credential, raw provider
selector, hostname, or endpoint.

`PATH_BLOBS.sha256` is the final stable-snapshot inventory. It lists every
other regular file in this subtree in sorted order with both its Git blob SHA-1
and raw-content SHA-256. The manifest explicitly excludes itself to avoid a
self-referential digest; `verify_gate_a.py` enforces the exact path set and all
listed bytes.
