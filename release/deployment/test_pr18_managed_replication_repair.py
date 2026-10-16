"""Exact managed-owner replication edge under an externally reviewed consumer.

Historical source bytes are inert inputs to disposable Git histories. These
tests do not establish live role facts, provider acceptance or release authority.
"""
import base64
import hashlib
import json
from pathlib import Path
import re
import shutil
import unittest
import zlib
from unittest import mock

import schema_change as sc
import source_git
import test_pr18_run_metadata_repair as historical

POLICY = "release/deployment/schema_change.py"
OLD_TEST = "release/deployment/test_pr18_run_metadata_repair.py"
TEST = "release/deployment/test_pr18_managed_replication_repair.py"
FIXTURE = "release/deployment/fixtures/pr18-managed-replication-repair.json"
SLOT = re.compile(rb"(?m)^_PR18_MANAGED_REPLICATION_POLICY_SHA256 = '([0-9a-f]{64})'\n")
HISTORICAL_WRAPPER_SHA256 = '750c846b773a4b8cc8d5129c0136f9d3b59dd304d42233f1e83ff30c4a8c384c'
SOURCE_PINS = {'cmd/whatomate/migration_plan_managed_owner_admission_pg_test.go': ('1f65a0b7da0d8b0637416e64bf061f7fd7fbdbcf27dd95799c06ea1fb04b6abb',
                                                                     '3e4ef3d4ffc94f4be16e03eb2c1efef227f5a7869e95d8a586b846e3a78616d5'),
 'cmd/whatomate/migration_plan_managed_owner_pg_test.go': ('b8cc9cc0090e03a06d657f1af5d70bf97c60dabd8dd0f6e65aa211007bd8d5d4',
                                                           'faa099ea8cc47569f1bbaddabf03285b7d0bde8f1d797abd9908a3407e48e215'),
 'docs/catalog.md': ('29f182362f3128732ea55f6a3ba43dcc099e84fb2f1e3b3a599db4b3cbe115dc',
                     '4c2641310c4f316af3a7553579beae81df5431688bfef7687675784fac40dae1'),
 'internal/database/data_gates.go': ('b8cb4b1b608f87069e2724ae29d4b6b38bcdbcf85e5c2bf085734015902953cd',
                                     'a56b6a9281cddb47f845ccd9a9b5920ecfb061f7c87d12d35d54f27c29977d06'),
 'internal/database/plan.go': ('83d6f4e83ced0c9a02f3ce18470e3da54914cb63c77f70f9a8fda7cb44c68365',
                               '922b3f623817045bbe0f45a870bd910844a697a0289d23f94ab6503bed9df67b'),
 'internal/dbcatalog/registry/migration_path.json': ('4acecb4e3caed65bb71f8d61ea2b3e760129523bfc599130fa3aee5ed421969d',
                                                     '08618e383eed2ab9eb771918b0ef618efa2ecce8edb99cfd476bd18980938ba6')}


