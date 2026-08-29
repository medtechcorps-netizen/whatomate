# Gate-C State Machine and Runbook

This is an inert design artifact for `gate-c-nonproduction-only`. It contains no executable provider command.

## State machine

| State | Required evidence | Allowed next state |
| --- | --- | --- |
| `EMPTY` | Exact disposable inventory; source empty; both firewalls empty | `WRITER_ADMITTED` |
| `WRITER_ADMITTED` | Writer-only private admission; distinct writer domain; fresh authorization consumed | `MARKER_COMMITTED`, `QUARANTINED` |
| `MARKER_COMMITTED` | One pinned fixed-key CAS; fixed readback; signed hash-only receipt | `WRITER_REVOKING`, `QUARANTINED` |
| `WRITER_REVOKING` | Binding removed; full redeploy; phase leaf/capability/mTLS/wrapping generation revoked | `WRITER_DELETED`, `QUARANTINED` |
| `WRITER_DELETED` | Ephemeral writer broker absent; no active/nonterminal/rollback-capable broker deployment | `SOURCE_STABLE`, `QUARANTINED` |
| `SOURCE_STABLE` | Original source firewall observed identically twice | `FORK_ISSUED`, `QUARANTINED` |
| `FORK_ISSUED` | Fork POST recorded exactly once; outcome may be ambiguous | `FORK_RECONCILED`, `QUARANTINED` |
| `FORK_RECONCILED` | Exactly one matching fork; no inherited writer admission | `RECOVERY_ADMISSION_AUTHORIZED`, `QUARANTINED` |
| `RECOVERY_ADMISSION_AUTHORIZED` | Writer ledger durably published one opaque hash-only `RecoveryAdmissionAuthorization` | `RECOVERY_ADMISSION_PUBLISHED`, `QUARANTINED` |
| `RECOVERY_ADMISSION_PUBLISHED` | Observer authority independently validated, signed, and durably published one exact `RecoveryAdmission` through the observer ledger | `OBSERVER_ADMITTED`, `QUARANTINED` |
| `OBSERVER_ADMITTED` | Observer-only recovery admission; no source admission | `RECOVERY_READ_ONE`, `QUARANTINED` |
| `RECOVERY_READ_ONE` | First fixed-key recovery read retained only in broker memory | `RECOVERY_READ_TWO`, `QUARANTINED` |
| `RECOVERY_READ_TWO` | Second recovery read equals first; zero source reads | `EVIDENCE_COMPLETE`, `QUARANTINED` |
| `EVIDENCE_COMPLETE` | Independent signatures verify; public marker hashes match | `CLEANING` |
| `QUARANTINED` | No further normal operation; cleanup authority only | `CLEANING` |
| `CLEANING` | Idempotent deletion ledger active | `CLEAN`, `QUARANTINED` |
| `CLEAN` | Two matching zero-resource observations; all temporary authority revoked | terminal |

There is no transition from `MARKER_COMMITTED`, `WRITER_REVOKING`, or `WRITER_DELETED` directly to observer admission. Fork authority is impossible until writer deletion and stable source-firewall proof are complete.

## Authorization versus operation identity

- `operation_sha256` is stable across an exact retry and commits to semantic operation, generation, normalized configuration, and intended side effect.
- `authorization_sha256` is fresh for every call and commits to audience, challenge, JWT identifier, workflow run, attempt, and expiry.
- The authority derives that digest only after raw-JWT and canonical-request
  verification. A broker capability includes the digest and exact
  envelope-issued, token-not-before, and exclusive token-not-after instants.
  The broker's signed CAS and readback carry distinct post-call completion
  instants, each of which must be inside
  `[max(envelope-issued, token-not-before), token-not-after)`; bearer
  JWT, JTI, and challenge values are never persisted in the capability or
  public evidence.
- The two digests must differ.
- Reusing an authorization fails even when the operation matches.
- Replaying a committed operation with a fresh authorization returns the exact recorded canonical result without reissuing its side effect.

## Universal side-effect procedure

The lifecycle `side_effect` is exactly one of: `authority-app-create`,
`authority-app-update`, `authority-app-delete`, `broker-app-create`,
`broker-app-update`, `full-redeploy`, `broker-app-delete`, `trusted-source-add`,
`trusted-source-delete`, `binding-install`, `binding-remove`,
`credential-install`, `credential-remove`, `leaf-issue`, `leaf-revoke`,
`capability-issue`, `capability-revoke`, `mtls-issue`, `mtls-revoke`,
`wrapping-key-revoke`, `marker-cas-v2`, `fork-post`, `evidence-publish`, or
`cleanup-delete`. Apply this procedure independently to every effect; a coarse
lifecycle label cannot stand in for one of these records.

