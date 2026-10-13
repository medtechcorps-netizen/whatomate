package dbcatalog

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func planTestInput(t *testing.T) PlanInput {
	t.Helper()
	steps, err := decodePlanSteps(builtinPlanSteps)
	if err != nil {
		t.Fatal(err)
	}
	source := PlanSource{catalog: testCatalog(), steps: steps, unknown: []UnknownCount{}, valid: true}
	counts := map[string]int64{}
	for _, key := range countKeys {
		counts[key] = 0
	}
	snapshot, err := project(source.CaptureCatalog().Objects, source.CaptureCatalog(), Ownership{true, true}, counts)
	if err != nil {
		t.Fatal(err)
	}
	input := PlanInput{Source: source, Snapshot: snapshot}
	for i, id := range planSeedIDs() {
		input.Seeds[i] = PlanSeedObservation{ID: id, State: SeedClear}
	}
	return input
}

func planTestRehash(t *testing.T, s *Snapshot) {
	t.Helper()
	// Recompute even malformed input bytes so the negative cases exercise more
	// than an old digest. Plan must independently validate the complete grammar.
	raw, err := json.Marshal(Catalog{Version: s.Version, Objects: sortedObjects(s.Objects)})
	if err != nil {
		t.Fatal(err)
	}
	s.SHA256 = digest(append(raw, '\n'))
}

func TestPlanSeparatesKnownUnknownAndSeedDriftWithoutLedger(t *testing.T) {
	input := planTestInput(t)
	result, err := Plan(context.Background(), input)
	if err != nil || result.CatalogVersion != Version || result.LiveSHA256 != input.Snapshot.SHA256 || len(result.KnownDrift)+len(result.UnknownExcess)+len(result.PendingIDs)+len(result.SeedDriftIDs)+len(result.Incomplete) != 0 {
		t.Fatal(result, err)
	}
	input.Snapshot.Objects[0].Attributes["force_rls"] = "false"
	planTestRehash(t, &input.Snapshot)
	input.Snapshot.Unknown = []UnknownCount{{Kind: "columns", Parent: "organizations", Count: 2}, {Kind: "relations", Count: 1}}
	input.Seeds[0].State = SeedDrift
	result, err = Plan(context.Background(), input)
	if err != nil || !reflect.DeepEqual(result.KnownDrift, []Difference{{"relations", "organizations", "changed"}}) || len(result.UnknownExcess) != 2 || !reflect.DeepEqual(result.SeedDriftIDs, []string{"SeedPermissionsAndRoles"}) || len(result.PendingIDs) != 0 || len(result.Incomplete) != 0 {
		t.Fatal(result, err)
	}
	input.Snapshot.Objects[0].Missing = true
	input.Snapshot.Objects[0].Attributes = map[string]string{}
	planTestRehash(t, &input.Snapshot)
	result, err = Plan(context.Background(), input)
	if err != nil || result.KnownDrift[0].Status != "missing" || len(result.PendingIDs) != 0 {
		t.Fatal(result, err)
	}
}

func TestPlanUnknownBucketsNeverCancelAndSchemaThresholdIsSourceSpecific(t *testing.T) {
	input := planTestInput(t)
	input.Source.unknown = []UnknownCount{{Kind: "columns", Parent: "organizations", Count: 3}, {Kind: "relations", Count: 1}}
	input.Snapshot.Unknown = []UnknownCount{{Kind: "columns", Parent: "organizations", Count: 1}, {Kind: "relations", Count: 2}}
	input.Snapshot.CountOnly["non_public_schemas"] = 10
	result, err := Plan(context.Background(), input)
	if err != nil || !reflect.DeepEqual(result.UnknownExcess, []UnknownCount{{Kind: "relations", Count: 1}}) || result.OtherSchemaExcess != 0 {
		t.Fatal(result, err)
	}
	threshold := int64(7)
	input.Source.otherSchemas = &threshold
	result, err = Plan(context.Background(), input)
	if err != nil || result.OtherSchemaExcess != 3 {
		t.Fatal(result, err)
	}
	input.Snapshot.CountOnly["non_public_schemas"] = 6
	result, err = Plan(context.Background(), input)
	if err != nil || result.OtherSchemaExcess != 0 {
		t.Fatal(result, err)
	}
}

