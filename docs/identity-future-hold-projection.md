# Inactive future identity hold projection

The internal/identityreview package contains pure, unimported helpers for a future identity-review feature. It adds no server route, database adapter, grant writer, schema, migration, permission or routing capability. Original version-1 subject and intent encoding remains unchanged.

BuildHoldProjection canonicalizes supplied immutable hold evidence, including earlier zero-receipt generations, deduplicated member reason masks, receipt weights and individual refusal reasons. It separates direct BSUID and verified-phone namespaces. PageHoldProjection returns owned copies. Neither helper proves database completeness or authorizes access.

**ProjectionDigest must not be used as Intent.SnapshotDigest, a complete current preview seal, or grant authority.** Current selectors, canonical families, owners, routability, roles, membership, assignment and transfer facts are absent. A future separately versioned full observation must bind those facts with the verified projection and complete immutable/current candidate union. Derived current family members must never be inserted into immutable hold membership.

Before using this foundation in an adapter:

- Read bounded facts on the owned connection under the reviewed transaction and lock contract. Verify each hold's exact member count, canonical member digest and semantic claim digest before unioning. Supplied digests or a matching overall union are not proof.
- Preserve generation zero for unsupported holds. Enforce distinct positive cycle/version and nonnegative generation/count signed-bigint bounds. Future browser-facing large integers require exact decimal encoding, never rounded JavaScript numbers.
- Pin authenticated actor, selected organization, account, current cycle and projection/preview identifier independently of supplied data. Reauthorize the entire immutable/current union before every protected page and decision.
- Bound enumeration, recursion, cumulative records/bytes and cursor input before retention/parsing. Paging hashes the entire bounded projection without preemption: check the same caller-clipped budget before and after, and expose no result after cancellation.
- Prove actual membership, role-permission child, reseller, assignment, transfer and contact-deletion races. The organization selector lock alone does not serialize every authority writer. Positive witness row locks are only a proposed contract; absence/default/fallback cases need separate serialization proof.
- Keep historical phone-only evidence unproven where authenticated raw sidecar facts were not retained. Syntax or a source discriminator is not signature provenance, outbound-address proof or permission to merge histories.

Capacity limits have no production defaults. Receipt weights and endpoint units are not unique people. A future grant needs its own idempotent auditable lifecycle; approval must not replay old WAMIDs or cause AI, sends, booking, Resume, read receipts or provider work. Reconnect, revocation, protected-copy access, identity evolution and restart remain separate required protocols.

Prepared pure tests cover canonicalization, scope/domain separation, refusals and page ownership. They do not establish SQL completeness, RLS, authority locking or active-feature readiness. This source addition must pass ordinary full hosted gates and generated closure with update modes disabled.

Use the existing [catalog contract](catalog.md), [compatibility contract](compat.md), [tenant RLS runbook](tenant-rls-runbook.md) and [release runbook](release.md) for schema and release authority. This document grants no activation or migration exception and does not replace coordinated Stage2 prerequisites, notice or deployment windows.
