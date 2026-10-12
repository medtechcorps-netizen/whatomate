package dbcatalog

// This fixed read-only baseline is independent source material. It does not
// render a migration, explain a transition, or authenticate a caller's source.
import (
	"bytes"
	"context"
	_ "embed"
	"reflect"
	"time"
)

const nullableBuiltinCatalogSHA256 = "1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7"

//go:embed profiles/nullable-column-v1/catalog-v0.json
var nullableBuiltinCatalog []byte

//go:embed profiles/nullable-column-v1/probe-operation.json
var nullableBuiltinOperation []byte

func nullableBuiltinInputs(catalogRaw, operationRaw []byte) (Catalog, NullableColumnOperationV1, error) {
	if len(catalogRaw) == 0 || len(catalogRaw) > nullableObservationLimit || digest(catalogRaw) != nullableBuiltinCatalogSHA256 {
		return Catalog{}, NullableColumnOperationV1{}, refuse("nullable-baseline-source")
	}
	catalog, err := LoadCatalog(catalogRaw)
	if err != nil {
		return Catalog{}, NullableColumnOperationV1{}, refuse("nullable-baseline-source")
	}
	wire, err := CanonicalCatalog(catalog)
	if err != nil || !bytes.Equal(wire, catalogRaw) {
		return Catalog{}, NullableColumnOperationV1{}, refuse("nullable-baseline-source")
	}
	operation, err := ParseNullableColumnOperationV1(operationRaw)
	if err != nil || operation.value != (nullableOperation{1, "add_extra", "add_nullable_column", "organizations", "extra", "pg_catalog.int8", true, nil}) {
		return Catalog{}, NullableColumnOperationV1{}, refuse("nullable-baseline-source")
	}
	return catalog, operation, nil
}

func validateNullableBuiltinBaseline(observation NullableObservationV2, catalog Catalog, operation NullableColumnOperationV1) error {
	if err := validateNullableObservationV2(observation.value); err != nil {
		return err
	}
	expected, err := CanonicalCatalog(catalog)
	if err != nil || digest(expected) != nullableBuiltinCatalogSHA256 || operation.value.Default != nil || operation.value != (nullableOperation{1, "add_extra", "add_nullable_column", "organizations", "extra", "pg_catalog.int8", true, nil}) {
		return refuse("nullable-baseline-source")
	}
	actual, err := CanonicalCatalog(Catalog{Version: observation.value.Snapshot.Version, Objects: observation.value.Snapshot.Objects})
	if err != nil || !bytes.Equal(actual, expected) || observation.value.Graph.Relation.Object != (nullableRef{Kind: "relations", Identity: "organizations"}) {
		return refuse("nullable-baseline-catalog")
	}
	defaults, err := nullableExpectedDefaultRows()
	if err != nil || !reflect.DeepEqual(observation.value.Graph.Security.DefaultACLRows, defaults) {
		return refuse("nullable-default-contract")
	}
	_, err = CanonicalNullableObservationV2(observation)
	return err
}

// CaptureBuiltinNullableV2Baseline observes only the compiled organizations
// probe through the runtime connection in one bounded read-only transaction.
// Complete catalog/default-ACL equality is checked before any observation is
// returned. Source/image/ownership authentication belongs to the owning caller.
func CaptureBuiltinNullableV2Baseline(ctx context.Context, runtimeDSN string, timeout time.Duration) (NullableObservationV2, error) {
	catalog, operation, err := nullableBuiltinInputs(nullableBuiltinCatalog, nullableBuiltinOperation)
	if err != nil {
		return NullableObservationV2{}, err
	}
	globals, err := BuiltinGlobalTables()
	if err != nil {
		return NullableObservationV2{}, err
	}
	observation, err := CaptureNullableGraphV2(ctx, runtimeDSN, Options{
		Catalog: catalog, GlobalTables: globals, Timeout: timeout,
	}, operation)
	if err != nil {
		return NullableObservationV2{}, err
	}
	if err := validateNullableBuiltinBaseline(observation, catalog, operation); err != nil {
		return NullableObservationV2{}, err
	}
	return observation, nil
}