func TestPlanReportsHybridKnownDriftAgainstOneSelectedSource(t *testing.T) {
	production, err := BuiltinProductionPlanInputs()
	if err != nil {
		t.Fatal(err)
	}
	golden, err := BuiltinGoldenPlanInputs()
	if err != nil {
		t.Fatal(err)
	}
	actual := production.CaptureCatalog()
	changed := false
	normal := golden.CaptureCatalog()
	for i := range actual.Objects {
		if !reflect.DeepEqual(actual.Objects[i].Attributes, normal.Objects[i].Attributes) {
			actual.Objects[i] = normal.Objects[i]
			changed = true
			break
		}
	}
	if !changed {
		t.Fatal("fixture lacks reviewed production differences")
	}
	counts := map[string]int64{}
	for _, key := range countKeys {
		counts[key] = 0
	}
	counts["non_public_schemas"] = 7
	snapshot, err := project(actual.Objects, production.CaptureCatalog(), Ownership{true, true}, counts)
	if err != nil {
		t.Fatal(err)
	}
	input := PlanInput{Source: production, Snapshot: snapshot}
	for i, id := range planSeedIDs() {
		input.Seeds[i] = PlanSeedObservation{ID: id, State: SeedClear}
	}
	result, err := Plan(context.Background(), input)
	if err != nil || len(result.KnownDrift) != 1 || len(result.Incomplete) != 0 {
		t.Fatal("hybrid drift was hidden by equality admission", result, err)
	}
}

func TestPlanRejectsIncompleteOrForgedSnapshotAndCounts(t *testing.T) {
	edits := map[string]func(*PlanInput){
		"zero source":       func(in *PlanInput) { in.Source = PlanSource{} },
		"wrong hash":        func(in *PlanInput) { in.Snapshot.SHA256 = strings.Repeat("0", 64) },
		"omitted known":     func(in *PlanInput) { in.Snapshot.Objects = nil },
		"duplicate known":   func(in *PlanInput) { in.Snapshot.Objects = append(in.Snapshot.Objects, in.Snapshot.Objects[0]) },
		"substituted known": func(in *PlanInput) { in.Snapshot.Objects[0].Identity = "another" },
		"parent changed": func(in *PlanInput) {
			in.Snapshot.Objects[0].Identity = "organizations.other"
			in.Snapshot.Objects[0].Parent = "organizations"
		},
		"missing attributes":      func(in *PlanInput) { in.Snapshot.Objects[0].Attributes = map[string]string{} },
		"private attribute":       func(in *PlanInput) { in.Snapshot.Objects[0].Attributes["owner"] = "private@example.com" },
		"missing with attributes": func(in *PlanInput) { in.Snapshot.Objects[0].Missing = true },
		"version":                 func(in *PlanInput) { in.Snapshot.Version++ },
		"old seed digests": func(in *PlanInput) {
			in.Snapshot.Seeds = []Seed{{Table: "permissions", Count: 1, KeysSHA256: strings.Repeat("0", 64)}}
		},
		"missing count":  func(in *PlanInput) { delete(in.Snapshot.CountOnly, "oids") },
		"extra count":    func(in *PlanInput) { in.Snapshot.CountOnly["private"] = 1 },
		"negative count": func(in *PlanInput) { in.Snapshot.CountOnly["oids"] = -1 },
		"unknown kind":   func(in *PlanInput) { in.Snapshot.Unknown = []UnknownCount{{Kind: "private", Count: 1}} },
		"unknown private parent": func(in *PlanInput) {
			in.Snapshot.Unknown = []UnknownCount{{Kind: "columns", Parent: "unrecognized", Count: 1}}
		},
		"negative bucket":        func(in *PlanInput) { in.Snapshot.Unknown = []UnknownCount{{Kind: "relations", Count: -1}} },
		"over cap bucket":        func(in *PlanInput) { in.Snapshot.Unknown = []UnknownCount{{Kind: "relations", Count: maxObjects + 1}} },
		"known plus unknown cap": func(in *PlanInput) { in.Snapshot.Unknown = []UnknownCount{{Kind: "relations", Count: maxObjects}} },
		"over cap total": func(in *PlanInput) {
			in.Snapshot.Unknown = []UnknownCount{{Kind: "relations", Count: maxObjects}, {Kind: "columns", Count: 1}}
		},
		"duplicate bucket": func(in *PlanInput) {
			in.Snapshot.Unknown = []UnknownCount{{Kind: "relations", Count: 1}, {Kind: "relations", Count: 1}}
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			input := planTestInput(t)
			edit(&input)
			if name != "wrong hash" {
				planTestRehash(t, &input.Snapshot)
			}
			result, err := Plan(context.Background(), input)
			if err == nil || !reflect.DeepEqual(result, PlanResult{}) {
				t.Fatal("malformed facts yielded a plan", result, err)
			}
		})
	}
}