HISTORICAL_REPLICATION_INPUTS = {
    'release/deployment/schema_change.py': (146262, 'ad898c247153afaa43da9a78a52ba8739f40900e373bba468f14587790078c12', (
        'eNrsvfl/G0eWJ/g7/4oseGcJyCCYmbjpYlXTMu3SjmxpKLm6qykWnKeEFgiwAVCyStL/vt/3XkRkRB4AKLt6ZnanPt0WkRkZ'
        '54t3H1/94fR+sz6N58vTbPnOu/uwfbNa9o9ardZVFqUnq+Xig7fOFlm0ybxkEW0283yerXve42gbLVavvejubjFPou18tfTm'
        'G2+52nrz27tFdpstt1nqvcnWWe/o6Pk6O0nUF+ssWa3TDf7dRvOlt32Teav1/PV8GS28TfImu428fJ1l/8jMICdxFq3nS/Pp'
        'URItvSRarz946Wp9Gy23XrJKs9MUzb3kTbR8nW263vs380Xm3c5fr2V2mND6w91qvtxuvGiZelGynb/Ljl6vFmm21J9hiFua'
        'VbxYJW+ztOddZa/nG3zoPX7ivc6WGTrL5Pt32Zr2YuOlGTZGBjlKFqvN/Trz3kSbN9nmG6wOm3K7Su8xFfMBljxfe3izXa0/'
        'nK6zd/MNTTBZYYaYVY92/+goX69uvdksv9+iw9mMtnW13mJo7DEPtjk60s/Wr++i9SbTv9N5ni/msf5Jk7F+/scGE1V/r803'
        'mw8bGfIu2lJrPd5z/JQX2w93dAjq+cXyQ9d7ssWGxIus6/0U3Wbpy3uc/NERuupRL735cpOtt22/62EL29RTGwvCscxmnd46'
        '26wW77J2B23XOJxOp5gUw9tMjttM8M38bpasbm+xVdHGk7/0y7tsmWJys7tFZJ4JMM2W94sFzbH0eL5Ms19LzzCwHKP7eJst'
        'AWOzu/WK5n50dPTDzxdX311+N3t5dXn5wjv32i1AVbYGBDMMxpj8aavrFU8BAdlic9rqmE+/f/K0/CkgMF1k680pYHEbr7az'
        'fLF6PzMA3Hu9anWtDn7+6fHLJ89+4k6OPPyv3Upu09P3+Hh1Cyg9JUDmj7zW+n559fTFj7qrVqe77wsG1g/2R1f3y+38Njvk'
        '22gxxz5kL7I1ujEd/IhNOODrdfaf9/N1lr7YRuvt/d13akMfq9tx8Nz3fm82nu7+MlucLrLXUfJhdgvEpHr6Nkre4swXT/nN'
        'v2KwzcXd3ZNlvPqV+ukc/fjkh6sLOoba86hCxd1qs30NyFf9X7nn8ngFeAciBF6omWZTJ+vGTr5frZ/j8mcP6Oz9fPvGdPUi'
        '22wccNn7OXDhPM3s6Vwk24d1oajMB9lzdKWowAO6EAD4njEnOniOt0Cu6XzvTHD9c1AUQjMga9Eyycy6tlmyfa7ePzavn4B2'
        'bOfbD1dA4tn7l6Bjr7P1c0EUu0YSlKI6/ytP9yU/wnQFsvQ9/+7y8VOGqUO2DvPCwKm9/xoA0OOzp99d/oSurI5iRZVPhQye'
        'to4eX7y8ePrsBzRT7b9Gv9KoR5SjdfSXJy9ePrv6m9NCETPV4oenz7694FkXLV4vVnG0mG0JFW9UuxeXl9+5rTZZluq3V5c/'
        'YCQeqG7Ga0WZeTdmm212pz8sLuXzi5d/2fe5QbAzplnlLq6ePXtJc7zms2y9xv24j3vY6dPNmzXQ3BpfzRcWInLQUm8fNut+'
        'YbclhP6F3ezC1F/aZSPh+E0d/ub9q1yeXvNt6R7dHH337OrHi59eNkCP4jtnFlOpIOcrDxjvHrzs86s+mKolmL3tPPHi1WoL'
        'cIvuwKxt5zmmDhaVuOVbcMjgsSNiD5kNBLOSLXpH3168uHz65KfL2V/92Yu/XITDEabykVerruiZ1wrGUTYaT4No3B8Phnk/'
        '6w/yvO+PR36UZOk0D8fxdJIFk2mG33gX+JM8D9JJPO0PRsN4orZP3Wh0OI2nUT/Ikn4WBqOB3w+iOPYHWRSOgzwe+1E/DP1x'
        'nk4Gw3ASJ+M8irJokoVhPp2MBsOgrzpUCAAdJkE4zKbhBFPLomjiR+kwDZN0Mor8LAvC/igeRONgMhlEw+nUH/fDaRxOk0Ga'
        '9iMMl1GHn49+vPi32cXVyyffXzx+Ofv2by+Zcxp4j7zAD/U/3EgtZPb42Y8/PuFrG/qDydHzn799+uTx7KeLHy/xaG2Ovr1u'
        'XV+c/Ht08o/ZjfrDP5nOeqcnNx/9bjgcfn7178SzPTv405tH/MXREVMx77GSmERAahdMcueM9+ktuNAzYo75lwKqMwKWBT9Z'
        'gw8Go3/mbemjazTser1e74Zfbu5vb6P1h+rLF48vfvoJpEOjLmGVHIaUfty9fU0caYHqHl88fUoo8+XlFbMx3G9bTcQsHBeL'
        'Ounwc1BDTz3wIDZJY/6g9ap3cb9dyc3KXrUVZKh3MZCCuXT/issLmkmEbPPq/aO6tg0MTl1rMGiLgphWO8tuV++y5vcvQIGe'
        'Z+vbObM+m4tlerUCxaq0+37+64sPoDq39Nr6oNLwMQ5xm32X5dH9YnuR3s6XDU2ekERSM5JmQgnMbmpW3JOdwW60X3Ve9dp/'
        'PpP+Pl2k6aeLBc7m03fr1d2nK6z5NutUe2j/ed55FeO7y1+z5NNV9L6DnjaPrn951brBv3iRSIcRd5ZSZ1tQoISeYejl9hOE'
        '2NXb7BMJZUCJrzZfr5boUo3SOSpuxAsWqR6ztP2tCNltEeV6VyL2Xa7Xq7W6H2mWQwSeL+fb2ay9yRZ5t7gSWvYkwL/peCd/'
        '8n5aLbMzs67N/V22hnRpvm+JOHciov6JEvFbneILDNBT/Rvw30AQxBw32batXnU6tByeGsgOnt6tZul8fcaicpeEccxuAeai'
        'mFn8AVqDM3WTwJUuSzJuDx3NVvfbu/uiv6533TpJhK3D9fvP+xVYHIxwvspzPH1E49x0WfNxTvTvJI+Imp2pVb6+j9ZpS89U'
        '1pwyi7OpTJnIIaOhLhQ2kWAknjevwkIvtKAbtY7oPfbI2QGaMakeaM4nJ8vVyZohbqN+48+TDVQX9/LgHy0ZWMbkJq0bhVXm'
        '2SKlI8AgvQ1Y7W07br361ffVYc1z3YQUMfLn9Ulw452fe3GrVcCAvOrdre7a5stFtmzL847337ywaKzAkLaxvWNDqakc1Vn9'
        '/hCzeGOwI+sZCDeu6QRIF2JNoOuFHQtieXPwuVoRf3rTg0yFM263ok0yn4uEDEElySzIBRNSdEP/o3MudQT2Oig6u9/mJxOr'
        'h+zXJLvbej8v5/Se7yBpWfDY7XgdzUkTWL2xO7bMY/URujI94RgA0jk0M+DNkjdMTL/78eXP/3bT6qpt6JA60b3RDzyk4qB6'
        '0FJCQdRuS9dd3p5Ox76P0lDfa+CGuHJLtKJO3RTqo7gp+Nf7ZM2XIIO0ZTVXZLE52ULBaS6B7leugHR806mDe+I2SYEIAJuD'
        '8gh80R8ALz0gtpYe3WhwJx5TfVfspFo0TZefkcqj683AkeKSonv1wbV/Q8q5LQvONJVty7lGqlnH+8O5F0CLy5+XQMyGV2rH'
        'kImmrGXRa/RanevgLLyhBtdxi7a/ddM8XfW7srObN6v3GDBvfdR7+vnsIw34GZhlx7zUsecgaSwIbrNft0D89+tE0KI6Wdme'
        '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'
        'TZtn7bPS8ZrtXlKxvbbkFbGSeanUr5THS6bZ9UrZoKt5YLsF2+SgK0r3emC+X1bNntXUnZ/ZtbJFs1aqGll7NW0uzk0d58CM'
        'IBIleVs5yK2MXk72dzuTbumVyltw6HpL1k5rwDJEV2ZXDg5vcH2z8x81NDmrCTszeyhGyZOtuBgeVQvG845KK52hyuRNKW+x'
        '0mnoiuLW8vSbXVMp5bxpmos003ORXzVzsdQq1elYL6shrs7pt3fsQHfXlNyOi2W6AO80+sr7ltXJ0Md5uszqan2q1eoyJaU0'
        '4OqP76AKp1qNokfcfFPqjbihIoZDcWsFF0BaAeGT3y8LXplCiCRX7aa3J2/iQ7Pe11SUn1np1XfEROlINzpaklPr4icsab62'
        'dqUjLh5aJ5lqmlTaFkKu7UWxCzE1R7QYfaCVy7Hoszl4Zfd3+y+4kzSpbmKFftLJE1nJnVQ3uR3f7r0IykR4blRaJaxWZAlp'
        'lxxw92bVq+UoSzkHrYFrGbwdM6hnJh4wFTvvsjWPKsu3YxI1vMsDZlBOYHdeTW134DzqGaaGqTyy0+zXTqyU4q+YVwPta5pW'
        'TfPfMCsdVkkGpkryeZ2nx841byeZN+7UJtbQ9ZQ+0DlIuUA0JPM+35nc+9zJ9H3YeOXE8nvSg5/XZQvfma67SCDQkNTxbGfC'
        'w8PSOdbCQS3MVnM0ujOry9RYytLYxAqU2Xmlb21VyG6dbXWvfFzpzZhN6tC9MiZ2TeqAEp/cgOl3fEaschFK3BAzbQfK1zUp'
        'CQvFfatJBVHnuYE+57f3t570a8p/cY3TUnQ33VK1fcz4/a6ZCzksrchb2HzX9uYv/OzS4X/ajDle7jfMuEB01oyVs8N5TVYN'
        'hXq7nos3zfpUVjj9u2uAweBRt2h91eJqYFULI/UMp/GF00ynjsHTrmCu9GtAsjm8v2yoKNp1S4UFJTFvYcfgXkmZu69HbiPV'
        'H2SP3S6dWdYmSKn0qFqVZ2h39ged0s9oHpWNqJmRV/ExemvfS1jAie0K42oXFMSQzp6ukJMyw1fh1+XHOxwdHWlCfClPd+eP'
        'rGLUE1YXt8ruxsbFhCx25GFym8G5kUWWlo2sYXKVAAHJs4vnhSm54EqNAVlBH7udb6z+67C/5S5i0jTCWSTw8b/Cgd3usV24'
        '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'
    )),
    'release/deployment/test_pr18_managed_replication_repair.py': (34304, '023f81db0b9ac67724e799234658ec2bee8001479156737dd55897bb64d72393', (
        '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'
    )),
}


