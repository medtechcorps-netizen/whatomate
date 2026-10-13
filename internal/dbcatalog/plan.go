package dbcatalog

import (
	"context"
	"reflect"
	"sort"
)

// Zero is invalid: an omitted observation can never mean a clean seed.
type SeedState uint8

const (
	SeedInvalid SeedState = iota
	SeedClear
	SeedDrift
	SeedIncomplete
)

type PlanSeedObservation struct {
	ID    string
	State SeedState
	Code  string
}
type PlanInput struct {
	Source   PlanSource
	Snapshot Snapshot
	Seeds    [10]PlanSeedObservation
}
type PlanIssue struct {
	Component string
	Code      string
}
type PlanResult struct {
	CatalogVersion    int
	LiveSHA256        string
	KnownDrift        []Difference
	UnknownExcess     []UnknownCount
	OtherSchemaExcess int64
	PendingIDs        []string
	SeedDriftIDs      []string
	Incomplete        []PlanIssue
}

func planContext(ctx context.Context) error {
	if ctx == nil {
		return refuse("plan-context")
	}
	if ctx.Err() != nil {
		return refuse("plan-cancelled")
	}
	return nil
}

// Plan compares complete observations against one explicitly selected immutable
// source. It has no database, repair, ledger query or executor capability.
func Plan(ctx context.Context, input PlanInput) (PlanResult, error) {
	if err := planContext(ctx); err != nil {
		return PlanResult{}, err
	}
	source := input.Source
	if !source.valid || len(source.catalog.Objects) == 0 || validatePlanSteps(source.steps) != nil || validateCatalog(source.catalog) != nil {
		return PlanResult{}, refuse("plan-source")
	}
	wanted := make(map[string]Object, len(source.catalog.Objects))
	parents := map[string]bool{}
	for _, o := range source.catalog.Objects {
		if err := planContext(ctx); err != nil {
			return PlanResult{}, err
		}
		if o.Missing {
			return PlanResult{}, refuse("plan-source")
		}
		wanted[o.Kind+":"+o.Identity] = o
		if o.Kind == "relations" {
			parents[o.Identity] = true
		}
	}
	snapshot := input.Snapshot
	if snapshot.Version != Version || len(snapshot.Objects) != len(wanted) || len(snapshot.Seeds) != 0 {
		return PlanResult{}, refuse("plan-snapshot")
	}
	if err := validateCatalog(Catalog{Version: snapshot.Version, Objects: snapshot.Objects}); err != nil {
		return PlanResult{}, refuse("plan-snapshot")
	}
	seen := make(map[string]bool, len(snapshot.Objects))
	present := int64(0)
	result := PlanResult{CatalogVersion: Version, LiveSHA256: snapshot.SHA256, KnownDrift: []Difference{}, UnknownExcess: []UnknownCount{}, PendingIDs: []string{}, SeedDriftIDs: []string{}, Incomplete: []PlanIssue{}}
	for _, o := range snapshot.Objects {
		if err := planContext(ctx); err != nil {
			return PlanResult{}, err
		}
		key := o.Kind + ":" + o.Identity
		want, ok := wanted[key]
		if !ok || seen[key] || o.Parent != want.Parent {
			return PlanResult{}, refuse("plan-snapshot")
		}
		seen[key] = true
		if !o.Missing {
			present++
		}
		if o.Missing {
			result.KnownDrift = append(result.KnownDrift, Difference{o.Kind, o.Identity, "missing"})
		} else if !reflect.DeepEqual(o.Attributes, want.Attributes) {
			result.KnownDrift = append(result.KnownDrift, Difference{o.Kind, o.Identity, "changed"})
		}
	}
	canonical, err := CanonicalCatalog(Catalog{Version: snapshot.Version, Objects: snapshot.Objects})
	if err != nil || !shaPattern.MatchString(snapshot.SHA256) || digest(canonical) != snapshot.SHA256 {
		return PlanResult{}, refuse("plan-snapshot")
	}
	if err = planContext(ctx); err != nil {
		return PlanResult{}, err
	}
	if len(snapshot.CountOnly) != len(countKeys) {
		return PlanResult{}, refuse("plan-counts")
	}
	for _, key := range countKeys {
		v, ok := snapshot.CountOnly[key]
		if !ok || v < 0 {
			return PlanResult{}, refuse("plan-counts")
		}
	}
	baseline, err := planUnknownCounts(ctx, source.unknown, parents)
	if err != nil {
		return PlanResult{}, refuse("plan-source")
	}
	actual, err := planUnknownCounts(ctx, snapshot.Unknown, parents)
	if err != nil {
		return PlanResult{}, err
	}
	observedTotal := present
	for _, count := range actual {
		observedTotal += count
	}
	if observedTotal > maxObjects {
		return PlanResult{}, refuse("plan-counts")
	}
	// Compare each bucket, never the sum: one decrease cannot cancel an increase.
	for _, bucket := range snapshot.Unknown {
		if err = planContext(ctx); err != nil {
			return PlanResult{}, err
		}
		key := bucket.Kind + ":" + bucket.Parent
		if excess := actual[key] - baseline[key]; excess > 0 {
			result.UnknownExcess = append(result.UnknownExcess, UnknownCount{bucket.Kind, bucket.Parent, excess})
		}
	}
	if source.otherSchemas != nil {
		if *source.otherSchemas < 0 {
			return PlanResult{}, refuse("plan-source")
		}
		if n := snapshot.CountOnly["non_public_schemas"] - *source.otherSchemas; n > 0 {
			result.OtherSchemaExcess = n
		}
	}
	if !snapshot.Ownership.MigrationOwnerOwnsDatabase || !snapshot.Ownership.MigrationOwnerOwnsPublicSchema {
		result.Incomplete = append(result.Incomplete, PlanIssue{Component: "catalog", Code: "ownership"})
	}
	ids := planSeedIDs()
	for i, row := range input.Seeds {
		if err = planContext(ctx); err != nil {
			return PlanResult{}, err
		}
		if row.ID != ids[i] {
			return PlanResult{}, refuse("plan-seeds")
		}
		switch row.State {
		case SeedClear, SeedDrift:
			if row.Code != "" {
				return PlanResult{}, refuse("plan-seeds")
			}
		case SeedIncomplete:
			if row.Code != "unavailable" && row.Code != "conflict" {
				return PlanResult{}, refuse("plan-seeds")
			}
		default:
			return PlanResult{}, refuse("plan-seeds")
		}
		if row.State == SeedDrift {
			result.SeedDriftIDs = append(result.SeedDriftIDs, row.ID)
		}
		if row.State == SeedIncomplete {
			result.Incomplete = append(result.Incomplete, PlanIssue{Component: row.ID, Code: row.Code})
		}
	}
	if input.Seeds[6].State != SeedClear {
		return PlanResult{}, refuse("plan-seeds")
	}
	parentState := SeedClear
	for _, row := range input.Seeds[2:7] {
		if row.State == SeedDrift {
			parentState = SeedDrift
			break
		}
		if row.State == SeedIncomplete {
			parentState = SeedIncomplete
		}
	}
	if input.Seeds[1].State != parentState || parentState == SeedIncomplete && input.Seeds[1].Code != "unavailable" {
		return PlanResult{}, refuse("plan-seeds")
	}
	// All 21 validated compiled declarations are baseline-only. The empty pending
	// list follows that complete declaration; no missing ledger is interpreted.
	sort.Slice(result.KnownDrift, func(i, j int) bool {
		a, b := result.KnownDrift[i], result.KnownDrift[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Identity < b.Identity
	})
	sort.Slice(result.UnknownExcess, func(i, j int) bool {
		a, b := result.UnknownExcess[i], result.UnknownExcess[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Parent < b.Parent
	})
	sort.Strings(result.SeedDriftIDs)
	sort.Slice(result.Incomplete, func(i, j int) bool {
		a, b := result.Incomplete[i], result.Incomplete[j]
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		return a.Code < b.Code
	})
	if err = planContext(ctx); err != nil {
		return PlanResult{}, err
	}
	return result, nil
}

func planUnknownCounts(ctx context.Context, counts []UnknownCount, parents map[string]bool) (map[string]int64, error) {
	if len(counts) > maxObjects {
		return nil, refuse("plan-counts")
	}
	out := make(map[string]int64, len(counts))
	total := int64(0)
	for _, row := range counts {
		if err := planContext(ctx); err != nil {
			return nil, err
		}
		key := row.Kind + ":" + row.Parent
		if _, exists := out[key]; exists || !knownKind(row.Kind) || row.Count < 0 || row.Count > maxObjects || row.Parent != "" && !parents[row.Parent] {
			return nil, refuse("plan-counts")
		}
		total += row.Count
		if total > maxObjects {
			return nil, refuse("plan-counts")
		}
		out[key] = row.Count
	}
	return out, nil
}
