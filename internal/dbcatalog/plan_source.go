package dbcatalog

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

const planProfileSHA256 = "808e11a5fdab638adc6f0004e9595371d1eee9a51763e12e779b355f7cdd391d"
const planGoldenSHA256 = "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8"
const planRemovalRationale = "removes-write: legacy admin@admin.com escalation removed at rev 2; retained inert marker bound to parent closure"
const planParentID = "SeedSystemRolesForAllOrgs"
const planLegacyID = planParentID + "/legacy-super-admin-update"

// PlanSource contains only freshly decoded, fixed compiled consistency data.
// It is not execution authority and has no public path/byte-loader override.
type PlanSource struct {
	catalog      Catalog
	globals      []GlobalTable
	steps        []planStep
	unknown      []UnknownCount
	otherSchemas *int64
	valid        bool
}

type planStep struct {
	ID         string   `json:"id"`
	Rev        int      `json:"rev"`
	Func       string   `json:"func"`
	Kind       string   `json:"kind"`
	Baseline   *bool    `json:"baseline"`
	Idempotent *bool    `json:"idempotent"`
	N1Safe     *bool    `json:"n1_safe"`
	Gates      []string `json:"requires_gates"`
	Files      []string `json:"closure_files"`
	SHA256     string   `json:"closure_sha256"`
	Parent     string   `json:"parent,omitempty"`
}

type planDeclaration struct {
	id, function, kind string
	idempotent         bool
	parent             string
}

// Only semantic identities are fixed here. Generated revisions and closure
// hashes remain actual build inputs checked by the source generator, not old
// hardcoded digests or an assumption that the removed pair stays at rev 2.
func planDeclarations() []planDeclaration {
	return []planDeclaration{
		{"PrepareProviderIntegrationManagementMode", "github.com/shridarpatil/whatomate/internal/database.PrepareProviderIntegrationManagementMode", "baseline-pre-ledger", true, ""},
		{"PrepareMessageIngestionOrder", "github.com/shridarpatil/whatomate/internal/database.PrepareMessageIngestionOrder", "baseline-pre-ledger", true, ""},
		{"PrepareMetaInstagramDeletionJournalTenant", "github.com/shridarpatil/whatomate/internal/database.PrepareMetaInstagramDeletionJournalTenant", "baseline-pre-ledger", false, ""},
		{"AutoMigrate", "github.com/shridarpatil/whatomate/internal/database.AutoMigrate", "baseline-pre-ledger", false, ""},
		{"InstallMessageIngestionOrderTrigger", "github.com/shridarpatil/whatomate/internal/database.InstallMessageIngestionOrderTrigger", "baseline-pre-ledger", true, ""},
		{"BackfillProviderIntegrationBindings", "github.com/shridarpatil/whatomate/internal/database.BackfillProviderIntegrationBindings", "baseline-pre-ledger", true, ""},
		{"SeedPermissionsAndRoles", "github.com/shridarpatil/whatomate/internal/database.SeedPermissionsAndRoles", "seed-reconciler", true, ""},
		{"SeedSystemRolesForAllOrgs", "github.com/shridarpatil/whatomate/internal/database.SeedSystemRolesForAllOrgs", "seed-reconciler", false, ""},
		{"SeedSystemRolesForAllOrgs/seedSystemRolesForOrg", "github.com/shridarpatil/whatomate/internal/database.seedSystemRolesForOrg", "seed-reconciler", false, "SeedSystemRolesForAllOrgs"},
		{"SeedSystemRolesForAllOrgs/FixSystemRolePermissions", "github.com/shridarpatil/whatomate/internal/database.FixSystemRolePermissions", "seed-reconciler", true, "SeedSystemRolesForAllOrgs"},
		{"SeedSystemRolesForAllOrgs/ApplyManagerSettingsPolicyMigration", "github.com/shridarpatil/whatomate/internal/database.ApplyManagerSettingsPolicyMigration", "seed-reconciler", true, "SeedSystemRolesForAllOrgs"},
		{"SeedSystemRolesForAllOrgs/MigrateExistingUserRoles", "github.com/shridarpatil/whatomate/internal/database.MigrateExistingUserRoles", "seed-reconciler", true, "SeedSystemRolesForAllOrgs"},
		{"SeedSystemRolesForAllOrgs/legacy-super-admin-update", "github.com/shridarpatil/whatomate/internal/database.SeedSystemRolesForAllOrgs", "seed-reconciler", true, "SeedSystemRolesForAllOrgs"},
		{"MigrateUserOrganizations", "github.com/shridarpatil/whatomate/internal/database.MigrateUserOrganizations", "baseline-pre-ledger", true, ""},
		{"CreateDefaultAdmin", "github.com/shridarpatil/whatomate/internal/database.CreateDefaultAdmin", "baseline-pre-ledger", false, ""},
		{"EnsurePlatformReseller", "github.com/shridarpatil/whatomate/internal/database.EnsurePlatformReseller", "seed-reconciler", false, ""},
		{"EnsureReReplyProductCatalog", "github.com/shridarpatil/whatomate/internal/database.EnsureReReplyProductCatalog", "seed-reconciler", true, ""},
		{"SeedDefaultWidgets", "github.com/shridarpatil/whatomate/internal/database.SeedDefaultWidgets", "seed-reconciler", false, ""},
		{"BackfillLastInboundAt", "github.com/shridarpatil/whatomate/internal/database.BackfillLastInboundAt", "baseline-pre-ledger", true, ""},
		{"BackfillChatbotFlowGraph", "github.com/shridarpatil/whatomate/internal/handlers.BackfillChatbotFlowGraph", "baseline-pre-ledger", false, ""},
		{"BackfillLegacyWhatsAppInbox", "github.com/shridarpatil/whatomate/internal/channel.BackfillLegacyWhatsAppInbox", "baseline-pre-ledger", false, ""},
	}
}

