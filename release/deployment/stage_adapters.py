"""Real, read-only verification adapters for the separate staging lifecycle.

The prior staging source can be absent from the production record chain. In
that case it is discovered only from the certificate returned by successful
cryptographic verification, then pinned for every component's verification.
No adapter constructs a provider client or grants a mutation capability.
"""

from __future__ import annotations

import release_record
import schema_change
import ship
import ship_common as common
import spec_images
import stage_contract as contract
import trivy_policy


require = contract.require
STAGING_SIGNER = ".github/workflows/staging-images.yml"


class Adapters:
    def __init__(self, ctx, control):
        # Pure checks first; make_adapters itself performs no network or file I/O.
        contract.validate_environment(ctx.env)
        require(not any(key.startswith("STAGING_") for key in ctx.env), "context-invalid:stage-mixed-secrets")
        require(ship.parse_mode(ctx.env) == ("stage", ""), "input-invalid:stage-mode")
        require(common.validate_context(ctx.env) == control, "context-invalid:stage-control")
        self.ctx, self.control = ctx, dict(control)

    def verify_plan(self, candidate, candidate_sha256, run_id):
        ctx, control = self.ctx, self.control
        require(common.validate_context(ctx.env) == control, "context-invalid:stage-control")
        require(ship.parse_mode(ctx.env) == ("stage", ""), "input-invalid:stage-mode")
        require(run_id == control["run_id"] and candidate_sha256 == ctx.env.get("CANDIDATE_SHA256"), "candidate-invalid:stage-binding")
        require(candidate == ship.decode_candidate(ctx.env, control, ctx.now()), "candidate-invalid:stage-payload")
        require(not any(ctx.env.get(key) for key in ("PLAN_TARGET_RELEASE", "PLAN_TARGET_MANIFEST_SHA256")), "input-invalid:stage-production-target")
        exceptions = trivy_policy.check(ctx.deps.trivy_policy_path, ctx.now().date())
        require(candidate["trivy"]["active_exceptions"] == exceptions, "candidate-invalid:stage-trivy-policy")
        age = (ctx.now() - common.require_timestamp(candidate["trivy"]["db_updated_at"], "candidate-invalid:stage-trivy-db")).total_seconds()
        require(ship.TRIVY_DB_MIN_AGE_SECONDS <= age <= ship.TRIVY_DB_MAX_AGE_SECONDS, "candidate-invalid:stage-trivy-db")
        bootstrap = ctx.bootstrap()
        require(not set(candidate["images"].values()).intersection(bootstrap["images"].values()), "candidate-invalid:bootstrap-digest")
        gh = ctx.gh()
        ship.require_ci_green(gh, control["sha"])
        ship.require_old_lanes_idle(gh, ctx.repo_dir)
        chain = release_record.resolve_chain(gh, ctx.repo_dir, bootstrap=bootstrap, work_dir=ctx.work_dir("stage-chain"))
        latest = chain.latest
        require(latest.release == ctx.env.get("PLAN_LATEST_RELEASE") and latest.manifest_sha256 == ctx.env.get("PLAN_LATEST_MANIFEST_SHA256"), "latest-changed-since-plan")
        release_record.require_no_downgrade(ctx.repo_dir, latest.sha, control["sha"])
        schema_change.guard(ctx.repo_dir, latest.sha, control["sha"])
        return True

    def _discover_source(self, images):
        gh = self.ctx.gh()
        identity = gh.signer_identity(release_record.SHIP_SIGNER, self.control["sha"], self.control["sha"])
        # Do not trust unsigned registry labels, predicate fields, a local hint,
        # or the caller's expected source when discovering the previous build.
        results = gh._json([
            "attestation", "verify", f"oci://{common.IMAGE_REPOSITORY['web']}@{images['web']}",
            "--repo", common.REPOSITORY,
            "--cert-identity", identity["buildSignerURI"],
            "--source-ref", release_record.SOURCE_REF,
            "--deny-self-hosted-runners", "--predicate-type", release_record.SLSA_PREDICATE,
            "--format", "json",
        ], code="attestation-unverified:stage-source")
        require(type(results) is list and bool(results), "attestation-unverified:stage-source")
        sources = set()
        for item in results:
            try:
                certificate = item["verificationResult"]["signature"]["certificate"]
                source = common.require_sha1(certificate["sourceRepositoryDigest"], "attestation-unverified:stage-source")
            except (KeyError, TypeError):
                common.fail("attestation-unverified:stage-source")
            require(release_record.certificate_matches(item, gh.signer_identity(release_record.SHIP_SIGNER, source, source)), "attestation-unverified:stage-source")
            sources.add(source)
        require(len(sources) == 1, "attestation-unverified:stage-source")
        return sources.pop()

    def verify_products(self, images, expected_source=None):
        images = spec_images.require_image_set(images, "attestation-unverified:stage-images")
        if expected_source is not None:
            common.require_sha1(expected_source, "attestation-unverified:stage-source")
        bootstrap = self.ctx.bootstrap()
        if images == bootstrap["images"]:
            source = bootstrap["sha"]
        else:
            require(not set(images.values()).intersection(bootstrap["images"].values()), "attestation-unverified:stage-mixed-bootstrap")
            source = expected_source or self._discover_source(images)
        require(expected_source is None or source == expected_source, "attestation-unverified:stage-source")
        # A previous staging build must be an ancestor of this dispatched main.
        release_record.require_no_downgrade(self.ctx.repo_dir, source, self.control["sha"])
        gh = self.ctx.gh()
        release_record.verify_images(gh, images, source, bootstrap)
        if images != bootstrap["images"]:
            for component in common.COMPONENTS:
                gh.verify_image(common.IMAGE_REPOSITORY[component], images[component],
                    signer_workflow=release_record.SHIP_SIGNER, signer_digest=source, source_digest=source,
                    predicate_type=release_record.SPDX_PREDICATE)
        return source

    def verify_staging(self, binding):
        common.exact_keys(binding, {"image", "source_sha"}, "attestation-unverified:stage-binding")
        source = common.require_sha1(binding["source_sha"], "attestation-unverified:stage-source")
        image = common.exact_string(binding["image"], "attestation-unverified:stage-image")
        repositories = [repo for repo in contract.STAGING_REPOS.values() if image.startswith(repo + "@")]
        require(len(repositories) == 1, "attestation-unverified:stage-image")
        contract.image_ref(image, repositories[0], "attestation-unverified:stage-image")
        release_record.require_no_downgrade(self.ctx.repo_dir, source, self.control["sha"])
        gh = self.ctx.gh()
        gh._verify("oci://" + image,
            gh.signer_flags(STAGING_SIGNER, source, source, release_record.SLSA_PREDICATE),
            gh.signer_identity(STAGING_SIGNER, source, source), code="attestation-unverified:staging-image")
        return source


def make_adapters(ctx, control):
    return Adapters(ctx, control)
