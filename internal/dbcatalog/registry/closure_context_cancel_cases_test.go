package registry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClosureContextCancelTerminals(t *testing.T) {
	program, err := loadClosureProgram(context.Background(), copyClosureFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"Deadline", "Timeout", "Cancel", "VarTuple", "NestedCall", "NestedTuple",
		"DistinctLocal", "MethodWrapper", "DirectWrapper",
	} {
		t.Run(name, func(t *testing.T) {
			result, err := program.closure("closurefixture.test/cancellation." + name)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(result.Declarations, "\n"), "#import:cancellation/valid.go:context") {
				t.Fatal("context import declaration lost")
			}
		})
	}
}

func TestClosureContextCancelRejectsUnprovenValues(t *testing.T) {
	program, err := loadClosureProgram(context.Background(), copyClosureFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"Reassigned", "NestedReassigned", "AddressEscape", "NestedAddressEscape",
		"Alias", "ReturnValue", "StoredValue", "CallbackEscape", "InitializedLater",
		"WrongTupleSlot", "FactoryAlias", "WrongFactory", "ShadowedFactory",
		"NamedWrapper", "MixedWrapper", "ChainedWrapper", "IndirectWrapper",
		"NestedWrapper", "InterfaceWrapper", "MethodExpression", "WrapperAlias",
		"RangeReassigned", "OtherFunctionType",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := program.closure("closurefixture.test/cancellation." + name); err == nil {
				t.Fatal("unproven cancellation value accepted")
			}
		})
	}
}

func TestClosureContextCancelWrapperBodyRemainsHashed(t *testing.T) {
	root := copyClosureFixture(t)
	program, err := loadClosureProgram(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	before, err := program.closure("closurefixture.test/cancellation.MethodWrapper")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(before.Declarations, "\n"), "closurefixture.test/cancellation.DeadlineValue.cleanup") {
		t.Fatal("proven wrapper declaration lost")
	}
	path := filepath.Join(root, "cancellation", "valid.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "2 * time.Second") != 1 {
		t.Fatal("fixture preimage changed")
	}
	raw = []byte(strings.Replace(string(raw), "2 * time.Second", "3 * time.Second", 1))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	program, err = loadClosureProgram(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	after, err := program.closure("closurefixture.test/cancellation.MethodWrapper")
	if err != nil {
		t.Fatal(err)
	}
	if after.SHA256 == before.SHA256 {
		t.Fatal("wrapper body change did not change closure")
	}
}