func planSeedIDs() [10]string {
	return [10]string{
		"SeedPermissionsAndRoles", planParentID,
		planParentID + "/seedSystemRolesForOrg", planParentID + "/FixSystemRolePermissions",
		planParentID + "/ApplyManagerSettingsPolicyMigration", planParentID + "/MigrateExistingUserRoles", planLegacyID,
		"EnsurePlatformReseller", "EnsureReReplyProductCatalog", "SeedDefaultWidgets",
	}
}

// CaptureCatalog returns independent backing storage, including attribute maps.
func (source PlanSource) CaptureCatalog() Catalog {
	return copyPlanCatalog(source.catalog)
}

func copyPlanCatalog(c Catalog) Catalog {
	out := Catalog{Version: c.Version, Objects: make([]Object, len(c.Objects))}
	for i, o := range c.Objects {
		out.Objects[i] = o
		if o.Attributes != nil {
			out.Objects[i].Attributes = make(map[string]string, len(o.Attributes))
			for k, v := range o.Attributes {
				out.Objects[i].Attributes[k] = v
			}
		}
	}
	return out
}

// BuiltinProductionPlanInputs selects production-v0 before observation, without
// asking which source best matches the live database or invoking nullable proof.
func BuiltinProductionPlanInputs() (PlanSource, error) {
	return decodePlanSource(builtinCatalog, nullableBuiltinCatalog, bootstrapEmptyProductionV0, builtinGlobals, builtinHistory, builtinPlanSteps, builtinPlanBindings, true)
}

// BuiltinGoldenPlanInputs is the explicit normal synthetic-bootstrap baseline.
func BuiltinGoldenPlanInputs() (PlanSource, error) {
	return decodePlanSource(builtinCatalog, nullableBuiltinCatalog, bootstrapEmptyProductionV0, builtinGlobals, builtinHistory, builtinPlanSteps, builtinPlanBindings, false)
}

func planObject(raw []byte, required []string, optional ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := strictDecode(raw, &fields); err != nil || fields == nil {
		return nil, refuse("plan-source")
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, k := range required {
		allowed[k] = true
		if v, ok := fields[k]; !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, refuse("plan-source")
		}
	}
	for _, k := range optional {
		allowed[k] = true
	}
	for k, v := range fields {
		if !allowed[k] || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, refuse("plan-source")
		}
	}
	return fields, nil
}

func planGoPath(s string) bool {
	if len(s) == 0 || len(s) > 512 || !strings.HasSuffix(s, ".go") || strings.HasSuffix(s, "_test.go") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
		for _, r := range part {
			letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
			digit := r >= '0' && r <= '9'
			if !letter && !digit && r != '_' && r != '-' && r != '.' {
				return false
			}
		}
	}
	return true
}

func validatePlanSteps(steps []planStep) error {
	declarations := planDeclarations()
	if len(steps) != len(declarations) {
		return refuse("plan-registry")
	}
	for i, row := range steps {
		want := declarations[i]
		if row.ID != want.id || row.Func != want.function || row.Kind != want.kind || row.Parent != want.parent || row.Rev < 1 ||
			row.Baseline == nil || !*row.Baseline || row.Idempotent == nil || *row.Idempotent != want.idempotent ||
			row.N1Safe == nil || *row.N1Safe || row.Gates == nil || len(row.Gates) != 0 || !shaPattern.MatchString(row.SHA256) || len(row.Files) == 0 || len(row.Files) > 1000 {
			return refuse("plan-registry")
		}
		previous := ""
		for _, name := range row.Files {
			if !planGoPath(name) || name <= previous {
				return refuse("plan-registry")
			}
			previous = name
		}
	}
	// Both metadata rows bind the same removed writer. A new measured revision
	// pair may advance, but cannot regress to the pre-removal revision or split.
	parent, legacy := steps[7], steps[12]
	if parent.Rev < 2 || parent.Rev != legacy.Rev || parent.SHA256 != legacy.SHA256 || !reflect.DeepEqual(parent.Files, legacy.Files) || parent.Func != legacy.Func {
		return refuse("plan-registry")
	}
	return nil
}

