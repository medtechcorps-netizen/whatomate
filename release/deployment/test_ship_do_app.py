from __future__ import annotations

import copy
import http.client
import json
import sys
import unittest
import urllib.error
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import do_app
import ship_common as common
import spec_images
import test_ship_support as support


def client(opener: object, *, allow_put: bool = True, app_id: str = support.APP_ID) -> do_app.DOAppClient:
    return do_app.DOAppClient(
        app_id,
        support.PG_ID,
        support.DO_TOKEN,
        expected_app_id_sha256=support.sha256_text(support.APP_ID),
        allow_put=allow_put,
        opener=opener,
    )


class QueueOpener:
    """Returns queued responses or raises queued exceptions, in order."""

    def __init__(self, items: list[object]) -> None:
        self.items = list(items)
        self.requests: list[object] = []

    def open(self, request: object, timeout: float | None = None) -> object:
        del timeout
        self.requests.append(request)
        item = self.items.pop(0)
        if isinstance(item, BaseException):
            raise item
        if callable(item):
            return item(request)
        return item


class IncompleteResponse(support.FakeResponse):
    def read(self, amount: int = -1) -> bytes:
        raise http.client.IncompleteRead(b'{"app":')


class IdentityAndAllowlistTests(unittest.TestCase):
    def test_app_identity_mismatch_makes_zero_requests(self) -> None:
        opener = QueueOpener([])
        with self.assertRaisesRegex(common.ReleaseError, "^app-identity-mismatch$"):
            client(opener, app_id="99999999-9999-4999-8999-999999999999")
        self.assertEqual(opener.requests, [])
        with self.assertRaisesRegex(common.ReleaseError, "^app-identity-mismatch$"):
            client(opener, app_id=None)  # type: ignore[arg-type]

    def test_token_shape_is_checked(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^target-invalid:token$"):
            do_app.DOAppClient(support.APP_ID, support.PG_ID, "short",
                               expected_app_id_sha256=support.sha256_text(support.APP_ID), allow_put=True)

    def test_only_the_four_paths_are_reachable(self) -> None:
        fake = client(QueueOpener([]))
        for path in (
            f"/v2/apps/{support.APP_ID}/logs",
            f"/v2/apps/{support.APP_ID}/deployments/not-a-uuid",
            "/v2/apps/99999999-9999-4999-8999-999999999999",
            f"/v2/databases/{support.PG_ID}/backups",
            f"/v2/databases/{support.PG_ID}/backups?page=2&per_page=200",
            f"/v2/databases/{support.PG_ID}?page=1&per_page=200",
            f"/v2/databases/{support.PG_ID}/users",
            "/v2/account",
        ):
            with self.subTest(path=path), self.assertRaisesRegex(common.ReleaseError, "^provider-invalid:path$"):
                fake._url(path)
        self.assertEqual(fake._url(fake.backups_path), f"https://api.digitalocean.com/v2/databases/{support.PG_ID}/backups?page=1&per_page=200")

    def test_response_url_status_type_and_size_are_checked(self) -> None:
        url = f"https://api.digitalocean.com/v2/apps/{support.APP_ID}"
        cases = {
            "response-url": support.FakeResponse(b"{}", url + "/other"),
            "status": support.FakeResponse(b"{}", url, status=201),
            "content-type": support.FakeResponse(b"{}", url, content_type="text/html"),
            "size": support.FakeResponse(b"", url),
            "json": support.FakeResponse(b"{\"a\":1.5}", url),
        }
        for detail, response in cases.items():
            with self.subTest(case=detail), self.assertRaisesRegex(common.ReleaseError, f"^provider-invalid:{detail}$"):
                client(QueueOpener([response])).get_app()

    def test_get_transport_failures_are_classified(self) -> None:
        url = f"https://api.digitalocean.com/v2/apps/{support.APP_ID}"
        for error in (support.http_error(url, 500), urllib.error.URLError("x"), TimeoutError(), OSError()):
            with self.subTest(error=type(error).__name__), self.assertRaisesRegex(common.ReleaseError, "^provider-get-failed$"):
                client(QueueOpener([error])).get_app()
        with self.assertRaisesRegex(common.ReleaseError, "^provider-get-failed$"):
            client(QueueOpener([IncompleteResponse(b"", url)])).get_app()

    def test_redirects_are_refused(self) -> None:
        handler = do_app.RejectRedirects()
        with self.assertRaisesRegex(common.ReleaseError, "^provider-invalid:redirect$"):
            handler.redirect_request(None, None, 302, "Found", {}, "https://example.invalid")

    def test_requests_carry_the_token_only_in_the_authorization_header(self) -> None:
        fake = support.FakeDO()
        client(fake).get_app()
        self.assertEqual(fake.headers[0]["authorization"], f"Bearer {support.DO_TOKEN}")
        self.assertEqual(fake.headers[0]["user-agent"], "rereply-ship/1")


class PutTests(unittest.TestCase):
    def test_dry_run_client_cannot_mutate_before_any_io(self) -> None:
        opener = QueueOpener([])
        with self.assertRaisesRegex(common.ReleaseError, "^dry-run-client-cannot-mutate$"):
            client(opener, allow_put=False).put_app_once(support.make_spec())
        self.assertEqual(opener.requests, [])

    def test_exactly_one_put_per_client(self) -> None:
        fake = support.FakeDO()
        api = client(fake)
        api.put_app_once(spec_images.set_images(support.make_spec(), support.NEW))
        with self.assertRaisesRegex(common.ReleaseError, "^second-mutation-blocked$"):
            api.put_app_once(support.make_spec())
        self.assertEqual(fake.put_count(), 1)
        body = fake.put_bodies[0]
        self.assertEqual(set(body), {"spec", "update_all_source_versions"})
        self.assertIs(body["update_all_source_versions"], False)

    def test_definitive_and_ambiguous_classification(self) -> None:
        url = f"https://api.digitalocean.com/v2/apps/{support.APP_ID}"
        for code in (400, 401, 403, 404, 405, 409, 415, 422):
            api = client(QueueOpener([support.http_error(url, code)]))
            with self.subTest(code=code), self.assertRaises(common.ReleaseError) as caught:
                api.put_app_once(support.make_spec())
            self.assertNotIsInstance(caught.exception, common.AmbiguousMutation)
            self.assertEqual(str(caught.exception), "provider-rejected")
            self.assertFalse(api.mutation_ambiguous)
        for error in (support.http_error(url, 408), support.http_error(url, 500), support.http_error(url, 503),
                      TimeoutError(), urllib.error.URLError("x"), OSError(),
                      IncompleteResponse(b"", url), support.FakeResponse(b"not json", url),
                      support.FakeResponse(b"{}", url, status=202)):
            api = client(QueueOpener([error]))
            with self.subTest(error=repr(error)[:40]), self.assertRaises(common.AmbiguousMutation):
                api.put_app_once(support.make_spec())
            self.assertTrue(api.mutation_ambiguous)
            self.assertEqual(api.put_count(), 1)

    def test_scrub_forgets_the_token(self) -> None:
        api = client(QueueOpener([]))
        api.scrub()
        self.assertEqual(api._token, "")


class ObservationTests(unittest.TestCase):
    def test_observe_stable_reads_twice_and_requires_live_equal_active(self) -> None:
        fake = support.FakeDO()
        snapshot = do_app.observe_stable(client(fake))
        self.assertEqual(snapshot.active_id, support.ACTIVE_ID)
        self.assertEqual(snapshot.public["images"], support.LIVE)
        self.assertEqual([method for method, _ in fake.requests], ["GET"] * 4)

    def test_observe_stable_refuses_pinned_pending_and_changing_state(self) -> None:
        with self.assertRaisesRegex(common.ReleaseError, "^cas-changed:deployment-pending$"):
            do_app.observe_stable(client(support.FakeDO(pinned=True)))
        fake = support.FakeDO()
        fake.spec["services"][0]["instance_count"] = 3
        with self.assertRaisesRegex(common.ReleaseError, "^cas-changed:live-differs-from-active$"):
            do_app.observe_stable(client(fake))
        fake = support.FakeDO()
        counter = {"n": 0}

        def bump(provider: support.FakeDO, path: str) -> None:
            if path.endswith(support.APP_ID):
                counter["n"] += 1
                provider.updated_at = f"2026-10-04T11:0{counter['n']}:00Z"

        fake.get_hooks.append(bump)
        with self.assertRaisesRegex(common.ReleaseError, "^cas-changed:double-read$"):
            do_app.observe_stable(client(fake))

    def test_observe_settled_retries_until_two_reads_agree(self) -> None:
        fake = support.FakeDO()
        counter = {"n": 0}

        def bump(provider: support.FakeDO, path: str) -> None:
            if path.endswith(support.APP_ID) and counter["n"] < 3:
                counter["n"] += 1
                provider.updated_at = f"2026-10-04T11:0{counter['n']}:00Z"

        fake.get_hooks.append(bump)
        sleeps: list[float] = []
        snapshot = do_app.observe_settled(client(fake), sleeper=sleeps.append, poll_limit=5)
        self.assertEqual(snapshot.active_id, support.ACTIVE_ID)
        self.assertTrue(sleeps)

    def test_materially_unchanged_ignores_only_updated_at(self) -> None:
        api = client(support.FakeDO())
        first = do_app.observe_stable(api)
        public = dict(first.public, app_updated_at_sha256="0" * 64)
        do_app.require_materially_unchanged(first, first._replace(public=public))
        changed = dict(first.public, canonical_spec_sha256="0" * 64)
        with self.assertRaisesRegex(common.ReleaseError, "^post-deploy-guard"):
            do_app.require_materially_unchanged(first, first._replace(public=changed))


class CandidateTests(unittest.TestCase):
    def setUp(self) -> None:
        self.desired = spec_images.set_images(support.make_spec(), support.NEW)

    def app(self, **values: object) -> dict:
        app = {"spec": self.desired, "active_deployment": {"id": support.ACTIVE_ID, "phase": "ACTIVE", "spec": support.make_spec()}}
        app.update(values)
        return app

    def test_old_active_is_never_locked_in_when_excluded(self) -> None:
        app = self.app()
        self.assertEqual(do_app.deployment_candidate(app, self.desired), support.ACTIVE_ID)
        self.assertIsNone(do_app.deployment_candidate(app, self.desired, exclude_ids={support.ACTIVE_ID}))

    def test_in_flight_deployment_wins(self) -> None:
        new = support.NEW_DEPLOYMENT_IDS[0]
        app = self.app(in_progress_deployment={"id": new, "spec": self.desired})
        self.assertEqual(do_app.deployment_candidate(app, self.desired, exclude_ids={support.ACTIVE_ID}), new)
        both = self.app(in_progress_deployment={"id": new, "spec": self.desired},
                        pending_deployment={"id": support.NEW_DEPLOYMENT_IDS[1], "spec": self.desired})
        with self.assertRaisesRegex(common.ReleaseError, "^reconcile-failed:multiple-inflight$"):
            do_app.deployment_candidate(both, self.desired)

    def test_lagging_in_flight_listing_still_reconciles(self) -> None:
        fake = support.FakeDO(lag=2)
        api = client(fake)
        api.put_app_once(self.desired)
        _app, deployment, ambiguous, candidate = do_app.reconcile_until_active(
            api, self.desired, job_name="rereply-rls-migrate", web_digest=support.NEW["web"],
            exclude_ids={support.ACTIVE_ID}, sleeper=lambda _: None, poll_limit=10,
        )
        self.assertEqual(candidate, support.NEW_DEPLOYMENT_IDS[0])
        self.assertFalse(ambiguous)
        self.assertEqual(do_app.deployment_object(deployment)["phase"], "ACTIVE")


class MigrationTests(unittest.TestCase):
    def deployment(self, **changes: object) -> dict:
        value = support.deployment_object(support.NEW_DEPLOYMENT_IDS[0], spec_images.set_images(support.make_spec(), support.NEW), "ACTIVE")
        value.update(changes)
        return value

    def check(self, deployment: dict) -> bool:
        return do_app.migration_succeeded(deployment, job_name="rereply-rls-migrate", web_digest=support.NEW["web"])

    def test_progress_tree_success(self) -> None:
        self.assertTrue(self.check(self.deployment()))

    def test_legacy_phase_field(self) -> None:
        self.assertTrue(self.check(self.deployment(jobs=[{"name": "rereply-rls-migrate", "phase": "SUCCEEDED"}])))
        with self.assertRaisesRegex(common.ReleaseError, "^post-deploy-guard:migration-failed$"):
            self.check(self.deployment(jobs=[{"name": "rereply-rls-migrate", "phase": "FAILED"}]))

    def test_failures(self) -> None:
        cases = {
            "missing": (self.deployment(jobs=[]), "migration-inventory"),
            "duplicate": (self.deployment(jobs=[{"name": "rereply-rls-migrate"}, {"name": "rereply-rls-migrate"}]), "migration-inventory"),
            "status": (self.deployment(progress=support.migration_progress("ERROR")), "migration-failed"),
            "no-progress": (self.deployment(progress=None), "migration-progress"),
            "empty-progress": (self.deployment(progress={"steps": []}), "migration-failed"),
            "source-digest": (self.deployment(jobs=[{"name": "rereply-rls-migrate", "source_image_digest": support.LIVE["web"]}]), "migration-digest"),
        }
        for label, (deployment, detail) in cases.items():
            with self.subTest(case=label), self.assertRaisesRegex(do_app.PostDeployGuard, f"^post-deploy-guard:{detail}$"):
                self.check(deployment)
        wrong_spec = self.deployment()
        wrong_spec["spec"] = support.make_spec()
        with self.assertRaisesRegex(do_app.PostDeployGuard, "^post-deploy-guard:migration-digest$"):
            do_app.migration_succeeded(
                dict(wrong_spec, jobs=[{"name": "rereply-rls-migrate"}]),
                job_name="rereply-rls-migrate", web_digest=support.NEW["web"],
            )

    def test_job_name_is_a_parameter(self) -> None:
        with self.assertRaisesRegex(do_app.PostDeployGuard, "^post-deploy-guard:migration-inventory$"):
            do_app.migration_succeeded(self.deployment(), job_name="other-job", web_digest=support.NEW["web"])


class ReconcileTests(unittest.TestCase):
    def run_reconcile(self, fake: support.FakeDO, *, poll_limit: int = 8) -> tuple:
        api = client(fake)
        desired = spec_images.set_images(support.make_spec(), support.NEW)
        try:
            api.put_app_once(desired)
        except common.AmbiguousMutation:
            pass
        return api, do_app.reconcile_until_active(
            api, desired, job_name="rereply-rls-migrate", web_digest=support.NEW["web"],
            exclude_ids={support.ACTIVE_ID}, sleeper=lambda _: None, poll_limit=poll_limit,
        )

    def test_accept_reaches_active(self) -> None:
        fake = support.FakeDO()
        _api, (app, _deployment, ambiguous, candidate) = self.run_reconcile(fake)
        self.assertEqual(do_app.app_object(app)["active_deployment"]["id"], candidate)
        self.assertFalse(ambiguous)

    def test_ambiguous_applied_reconciles(self) -> None:
        fake = support.FakeDO(scenarios=["ambiguous-applied"])
        api, (_app, _deployment, ambiguous, _candidate) = self.run_reconcile(fake)
        self.assertTrue(ambiguous)
        self.assertEqual(fake.put_count(), 1)

    def test_error_and_cancel_raise_terminal_with_the_candidate(self) -> None:
        for scenario in ("error", "cancel"):
            fake = support.FakeDO(scenarios=[scenario])
            with self.subTest(scenario=scenario), self.assertRaises(do_app.TerminalDeployment) as caught:
                self.run_reconcile(fake)
            self.assertEqual(caught.exception.deployment_id, support.NEW_DEPLOYMENT_IDS[0])
            self.assertEqual(str(caught.exception), "deployment-error")

    def test_deadline_raises_timeout(self) -> None:
        with self.assertRaises(do_app.ReconcileTimeout):
            self.run_reconcile(support.FakeDO(scenarios=["stall"]), poll_limit=4)
        with self.assertRaises(do_app.ReconcileTimeout) as caught:
            self.run_reconcile(support.FakeDO(scenarios=["ambiguous-not-applied"]), poll_limit=3)
        self.assertIsNone(caught.exception.deployment_id)

    def test_migration_failure_at_active_carries_the_candidate(self) -> None:
        fake = support.FakeDO(migration_status="ERROR")
        with self.assertRaises(do_app.PostDeployGuard) as caught:
            self.run_reconcile(fake)
        self.assertEqual(caught.exception.deployment_id, support.NEW_DEPLOYMENT_IDS[0])


class RollbackPreconditionTests(unittest.TestCase):
    def test_true_after_error_even_though_live_differs_from_active(self) -> None:
        fake = support.FakeDO(scenarios=["error"])
        api = client(fake)
        desired = spec_images.set_images(support.make_spec(), support.NEW)
        api.put_app_once(desired)
        with self.assertRaises(do_app.TerminalDeployment):
            do_app.reconcile_until_active(api, desired, job_name="rereply-rls-migrate", web_digest=support.NEW["web"],
                                          exclude_ids={support.ACTIVE_ID}, sleeper=lambda _: None, poll_limit=6)
        self.assertEqual(fake.active, support.ACTIVE_ID)
        self.assertNotEqual(fake.spec, fake.deployments[support.ACTIVE_ID]["spec"])
        self.assertTrue(do_app.observe_for_rollback(client(fake), desired))

    def test_false_when_spec_changed_or_a_deployment_is_pending_or_pinned(self) -> None:
        desired = spec_images.set_images(support.make_spec(), support.NEW)
        fake = support.FakeDO()
        self.assertFalse(do_app.observe_for_rollback(client(fake), desired))
        fake = support.FakeDO(spec=desired)
        fake.deployments[support.ACTIVE_ID]["spec"] = copy.deepcopy(desired)
        self.assertTrue(do_app.observe_for_rollback(client(fake), desired))
        fake.pinned = support.ACTIVE_ID
        self.assertFalse(do_app.observe_for_rollback(client(fake), desired))
        fake.pinned = None
        fake.deployments[support.NEW_DEPLOYMENT_IDS[0]] = support.deployment_object(support.NEW_DEPLOYMENT_IDS[0], desired, "PENDING_DEPLOY")
        fake.pending = support.NEW_DEPLOYMENT_IDS[0]
        self.assertFalse(do_app.observe_for_rollback(client(fake), desired))

    def test_false_when_reads_fail(self) -> None:
        url = f"https://api.digitalocean.com/v2/apps/{support.APP_ID}"
        self.assertFalse(do_app.observe_for_rollback(client(QueueOpener([support.http_error(url, 500)])), support.make_spec()))


if __name__ == "__main__":
    unittest.main()
