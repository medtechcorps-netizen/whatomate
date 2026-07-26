# ReReply reseller platform

ReReply has three authorization layers:

1. **Platform owner** — the existing global `is_super_admin` role. Platform
   owners can manage every reseller and organization.
2. **Reseller owner or administrator** — manages one or more reseller
   portfolios, but never receives global super-administrator access.
3. **Organization member** — works inside an individual customer CRM using
   the existing organization role and permission model.

Customer CRM records remain isolated by `organization_id` and PostgreSQL Row
Level Security (RLS). The reseller layer is a control plane over those
organizations; it does not weaken or replace tenant RLS.

## First deployment

Run the existing production migration command:

```bash
rereply rls-migrate -config /app/config.toml
```

The migration is idempotent. It:

- creates `resellers` and `reseller_members`;
- assigns every legacy organization to the `Platform Direct` reseller;
- records active platform super administrators as owners of that portfolio;
- adds traceable source metadata to organization memberships; and
- refuses runtime startup if any active organization has no reseller owner.

After migration, start the API and worker normally. The restricted runtime
database role must pass `VerifyTenantRLS` before either process starts.

## Onboard a reseller

1. Sign in as a platform owner and open **Partner Console**.
2. Select **New partner**.
3. Enter the partner company, customer-facing brand, support email, plan, and
   private workspace name.
4. Create the administrator as a normal user inside that private workspace.
5. Return to **Partner Console → Administrators** and assign the existing
   user's email as an `owner` or `admin`.
6. Provision customer workspaces from the portfolio tab.
7. Configure the brand colors, logo URL, support identity, organization limit,
   and optional custom hostname.

Adding a reseller administrator synchronizes inherited organization
memberships across all current customer workspaces. New workspaces
automatically receive the same administrators.

## Revocation and suspension

- Removing a reseller member deletes only memberships created by the reseller
  assignment. Independently granted, direct organization memberships are
  preserved.
- Suspending a reseller immediately invalidates inherited access at JWT,
  API-key, organization-switch, permission-cache, and organization-list
  boundaries.
- A suspended reseller disappears from reseller administrators' control plane.
  Platform owners retain access for support and recovery.
- The final reseller owner cannot be removed through the API.

## Plans and usage

The control plane currently supports `starter`, `growth`, and `enterprise`
plans. `max_organizations` is enforced before a workspace is provisioned.

The usage endpoint aggregates, per reseller:

- organization count;
- unique CRM users;
- WhatsApp accounts;
- contacts; and
- messages.

Tenant tables are counted inside a separate RLS-bound transaction for each
organization.

## Custom domains

`custom_domain` stores a validated, unique hostname without a protocol or
path. Saving the hostname does not create DNS records or TLS certificates.
DNS routing should only be enabled after the hostname is verified and the
deployment platform is configured to serve it.

## Security verification

Before production promotion, require:

```bash
go test -race -p 1 -timeout 10m ./...
cd frontend
npm run typecheck
npm run build
npm audit --audit-level=high
```

The reseller test suite covers cross-reseller visibility, inherited-access
synchronization, clean revocation, suspended-reseller lockout, legacy
backfill, and preservation of direct memberships.