func decodePlanSteps(raw []byte) ([]planStep, error) {
	var rows []json.RawMessage
	if err := strictDecode(raw, &rows); err != nil || len(rows) != 21 {
		return nil, refuse("plan-registry")
	}
	steps := make([]planStep, len(rows))
	required := []string{"id", "rev", "func", "kind", "baseline", "idempotent", "n1_safe", "requires_gates", "closure_files", "closure_sha256"}
	for i, rawRow := range rows {
		fields, err := planObject(rawRow, required, "parent")
		if err != nil || strictDecode(rawRow, &steps[i]) != nil {
			return nil, refuse("plan-registry")
		}
		_, hasParent := fields["parent"]
		if hasParent != (steps[i].Parent != "") {
			return nil, refuse("plan-registry")
		}
	}
	if err := validatePlanSteps(steps); err != nil {
		return nil, err
	}
	return steps, nil
}

func validatePlanBindings(raw []byte) error {
	required := []string{"version", "execution_labels", "model_descriptors", "model_list_sha256", "coordinator_sha256", "coordinator_calls", "seed_child_calls", "seed_child_order", "inline_sql", "callbacks", "callback_calls", "callback_sha256", "idempotency_rationale", "n1_safety_note", "child_semantics"}
	_, err := planObject(raw, required)
	if err != nil {
		return err
	}
	var binding struct {
		Version        int               `json:"version"`
		Labels         []string          `json:"execution_labels"`
		Models         []string          `json:"model_descriptors"`
		ModelsSHA      string            `json:"model_list_sha256"`
		CoordinatorSHA string            `json:"coordinator_sha256"`
		Calls          []string          `json:"coordinator_calls"`
		Children       []string          `json:"seed_child_calls"`
		Order          []string          `json:"seed_child_order"`
		Inline         string            `json:"inline_sql"`
		Callbacks      []string          `json:"callbacks"`
		CallbackCalls  []string          `json:"callback_calls"`
		CallbackSHA    string            `json:"callback_sha256"`
		Rationale      map[string]string `json:"idempotency_rationale"`
		Safety         string            `json:"n1_safety_note"`
		Semantics      string            `json:"child_semantics"`
	}
	if strictDecode(raw, &binding) != nil || binding.Version != 1 || binding.Inline != "" ||
		!shaPattern.MatchString(binding.ModelsSHA) || !shaPattern.MatchString(binding.CoordinatorSHA) || !shaPattern.MatchString(binding.CallbackSHA) ||
		len(binding.Models) == 0 || len(binding.Models) > 1000 || len(binding.Calls) == 0 || len(binding.Calls) > 1000 ||
		len(binding.Rationale) != 21 || binding.Rationale[planLegacyID] != planRemovalRationale ||
		binding.Rationale[planParentID] != "Composite repair includes conditional substeps; no blanket repeat-safety claim" ||
		binding.Semantics != "parent entries execute conceptually once; children are inert source metadata, never additional executable steps" ||
		binding.Safety != "false means not certified by N-1 compatibility proof; it does not assert a demonstrated incompatibility" {
		return refuse("plan-bindings")
	}
	children := []string{"seedSystemRolesForOrg", "FixSystemRolePermissions", "ApplyManagerSettingsPolicyMigration", "MigrateExistingUserRoles"}
	if !reflect.DeepEqual(binding.Children, children) || !reflect.DeepEqual(binding.Order, children) ||
		!reflect.DeepEqual(binding.Callbacks, []string{"handlers.BackfillChatbotFlowGraph", "channelapi.BackfillLegacyWhatsAppInbox"}) {
		return refuse("plan-bindings")
	}
	labels := []string{}
	for _, d := range planDeclarations() {
		if d.parent == "" && d.id != "BackfillChatbotFlowGraph" && d.id != "BackfillLegacyWhatsAppInbox" {
			labels = append(labels, d.id)
		}
		if text, ok := binding.Rationale[d.id]; !ok || len(text) == 0 || len(text) > 2048 {
			return refuse("plan-bindings")
		}
	}
	if !reflect.DeepEqual(binding.Labels, labels) {
		return refuse("plan-bindings")
	}
	for _, list := range [][]string{binding.Models, binding.Calls, binding.CallbackCalls} {
		if len(list) == 0 || len(list) > 1000 {
			return refuse("plan-bindings")
		}
		for _, v := range list {
			if len(v) == 0 || len(v) > 1024 {
				return refuse("plan-bindings")
			}
		}
	}
	return nil
}