def _historical_replication_input(path):
    size, expected, encoded = HISTORICAL_REPLICATION_INPUTS[path]
    if (type(size) is not int or not 0 < size <= 146262
            or type(encoded) is not str or len(encoded) > 4 * 146262):
        raise AssertionError("independent historical replication input bounds")
    try:
        packed = base64.b64decode(encoded, validate=True)
        decoder = zlib.decompressobj()
        raw = decoder.decompress(packed, size + 1)
    except (ValueError, zlib.error) as exc:
        raise AssertionError("independent historical replication input encoding") from exc
    if (len(raw) != size or not decoder.eof or decoder.unused_data or decoder.unconsumed_tail
            or hashlib.sha256(raw).hexdigest() != expected):
        raise AssertionError("independent historical replication input identity")
    return raw


class PR18ManagedReplicationRepairTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        historical.PR18RunMetadataRepairTests.setUpClass()
        raw = (Path(__file__).parent / "fixtures/pr18-managed-replication-repair.json").read_bytes()
        doc = json.loads(raw)
        if (type(doc) is not dict or set(doc) != {"schema_version", "before", "after"}
                or type(doc["schema_version"]) is not int or doc["schema_version"] != 1
                or len(SOURCE_PINS) != 6
                or any(type(doc[side]) is not dict or set(doc[side]) != set(SOURCE_PINS)
                       for side in ("before", "after"))):
            raise AssertionError("independent replication fixture schema")
        for path, pair in SOURCE_PINS.items():
            for side, expected in zip(("before", "after"), pair, strict=True):
                value = doc[side][path]
                if type(value) is not str or hashlib.sha256(value.encode()).hexdigest() != expected:
                    raise AssertionError("independent replication fixture identity")
        wrapper = Path(historical.__file__).read_bytes()
        if hashlib.sha256(wrapper).hexdigest() != HISTORICAL_WRAPPER_SHA256:
            raise AssertionError("independent run-metadata wrapper identity")
        cls.policy_input = _historical_replication_input(POLICY)
        cls.before = {**doc["before"],
                      POLICY: historical._historical_run_metadata_input(POLICY).decode(),
                      OLD_TEST: historical._historical_run_metadata_input(OLD_TEST).decode(),
                      TEST: None, FIXTURE: None}
        cls.after = {**doc["after"], POLICY: cls.policy_input.decode(),
                     OLD_TEST: wrapper.decode(), TEST: _historical_replication_input(TEST).decode(),
                     FIXTURE: raw.decode()}
        if len(cls.after) != 10 or set(cls.before) != set(cls.after):
            raise AssertionError("closed independent ten-path repair")

    def setUp(self):
        # The predecessor constructs its original exact history from frozen
        # inputs, including its original policy and test, under this consumer.
        self.prior = historical.PR18RunMetadataRepairTests()
        self.prior.setUp()
        self.addCleanup(self.prior.doCleanups)
        self.repo, self.base = self.prior.repo, self.prior.base
        self.predecessor = self.prior.head()
        for path, value in self.before.items():
            dest = self.repo.root / path
            self.assertEqual(dest.read_text(encoding="utf-8") if dest.is_file() else None, value)

    def head(self, extra=None):
        self.repo.checkout(self.predecessor)
        return self.repo.commit({**self.after, **(extra or {})}, "one exact managed replication repair")

    def local_refuses(self, head, base=None):
        base = base or self.predecessor
        self.assertIn("pr18-managed-replication-repair-incomplete",
                      sc.check_ci_source(self.repo.root, base, head))
        self.assertIn("classifier-v2-invalid:pr18-managed-replication-repair",
                      sc.classify_v2(self.repo.root, base, head).reasons)

    def full_refuses(self, head):
        self.assertTrue(sc.check_ci_source(self.repo.root, self.base, head))
        self.assertTrue(sc.classify_v2(self.repo.root, self.base, head).reasons)

    def test_exact_local_and_full_origin_keep_distinct_truthful_results(self):
        head = self.head()
        legacy = sc.classify(self.repo.root, self.predecessor, head)
        self.assertEqual((legacy.kind, legacy.dormant, legacy.reasons),
                         ("none", False, ("migration-path-changed:closure",)))
        local = sc.classify_v2(self.repo.root, self.predecessor, head)
        self.assertEqual((local.kind, local.dormant, local.reasons), ("none", False, ()))
        self.assertEqual(local.migration_changes, ("generated migration closure changed",))
        with mock.patch.object(sc, "_pr18_managed_replication_changed",
                side_effect=AssertionError("full origin must defer the redundant scan")), \
                mock.patch.object(sc, "_pr18_observer_transition", wraps=sc._pr18_observer_transition) as proof:
            full = sc.classify_v2(self.repo.root, self.base, head)
            self.assertEqual(proof.call_count, 1)
        self.assertEqual((full.kind, full.dormant, full.reasons), ("data", True, ()))
        self.assertEqual(full.migration_changes,
                         self.prior.historical.old.anchor_fixture.MIGRATION_OBSERVATIONS)
        self.assertEqual(local.requirements, ("compat-required", "generated-migration-closure-ci"))
        self.assertEqual(full.requirements, local.requirements)
        self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head), ())
        with mock.patch.object(sc, "_pr18_managed_replication_changed",
                side_effect=AssertionError("full origin must defer the redundant scan")), \
                mock.patch.object(sc, "_pr18_observer_transition", wraps=sc._pr18_observer_transition) as proof:
            self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
            self.assertGreaterEqual(proof.call_count, 1)

    def test_deferred_full_origin_cannot_clear_a_failed_final_transition(self):
        head = self.head()
        legacy = sc.classify(self.repo.root, self.base, head)
        self.assertEqual(legacy.reasons, sc._PR18_OBSERVER_LEGACY)
        with mock.patch.object(sc, "_pr18_managed_replication_changed",
                side_effect=AssertionError("the exact legacy path is deferred")), \
                mock.patch.object(sc, "_pr18_observer_transition", side_effect=sc._Invalid) as proof:
            full = sc.classify_v2(self.repo.root, self.base, head)
            self.assertIn(sc._PR18_OBSERVER_CALL, full.reasons)
            self.assertEqual(proof.call_count, 1)
            proof.reset_mock()
            ci = sc.check_ci_source(self.repo.root, self.base, head)
            self.assertIn(sc._PR18_OBSERVER_CALL, ci)
            self.assertIn(sc._PR18_OBSERVER_REASON, ci)
            self.assertEqual(proof.call_count, 1)

    def test_historical_run_metadata_inputs_are_not_current_consumer_bytes(self):
        for path, size, digest in (('release/deployment/schema_change.py',
  131619,
  '3ca123b80da6c8c22dc12bd962d94322135bac6b8da306f99636debd7e3d6c28'),
 ('release/deployment/test_pr18_run_metadata_repair.py',
  601800,
  'ebd8f028d75e8ad095ffc031ab02e8bbe078a8dadbadf17d5ede80d2d163b49f')):
            raw = historical._historical_run_metadata_input(path)
            self.assertEqual((len(raw), hashlib.sha256(raw).hexdigest()), (size, digest))
            self.assertEqual(self.before[path].encode(), raw)
            self.assertNotEqual(self.after[path].encode(), raw)
        size, digest, encoded = historical.HISTORICAL_RUN_METADATA_INPUTS[POLICY]
        packed = base64.b64decode(encoded, validate=True)
        variants = (
            (size - 1, digest, encoded),
            (601801, digest, encoded),
            (True, digest, encoded),
            (size, "0" * 64, encoded),
            (size, digest, base64.b64encode(packed[:-1]).decode()),
            (size, digest, base64.b64encode(packed + b"trailing").decode()),
            (size, digest, base64.b64encode(zlib.compress(b"x" * (size + 10))).decode()),
            (size, digest, "!invalid-base64!"),
        )
        for index, variant in enumerate(variants):
            with self.subTest(decoder_variant=index), \
                    mock.patch.dict(historical.HISTORICAL_RUN_METADATA_INPUTS, {POLICY: variant}):
                with self.assertRaises(AssertionError):
                    historical._historical_run_metadata_input(POLICY)

    def test_each_missing_or_changed_input_refuses(self):
        for path in self.after:
            with self.subTest(path=path, mutation="missing"):
                head = self.head({path: self.before[path]})
                self.local_refuses(head); self.full_refuses(head)
            with self.subTest(path=path, mutation="changed"):
                head = self.head({path: self.after[path] + "\n# unreviewed\n"})
                if path == "internal/dbcatalog/registry/migration_path.json":
                    # Invalid JSON retains the existing classifier's refusal;
                    # it must not be relabelled as an admission-edge failure.
                    self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head),
                                     ("classifier-invalid:migration-path",))
                    self.assertEqual(sc.classify_v2(self.repo.root, self.predecessor, head).reasons,
                                     ("classifier-invalid:migration-path",
                                      "classifier-v2-invalid:migration-path"))
                else:
                    self.local_refuses(head)
                self.full_refuses(head)
        path = "internal/dbcatalog/registry/migration_path.json"
        with self.subTest(path=path, mutation="valid-json-whitespace"):
            # Valid, semantically identical JSON must still match the reviewed
            # whole-file postimage exactly; the original pin assertions apply.
            head = self.head({path: self.after[path] + "\n"})
            self.local_refuses(head); self.full_refuses(head)

    def test_stale_closure_and_protected_source_refuse(self):
        path = "internal/dbcatalog/registry/migration_path.json"
        head = self.head({path: self.before[path]})
        self.local_refuses(head); self.full_refuses(head)
        for path in ("internal/dbcatalog/registry/data_steps.json", "internal/dbcatalog/golden/seeds.json",
                     "internal/dbcatalog/dormant_entrypoints.json", sc.CATALOG):
            self.repo.checkout(self.predecessor)
            value = (self.repo.root / path).read_text(encoding="utf-8") + "\n"
            head = self.head({path: value})
            self.local_refuses(head); self.full_refuses(head)
        for path in (sc._V2_PENDING + "unreviewed.json", sc.schema_nullable.SOURCE_PATH):
            head = self.head({path: "{}\n"})
            self.local_refuses(head); self.full_refuses(head)

    def test_split_extra_prefix_suffix_and_reapply_refuse(self):
        path = "internal/database/plan.go"
        self.repo.commit({path: self.after[path]}, "partial")
        head = self.repo.commit(self.after, "remaining")
        self.local_refuses(head); self.full_refuses(head)
        head = self.head({"README.md": "unreviewed extra\n"})
        self.local_refuses(head); self.full_refuses(head)
        self.repo.checkout(self.predecessor)
        self.repo.commit({OLD_TEST: self.after[OLD_TEST]}, "historical wrapper prefix")
        head = self.repo.commit(self.after, "repair after separate wrapper edit")
        self.local_refuses(head); self.full_refuses(head)
        self.repo.checkout(self.predecessor)
        self.repo.commit({"README.md": "nonempty prefix\n"})
        head = self.repo.commit(self.after, "repair after prefix")
        self.local_refuses(head); self.full_refuses(head)
        self.head()
        head = self.repo.commit({"README.md": "nonempty suffix\n"})
        self.local_refuses(head); self.full_refuses(head)
        self.head()
        self.repo.commit(self.before, "undo repair")
        head = self.repo.commit(self.after, "repeat repair")
        self.local_refuses(head); self.full_refuses(head)

    def test_complete_undo_cannot_escape_clean_delta(self):
        self.head()
        head = self.repo.commit(self.before, "complete undo")
        self.assertEqual(self.repo.git("diff", "--name-only", self.predecessor, head), "")
        self.local_refuses(head); self.full_refuses(head)

    def test_invalid_marker_introduction_then_undo_still_refuses(self):
        self.repo.checkout(self.base)
        self.repo.commit({TEST: self.after[TEST]}, "marker without predecessors")
        head = self.repo.commit({TEST: None}, "undo marker")
        self.assertEqual(self.repo.git("diff", "--name-only", self.base, head), "")
        self.local_refuses(head, self.base)

    def test_noop_prefix_and_suffix_preserve_exact_edge(self):
        self.repo.git("commit", "--allow-empty", "-m", "prefix noop")
        self.repo.commit(self.after, "exact edge after noop")
        self.repo.git("commit", "--allow-empty", "-m", "suffix noop")
        head = self.repo.git("rev-parse", "HEAD").strip()
        self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head), ())
        self.assertEqual(sc.check_ci_source(self.repo.root, self.base, head), ())
        self.assertFalse(sc.classify_v2(self.repo.root, self.base, head).reasons)

    def test_wrong_mode_and_renamed_input_refuse(self):
        for path, value in self.after.items():
            dest = self.repo.root / path
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_text(value, encoding="utf-8", newline="\n")
        (self.repo.root / TEST).chmod(0o755)
        self.repo.git("add", "-A")
        self.repo.git("update-index", "--chmod=+x", TEST)
        self.repo.git("commit", "-m", "wrong executable mode")
        head = self.repo.git("rev-parse", "HEAD").strip()
        self.local_refuses(head); self.full_refuses(head)
        head = self.head({TEST: None, TEST + ".renamed": self.after[TEST]})
        self.local_refuses(head); self.full_refuses(head)

    def test_policy_slot_does_not_self_authorize_changes(self):
        match, = SLOT.finditer(self.policy_input)
        body = self.policy_input + b"\n# unreviewed policy change\n"
        canonical = body[:match.start(1)] + b"0" * 64 + body[match.end(1):]
        recomputed = body[:match.start(1)] + hashlib.sha256(canonical).hexdigest().encode() + body[match.end(1):]
        for value in (body, recomputed, self.policy_input + match.group(0),
                      self.policy_input[:match.start(1)] + b"f" * 64 + self.policy_input[match.end(1):]):
            with self.subTest(digest=hashlib.sha256(value).hexdigest()):
                head = self.head({POLICY: value.decode()})
                self.local_refuses(head); self.full_refuses(head)

    def test_wrong_preimage_and_preexisting_marker_fail_exact_edge(self):
        for path, value in (("internal/database/plan.go", "// wrong preimage\n"),
                            (TEST, "# preexisting marker\n")):
            self.repo.checkout(self.predecessor)
            wrong = self.repo.commit({path: value}, "wrong predecessor")
            head = self.repo.commit(self.after, "otherwise exact edge")
            with self.assertRaises(sc._Invalid):
                sc._pr18_managed_replication_local(sc._Repository(self.repo.root), wrong, head)
            self.full_refuses(head)

    def test_second_parent_base_does_not_prove_first_parent_lineage(self):
        self.repo.git("checkout", "-b", "replication-side", self.predecessor)
        self.repo.git("commit", "--allow-empty", "-m", "side")
        side = self.repo.git("rev-parse", "HEAD").strip()
        self.repo.git("checkout", "-b", "replication-main", self.predecessor)
        self.repo.commit(self.after, "exact edge")
        self.repo.git("merge", "--no-ff", side, "-m", "side merged")
        head = self.repo.git("rev-parse", "HEAD").strip()
        self.local_refuses(head, side)

    def test_completed_base_has_no_permanent_test_path_reservation(self):
        head = self.repo.commit({OLD_TEST: self.before[OLD_TEST] + "\n# ordinary historical test edit\n"})
        self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head), ())
        policy = sc.classify_v2(self.repo.root, self.predecessor, head)
        self.assertEqual((policy.kind, policy.dormant, policy.reasons, policy.migration_changes),
                         ("none", False, (), ()))
        self.full_refuses(head)
        base = self.head()
        head = self.repo.commit({TEST: self.after[TEST] + "\n# ordinary later test edit\n"})
        self.assertEqual(sc.check_ci_source(self.repo.root, base, head), ())
        policy = sc.classify_v2(self.repo.root, base, head)
        self.assertEqual((policy.kind, policy.dormant, policy.reasons, policy.migration_changes),
                         ("none", False, (), ()))
        self.full_refuses(head)

    def test_original_deadline_history_and_blob_bounds_remain(self):
        head = self.head()
        for call in (sc.check_ci_source, sc.classify_v2):
            clock = [100.0]
            reader = source_git.ImmutableGit(self.repo.root, Path(shutil.which("git")), clock=lambda: clock[0])
            with self.assertRaises(source_git.SourceGitRefusal):
                with reader.operation():
                    clock[0] = reader.deadline
                    call(self.repo.root, self.predecessor, head)
            self.assertTrue(reader.failed and reader.closed)
        original = sc._git
        def huge(root, args):
            if args[:3] == ["rev-list", "--first-parent", "--reverse"]:
                return (b"a" * 40 + b"\n") * (sc.MAX_HISTORY_COMMITS + 1)
            return original(root, args)
        with mock.patch.object(sc, "_git", side_effect=huge):
            self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head),
                             ("classifier-invalid:migration-path",))
        head = self.head({"internal/database/plan.go": " " * (sc.MAX_ARTIFACT_BYTES + 1)})
        # The original dormant-entrypoint reader enforces this bound before
        # the repair guard; retain that precise fail-closed error ownership.
        self.assertEqual(sc.check_ci_source(self.repo.root, self.predecessor, head),
                         ("ci-source-pr18-observer-transition-required",
                          "classifier-invalid:dormant-entrypoints", "migration-path-changed:closure"))
        self.assertEqual(sc.classify_v2(self.repo.root, self.predecessor, head).reasons,
                         ("classifier-invalid:dormant-entrypoints",))
        self.full_refuses(head)