1. Verify exact state and two protected read-only snapshots where applicable.
2. Validate fresh authorization and exact configured generation.
3. Compute the stable operation digest.
4. Atomically consume the JTI and challenge in a distinct durable transaction.
   That burn commits before any endpoint semantic check, signing, operation
   update, audit append, or side effect and cannot roll back when later work
   fails.
5. In a separate durable transaction, create or verify the operation record.
   Bind the exact base operation, complete request digest, and exact terminal
   authority (including zero versus nonzero) before consulting a cached result.
6. If the operation is already terminal with the same digest, return its recorded response.
7. If it is new, record `prepared` before issuing one side effect.
8. Record `issued` before awaiting the response.
9. On success, reconcile by GET and record the canonical response digest.
10. On HTTP 408, timeout, connection loss, or runner loss, do not reissue. Enter reconciliation.
11. Reconcile by bounded GET-only observations:
    - exactly one matching result: record `reconciled-one`;
    - zero after the bounded window: record `reconciled-zero-terminal` and quarantine;
    - more than one: record `reconciled-multiple-terminal` and quarantine.
12. One durable `issued` transition permits exactly one wire mutation request.
    SDK, transport, and middleware automatic-retry budgets must be zero; an
    ambiguous response can enter only the status/read reconciliation path.
13. A differing digest for an existing operation is terminal misuse.
14. Bind the exact control, runtime-source, normalized workflow-definition,
    configuration, image, and app-spec hashes into the signed receipt.

`evidence-observe` is an authenticated durable read operation, not a mutation
side effect. It still consumes fresh authentication before semantic checks and
publishes its two-read result issue-once; reconciliation may only read the
prepared/durable observation and cannot perform another provider GET, signing
operation, or mutation.

The local ledger implementation in `internal/protocol/ledger.go` is only a fault-injecting test oracle. It is not runtime code and is not evidence that a real durable provider ledger exists.

## Writer sequence

1. Admit only the writer broker identity to the disposable source.
2. Prove the observer broker and runner have no source path.
3. Writer authority validates OIDC and consumes a fresh challenge.
4. Writer broker generates the marker internally.
5. Broker performs the compiled one-key CAS and one readback only. A missing
   predecessor and a present empty value are never conflated.
6. The writer authority signs the broker-attested hash-only readback receipt.
   The broker does not sign public evidence. Raw marker bytes are erased from
   request/response state.
7. Begin terminal teardown immediately.

## Mandatory writer deletion

1. Revoke the phase capability, leaf, mTLS identity, and wrapping-key
   generation in that exact order.
2. Remove the source binding, then the source credential, in that exact order.
3. Complete a full redeploy so stale components lose injected credentials.
4. Replace the source firewall with its exact initial projection.
5. Delete the ephemeral writer broker app. The credential-free writer
   authority remains but its phase capability, leaf, mTLS identity, and
   wrapping generation are terminally revoked.
6. Verify the broker is absent from direct GET plus complete paginated app and
   deployment inventories, the delete action is terminal, the old-instance
   grace period elapsed, and no rollback-capable deployment remains. Bind the
   delete action to the exact durable broker-delete request, provider result,
   and completion witness rather than accepting an unrelated terminal action.
7. Read the source firewall, complete app inventory, complete deployment
   inventory, and complete provider-operation inventory twice after a fixed
   minimum delay. Each read independently asserts pagination completeness;
   both canonical reads must match the signed original and retain zero broker
   apps, deployments, rollback-capable deployments, and nonterminal provider
   operations. Record post-GET timestamps and independent GET-only provenance.
8. Canonically encode the complete deletion proof together with both read
   timestamps, the minimum delay, and GET-only provenance, then bind its
   SHA-256 as `terminal_evidence_sha256`. Only that signed terminal receipt may
   permit `FORK_ISSUED`.
   Each provider-observed effect timestamp must come from the independently
   observed provider fact and be no later than its distinct durable completion
   timestamp. A delayed 408/EOF/5xx reconciliation therefore preserves both
   times rather than pretending they are equal. Caller-supplied monotonic
   timestamps are not evidence.