func decodePlanSource(goldenRaw, productionRaw, profileRaw, globalsRaw, historyRaw, stepsRaw, bindingsRaw []byte, production bool) (PlanSource, error) {
	raws := [][]byte{goldenRaw, productionRaw, profileRaw, globalsRaw, historyRaw, stepsRaw, bindingsRaw}
	limits := []int{4 << 20, 4 << 20, 64 << 10, 256 << 10, 16 << 10, 256 << 10, 256 << 10}
	total := 0
	for i, raw := range raws {
		if len(raw) == 0 || len(raw) > limits[i] {
			return PlanSource{}, refuse("plan-source")
		}
		total += len(raw)
	}
	if total > 8<<20 || digest(profileRaw) != planProfileSHA256 || digest(goldenRaw) != planGoldenSHA256 || digest(productionRaw) != nullableBuiltinCatalogSHA256 {
		return PlanSource{}, refuse("plan-source")
	}
	if _, err := planObject(profileRaw, []string{"version", "live_sha256", "golden_sha256", "released_source", "delta", "accepted_unpublished", "n_other_schemas", "bootstrap_column_order"}); err != nil {
		return PlanSource{}, err
	}
	var profile struct {
		Version     int               `json:"version"`
		Live        string            `json:"live_sha256"`
		Golden      string            `json:"golden_sha256"`
		Released    string            `json:"released_source"`
		Delta       []json.RawMessage `json:"delta"`
		Unpublished []json.RawMessage `json:"accepted_unpublished"`
		Other       int64             `json:"n_other_schemas"`
		Orders      []json.RawMessage `json:"bootstrap_column_order"`
	}
	if strictDecode(profileRaw, &profile) != nil || profile.Version != Version || profile.Live != nullableBuiltinCatalogSHA256 || profile.Golden != planGoldenSHA256 ||
		profile.Released != "e0016b3595114728e7864da2adb44c29065f25ef" || len(profile.Delta) != 72 || profile.Unpublished == nil || len(profile.Unpublished) != 0 || profile.Other != 7 || len(profile.Orders) != 8 {
		return PlanSource{}, refuse("plan-source")
	}
	catalogs := make([]Catalog, 2)
	for i, raw := range [][]byte{goldenRaw, productionRaw} {
		c, err := LoadCatalog(raw)
		if err != nil || len(c.Objects) == 0 {
			return PlanSource{}, refuse("plan-source")
		}
		canonical, err := CanonicalCatalog(c)
		if err != nil || !bytes.Equal(canonical, raw) {
			return PlanSource{}, refuse("plan-source")
		}
		for _, o := range c.Objects {
			if o.Missing {
				return PlanSource{}, refuse("plan-source")
			}
		}
		catalogs[i] = c
	}
	if len(catalogs[0].Objects) != len(catalogs[1].Objects) {
		return PlanSource{}, refuse("plan-source")
	}
	for i, o := range catalogs[0].Objects {
		p := catalogs[1].Objects[i]
		if o.Kind != p.Kind || o.Identity != p.Identity || o.Parent != p.Parent {
			return PlanSource{}, refuse("plan-source")
		}
	}
	var history []json.RawMessage
	if strictDecode(historyRaw, &history) != nil || len(history) != 1 {
		return PlanSource{}, refuse("plan-source")
	}
	if _, err := planObject(history[0], []string{"version", "class", "min_reader_version"}); err != nil {
		return PlanSource{}, err
	}
	var h struct {
		Version int    `json:"version"`
		Class   string `json:"class"`
		Minimum int    `json:"min_reader_version"`
	}
	if strictDecode(history[0], &h) != nil || h.Version != Version || h.Class != "baseline" || h.Minimum != 0 {
		return PlanSource{}, refuse("plan-source")
	}
	globals, err := LoadGlobalTables(globalsRaw)
	if err != nil || globals == nil {
		return PlanSource{}, refuse("plan-source")
	}
	var globalRows []json.RawMessage
	if strictDecode(globalsRaw, &globalRows) != nil {
		return PlanSource{}, refuse("plan-source")
	}
	for _, row := range globalRows {
		if _, err := planObject(row, []string{"name", "reason"}, "keys"); err != nil {
			return PlanSource{}, err
		}
	}
	steps, err := decodePlanSteps(stepsRaw)
	if err != nil {
		return PlanSource{}, err
	}
	if err = validatePlanBindings(bindingsRaw); err != nil {
		return PlanSource{}, err
	}
	source := PlanSource{catalog: catalogs[0], globals: globals, steps: steps, unknown: []UnknownCount{}, valid: true}
	if production {
		source.catalog = catalogs[1]
		threshold := profile.Other
		source.otherSchemas = &threshold
	}
	return source, nil
}