class ManagedReplicationHistoryReuseTests(unittest.TestCase):
    def test_retained_history_avoids_an_added_scan_and_unrelated_before_probe(self):
        base, head = "a" * 40, "b" * 40
        repository = sc._Repository(Path("unused-static-replication-history"))
        raw = (head + "\n").encode()
        sc._pr18_run_metadata_remember_history(repository, base, head, raw)
        with mock.patch.object(repository, "_tree", return_value=b"") as tree, \
                mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(sc, "_pr18_managed_replication_before") as before:
            self.assertFalse(sc._pr18_managed_replication_changed(repository, base, head))
            self.assertFalse(sc._pr18_managed_replication_changed(repository, base, head))
        before.assert_not_called()
        self.assertEqual(git.call_args_list, [mock.call(repository.root, ["cat-file", "-t", base])] * 2)
        self.assertIn(mock.call(head, sc._PR18_MANAGED_REPLICATION_MARKER), tree.call_args_list)
        self.assertEqual(repository._run_metadata_history_reads[(base, head)], raw)

    def test_failed_introduction_is_not_cached_and_undo_is_visible(self):
        base, introduced, head = "a" * 40, "b" * 40, "c" * 40
        repository = sc._Repository(Path("unused-static-replication-history"))
        sc._pr18_run_metadata_remember_history(repository, base, head,
                                             (introduced + "\n" + head + "\n").encode())
        def tree(revision, path):
            return b"present" if (revision, path) == (introduced, sc._PR18_MANAGED_REPLICATION_MARKER) else b""
        with mock.patch.object(repository, "_tree", side_effect=tree), \
                mock.patch.object(sc, "_git", return_value=b"commit\n"), \
                mock.patch.object(sc, "_pr18_verification_repair_history",
                                  return_value=((base, introduced), (introduced, head))) as history, \
                mock.patch.object(sc, "_pr18_managed_replication_edge", side_effect=sc._Invalid) as edge:
            for _ in range(2):
                with self.assertRaises(sc._Invalid):
                    sc._pr18_managed_replication_changed(repository, base, head)
            self.assertEqual(history.call_count, 2)
            self.assertEqual(edge.call_args_list, [mock.call(repository, base, introduced)] * 2)
        self.assertNotIn((base, head), getattr(repository, "_managed_replication_introductions_checked", set()))

    def test_cached_and_marked_paths_retain_active_reader_failures(self):
        base, head = "a" * 40, "b" * 40
        for cached in (False, True):
            repository = sc._Repository(Path("unused-static-replication-history"))
            if cached:
                repository._managed_replication_introductions_checked = {(base, head)}
            else:
                repository._trees[(base, sc._PR18_MANAGED_REPLICATION_MARKER)] = b"present"
            with mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal("git-failed:source-budget")):
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_managed_replication_changed(repository, base, head)
            if not cached:
                self.assertFalse(getattr(repository, "_managed_replication_introductions_checked", set()))


