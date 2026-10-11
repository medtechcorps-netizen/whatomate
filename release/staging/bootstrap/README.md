# Staging production-shape bootstrap

This standalone staging/test tool builds an **empty** database using the reviewed
public production profile from `internal/dbcatalog/golden/production-v0.json` and
the overlay from `internal/dbcatalog/shape/production-v0.sql`. It has no production
connection and is not imported by any production binary.

The image carries these two authoritative files in `/production-shape`. A binary
built outside the image requires the same `production-shape` directory beside
the executable; the Test workflow supplies it. The command has no profile path
flag or environment override. Both files have fixed SHA-256 checks and bounded
reads, and any missing or changed input refuses before the first database write.

After the existing environment, role, ownership, membership, replication,
default-privilege, advisory-lock and empty-public-schema checks, the tool builds
all eight table precreation plans from the current GORM model definitions. The
profile supplies only known column names and physical order. Types, defaults and
primary keys come from the models. The complete plan generator is checked against
the reviewed PR4 test exporter. Namespace-dependent objects such as a preexisting
public collation also refuse before mutation.

Precreation runs before the unchanged legacy-table, migration, seed, backfill and
tenant-RLS sequence. The reviewed v0 overlay then runs as an explicit comments-only
no-op. No arbitrary overlay SQL is accepted. Current source seeds are retained;
the production seed-data observation is not used to delete a source permission.
Runtime-role tenant-RLS verification remains mandatory.

The synthetic integration test executes the actual standalone binary, verifies
the observed known-catalog fingerprint with no unknown public objects, checks the
source permission count, reapplies the no-op twice, exercises the production RLS
coordinator with read-only startup connections, and verifies that a second
bootstrap refuses without changing the result. Original authority/refusal tests
remain required. These tests use only uniquely named disposable databases and
roles on `TEST_DATABASE_URL`.

This change does not retrofit an existing staging database, change production
migration code, or relax the migration closure contract. Adopting the profile in
an existing staging environment requires a separately reviewed signed bootstrap
image and the existing authorized empty-database setup/reset procedure. No image
build, publication, staging reset or production mutation is performed by these
source tests.