func TestPlanSeedCoverageCodesAndParentTriStateAreClosed(t *testing.T) {
	edits := map[string]func(*PlanInput){
		"missing":               func(in *PlanInput) { in.Seeds[0] = PlanSeedObservation{} },
		"duplicate":             func(in *PlanInput) { in.Seeds[9] = in.Seeds[0] },
		"order":                 func(in *PlanInput) { in.Seeds[0], in.Seeds[1] = in.Seeds[1], in.Seeds[0] },
		"invalid state":         func(in *PlanInput) { in.Seeds[0].State = SeedState(255) },
		"clear code":            func(in *PlanInput) { in.Seeds[0].Code = "unavailable" },
		"drift code":            func(in *PlanInput) { in.Seeds[0].State = SeedDrift; in.Seeds[0].Code = "conflict" },
		"incomplete no code":    func(in *PlanInput) { in.Seeds[0].State = SeedIncomplete },
		"raw error code":        func(in *PlanInput) { in.Seeds[0].State = SeedIncomplete; in.Seeds[0].Code = "private driver error" },
		"historical drift":      func(in *PlanInput) { in.Seeds[6].State = SeedDrift; in.Seeds[1].State = SeedDrift },
		"parent omits drift":    func(in *PlanInput) { in.Seeds[2].State = SeedDrift },
		"parent spurious drift": func(in *PlanInput) { in.Seeds[1].State = SeedDrift },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			input := planTestInput(t)
			edit(&input)
			_, err := Plan(context.Background(), input)
			requireError(t, err, "plan-seeds")
		})
	}
	input := planTestInput(t)
	input.Seeds[1].State = SeedDrift
	input.Seeds[2].State = SeedDrift
	input.Seeds[3].State = SeedIncomplete
	input.Seeds[3].Code = "conflict"
	result, err := Plan(context.Background(), input)
	if err != nil || len(result.SeedDriftIDs) != 2 || !reflect.DeepEqual(result.Incomplete, []PlanIssue{{input.Seeds[3].ID, "conflict"}}) {
		t.Fatal("drift must survive another incomplete child", result, err)
	}
	input.Seeds[2].State = SeedClear
	input.Seeds[1].State = SeedIncomplete
	input.Seeds[1].Code = "unavailable"
	result, err = Plan(context.Background(), input)
	if err != nil || len(result.SeedDriftIDs) != 0 || len(result.Incomplete) != 2 {
		t.Fatal(result, err)
	}
	input.Seeds[1].Code = "conflict"
	_, err = Plan(context.Background(), input)
	requireError(t, err, "plan-seeds")
}

func TestPlanOwnershipAndContextNeverBecomeClean(t *testing.T) {
	input := planTestInput(t)
	input.Snapshot.Ownership.MigrationOwnerOwnsDatabase = false
	result, err := Plan(context.Background(), input)
	if err != nil || !reflect.DeepEqual(result.Incomplete, []PlanIssue{{"catalog", "ownership"}}) {
		t.Fatal(result, err)
	}
	_, err = Plan(nil, input) //nolint:staticcheck // Exercise the explicit nil-context refusal.
	requireError(t, err, "plan-context")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Plan(ctx, input)
	requireError(t, err, "plan-cancelled")
	// Cancellation while processing a larger bounded input is cooperative, not
	// only an entry check. This context deterministically flips after checkpoints.
	ctx2 := &planCancellingContext{Context: context.Background(), remaining: 5}
	_, err = Plan(ctx2, input)
	requireError(t, err, "plan-cancelled")
}

type planCancellingContext struct {
	context.Context
	remaining int
}

func (c *planCancellingContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestPlanSyntheticAddedKnownFieldRemainsMissingNotPending(t *testing.T) {
	input := planTestInput(t)
	added := Object{Kind: "columns", Identity: "organizations.plan_marker", Parent: "organizations", Attributes: map[string]string{
		"position": "2", "type_sha256": digest([]byte("boolean")), "not_null": "false", "default_sha256": digest(nil), "identity": "", "generated": "", "collation_sha256": digest(nil), "acl": "",
	}}
	input.Source.catalog.Objects = append(input.Source.catalog.Objects, added)
	snapshot, err := project([]Object{testObject()}, input.Source.CaptureCatalog(), Ownership{true, true}, input.Snapshot.CountOnly)
	if err != nil {
		t.Fatal(err)
	}
	input.Snapshot = snapshot
	result, err := Plan(context.Background(), input)
	if err != nil || !reflect.DeepEqual(result.KnownDrift, []Difference{{"columns", "organizations.plan_marker", "missing"}}) || len(result.PendingIDs) != 0 {
		t.Fatal(result, err)
	}
}