class ManagedReplicationBeforeReuseTests(unittest.TestCase):
    @staticmethod
    def fixture():
        prior, current = "a" * 40, "b" * 40
        path = "internal/database/plan.go"
        value = b"// reviewed synthetic source\n"
        oid = hashlib.sha1(b"blob " + str(len(value)).encode() + b"\x00" + value).hexdigest()
        tree = b"100644 blob " + oid.encode() + b"\t" + path.encode() + b"\x00"
        repo = sc._Repository(Path("unused-static-replication-before"))
        repo.cache[(prior, path)] = value
        repo._trees[(prior, path)] = tree
        repo.total = len(value)
        return repo, prior, current, path, value, tree

    def test_exact_current_tree_and_both_hashes_reuse_success_with_original_accounting(self):
        repo, prior, current, path, value, tree = self.fixture()
        with mock.patch.object(repo, "_tree", return_value=tree) as read:
            sc._pr18_managed_replication_reuse_before_blob(
                repo, current, path, hashlib.sha256(value).hexdigest())
        read.assert_called_once_with(current, path)
        self.assertEqual(repo.cache[(current, path)], value)
        self.assertEqual(repo.total, 2 * len(value))
        self.assertEqual(repo.cache[(prior, path)], value)

    def test_wrong_identity_or_poisoned_cache_never_primes_current_preimage(self):
        for variant in ("current-oid", "current-mode", "current-path", "current-absent",
                        "current-malformed", "prior-oid",
                        "prior-mode", "prior-path", "poisoned-bytes", "missing-bytes",
                        "mutable-bytes", "wrong-sha256", "both-wrong-oid"):
            with self.subTest(variant=variant):
                repo, prior, current, path, value, tree = self.fixture()
                digest = hashlib.sha256(value).hexdigest()
                current_tree = tree
                wrong_oid = b"100644 blob " + b"f" * 40 + b"\t" + path.encode() + b"\x00"
                if variant == "current-oid": current_tree = wrong_oid
                elif variant == "current-mode": current_tree = tree.replace(b"100644", b"100755", 1)
                elif variant == "current-path": current_tree = tree.replace(path.encode(), b"wrong/path.go")
                elif variant == "current-absent": current_tree = b""
                elif variant == "current-malformed": current_tree = b"malformed tree row"
                elif variant == "prior-oid": repo._trees[(prior, path)] = wrong_oid
                elif variant == "prior-mode": repo._trees[(prior, path)] = tree.replace(b"100644", b"100755", 1)
                elif variant == "prior-path": repo._trees[(prior, path)] = tree.replace(path.encode(), b"wrong/path.go")
                elif variant == "poisoned-bytes": repo.cache[(prior, path)] = value + b"unreviewed"
                elif variant == "missing-bytes": repo.cache[(prior, path)] = None
                elif variant == "mutable-bytes": repo.cache[(prior, path)] = bytearray(value)
                elif variant == "wrong-sha256": digest = "0" * 64
                elif variant == "both-wrong-oid":
                    current_tree = wrong_oid
                    repo._trees[(prior, path)] = wrong_oid
                with mock.patch.object(repo, "_tree", return_value=current_tree) as read:
                    sc._pr18_managed_replication_reuse_before_blob(repo, current, path, digest)
                read.assert_called_once_with(current, path)
                self.assertNotIn((current, path), repo.cache)
                self.assertEqual(repo.total, len(value))
        # An existing current row is never overwritten as a substitute for the
        # original validator, which still rejects a corrupted current value.
        repo, prior, current, path, value, tree = self.fixture()
        repo.cache[(current, path)] = value + b"unreviewed"
        repo._trees[(current, path)] = tree
        with mock.patch.object(repo, "_tree", return_value=tree) as read:
            sc._pr18_managed_replication_reuse_before_blob(
                repo, current, path, hashlib.sha256(value).hexdigest())
            read.assert_not_called()
        with self.assertRaises(sc._Invalid):
            sc._pr18_run_metadata_blob(repo, current, path, hashlib.sha256(value).hexdigest())

    def test_per_blob_and_aggregate_bounds_still_refuse_without_cache_entry(self):
        for bound in ("blob", "aggregate"):
            with self.subTest(bound=bound):
                repo, prior, current, path, value, tree = self.fixture()
                if bound == "aggregate": repo.total = 64 * 1024 * 1024
                limit = len(value) - 1 if bound == "blob" else sc.MAX_ARTIFACT_BYTES
                with mock.patch.object(repo, "_tree", return_value=tree), \
                        mock.patch.object(sc, "MAX_ARTIFACT_BYTES", limit):
                    with self.assertRaises(sc._Invalid):
                        sc._pr18_managed_replication_reuse_before_blob(
                            repo, current, path, hashlib.sha256(value).hexdigest())
                self.assertNotIn((current, path), repo.cache)

    def test_before_profile_preserves_active_reader_failure_before_reuse(self):
        repo, prior, current, path, value, tree = self.fixture()
        for code in ("git-failed:source-budget", "git-failed:source-closed"):
            with self.subTest(code=code), \
                    mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal(code)), \
                    mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_managed_replication_before(repo, current)
            reuse.assert_not_called()
            self.assertNotIn((current, path), repo.cache)