Key-service sealer revocation and provider-side wrapping-key configuration
revocation are two distinct durable mutation operations. The sealer request has
its own fresh authorization, operation identity, result, and writer-root-signed
completion witness and must complete before a separately authorized provider
request may begin. The provider request has its own operation identity, result,
and signed completion witness. An HTTP 408, EOF, timeout, or 5xx on either
request permits only its own exact status reconciliation; neither mutation may
be reissued, and one witness cannot stand in as evidence for the other. Sealed
marker material is erased only after both terminal statuses and both durable
completions are recorded.

## Fork reconciliation

The fork request uses a provider-side configured source and destination. No caller supplies either identifier. The POST is issued once. An intentionally lost response must be resolved using the durable ledger and GET-only provider inventory. Zero, one, and multiple outcomes are tested; only one exact fork advances.

The fork must show no inherited writer rule. Any inherited, shared, broad, or unknown rule quarantines the experiment.

After terminal writer proof and exactly one fork result are both durable, the
writer-side fork controller may issue-once publish one opaque, hash-only
`RecoveryAdmissionAuthorization` through the writer ledger. It binds terminal
and fork completion plus the intended observer-domain identities, but is not
signed by the observer and exposes neither writer receipt material nor a marker
hash/preimage. The writer root signs the canonical authorization-publication
request inside the issue-once callback, and the outer protected verifier
requires both that signature and the exact completed writer-ledger record. An
ambiguous response permits only writer-ledger status
reconciliation; the authorization is never recreated.

The outer protected verifier validates that exact durable writer authorization
and projects a one-way, hash-only `ObserverAdmissionRequest`. The recovery
domain receives no writer key, root, ledger handle, signature, completion
witness, receipt, marker hash, or marker preimage. The observer authority
validates the projected recovery binding, signs a `RecoveryAdmission` with the
observer root, and issue-once publishes the signed admission through the
separate observer ledger. An ambiguous response permits only observer-ledger status
reconciliation, which returns the exact persisted signature and never signs
again. These are two distinct authorized effects, ledgers, and failure
boundaries; neither may be inferred from the other.

## Observer sequence

1. Require the opaque recovery admission to be durably published and consumed
   from the role-separated observer ledger. Complete a separately signed,
   issue-once observer admission lifecycle bound to the exact hash-only
   admission request, recovery admission, fork continuity, observer unit, and
   observer config; then admit only the observer broker identity to recovery.
2. Do not admit the observer to source.
3. Observer authority validates its distinct OIDC audience and fresh challenge.
4. The request contains no writer receipt, expected hash, marker, source selector, or endpoint.
5. Observer broker revalidates the exact exclusive capability immediately
   before each of exactly two compiled-key reads from recovery.
6. It requires both raw reads to match, hashes the bytes, erases the bytes, and
   sends the hash plus both bounded-separated read timestamps to the observer
   authority. Publishing that signed attestation is issue-once; after an
   ambiguous response, only status reconciliation may return the durable exact
   receipt without repeating either read.
   A second domain-separated signature binds the exact observer-ledger
   publication request and completed record.
7. Complete the signed observer cleanup lifecycle and remove observer recovery
   admission. Status paths may observe or reconcile that effect but may not
   issue it.
8. An outer verifier independently verifies both roots, both observer receipt
   signatures, the exact observer-ledger publication, the required admission
   and cleanup lifecycle proofs, independent fork-provider evidence, and the
   exclusive chronology from terminal writer proof through cleanup. Only then
   does it compare writer and observer marker hashes. A validly signed but
   unpublished receipt fails.

## Cleanup

Cleanup authority is unconditional after the first Gate-C write and survives failure.

1. Stop new challenge issuance.
2. Remove recovery and source admission rules.
3. Remove broker credential bindings and complete full redeploys where an app remains.
4. Delete observer apps and any writer remnants.
5. Delete recovery then source data stores.
6. Export only sanitized evidence digests, then delete both ledgers.
7. Delete lifecycle test resources, images, registry, and isolated network.
8. Revoke temporary roots, provider tokens, environments, branch policy, and branch.
9. Read the complete disposable inventory twice and require zero resources and zero nonterminal actions.

If cleanup is interrupted, resume from the recorded deletion operation. Never recreate a resource to simplify cleanup. Unknown residual state remains quarantined and blocks any later gate.

## Four-phase lifecycle rule

A future production design, if separately accepted, uses a fresh writer broker
app and phase generation for every release phase, chained to the persistent
credential-free writer authority root. Broker deletion terminates that
generation. No broker app identity, broad credential projection,
authorization, challenge, leaf, mTLS identity, wrapping key, or operation
namespace crosses phases. This prototype tests one synthetic generation only
and makes no production claim.