class ManagedReplicationProtectedHistoryReuseTests(unittest.TestCase):
    def repository(self, raw=None):
        repo = sc._Repository(Path("/synthetic-repository"))
        base, middle, head = "a" * 40, "b" * 40, "c" * 40
        repo._run_metadata_history_reads = {(base, head):
            ((middle + "\n" + head + "\n").encode() if raw is None else raw)}
        return repo, base, middle, head

    def test_only_six_protected_paths_use_retained_history_after_marker_proof(self):
        repo, base, middle, head = self.repository()
        events = []
        with mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(repo, "_tree", return_value=b"present"), \
                mock.patch.object(sc, "_pr18_managed_replication_blob",
                    side_effect=lambda *args: events.append(("marker", args[1:]))) as marker, \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob",
                    side_effect=lambda _repo, revision, path, digest: events.append((revision, path, digest))):
            sc._pr18_managed_replication_prime_protected(repo, base, head)
        git.assert_called_once_with(repo.root, ["cat-file", "-t", base])
        marker.assert_called_once_with(repo, head, sc._PR18_MANAGED_REPLICATION_MARKER,
            sc._PR18_MANAGED_REPLICATION_FILES[sc._PR18_MANAGED_REPLICATION_MARKER][1])
        self.assertEqual(events[0][0], "marker")
        self.assertEqual(events[1:], [(revision, path, digest)
            for revision in (base, middle, head) for path, digest in sc._PR18_OBSERVER_PROTECTED.items()])
        self.assertEqual(len(sc._PR18_OBSERVER_PROTECTED), 6)
        self.assertNotIn(sc.DORMANT, sc._PR18_OBSERVER_PROTECTED)

    def test_missing_or_invalid_history_never_traverses_or_primes(self):
        repo, base, middle, head = self.repository()
        variants = (b"", b"x" * (sc.MAX_HISTORY_COMMITS * 41 + 1),
                    (middle + "\n" + middle + "\n" + head + "\n").encode(),
                    (base + "\n" + head + "\n").encode(), (middle + "\n").encode(),
                    b"invalid-oid\n", "not-bytes")
        with mock.patch.object(sc, "_git", return_value=b"commit\n") as git, \
                mock.patch.object(repo, "_tree") as tree, \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
            repo._run_metadata_history_reads = {}
            sc._pr18_managed_replication_prime_protected(repo, base, head)
            for raw in variants:
                with self.subTest(raw_kind=type(raw).__name__, size=len(raw)):
                    repo._run_metadata_history_reads = {(base, head): raw}
                    with self.assertRaises(sc._Invalid):
                        sc._pr18_managed_replication_prime_protected(repo, base, head)
            self.assertTrue(all(call.args[1] == ["cat-file", "-t", base] for call in git.call_args_list))
            tree.assert_not_called(); reuse.assert_not_called()

    def test_absent_or_wrong_marker_and_reader_failure_never_prime(self):
        repo, base, middle, head = self.repository()
        with mock.patch.object(sc, "_git", return_value=b"commit\n"), \
                mock.patch.object(repo, "_tree", return_value=b""), \
                mock.patch.object(sc, "_pr18_managed_replication_blob") as marker, \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
            sc._pr18_managed_replication_prime_protected(repo, base, head)
            marker.assert_not_called(); reuse.assert_not_called()
        with mock.patch.object(sc, "_git", return_value=b"commit\n"), \
                mock.patch.object(repo, "_tree", return_value=b"present"), \
                mock.patch.object(sc, "_pr18_managed_replication_blob", side_effect=sc._Invalid), \
                mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
            with self.assertRaises(sc._Invalid):
                sc._pr18_managed_replication_prime_protected(repo, base, head)
            reuse.assert_not_called()
        for code in ("git-failed:source-closed", "git-failed:source-deadline"):
            with self.subTest(code=code), \
                    mock.patch.object(sc, "_git", side_effect=source_git.SourceGitRefusal(code)), \
                    mock.patch.object(repo, "_tree") as tree, \
                    mock.patch.object(sc, "_pr18_managed_replication_reuse_before_blob") as reuse:
                with self.assertRaises(source_git.SourceGitRefusal):
                    sc._pr18_managed_replication_prime_protected(repo, base, head)
                tree.assert_not_called(); reuse.assert_not_called()

if __name__ == "__main__":
    unittest.main()
