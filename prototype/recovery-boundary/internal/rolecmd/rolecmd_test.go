package rolecmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestEveryRoleIsFixedAndInert(t *testing.T) {
	for role, network := range roles {
		t.Run(role, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := Run(role, []string{DescribeArgument}, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			var descriptor Descriptor
			decoder := json.NewDecoder(&stdout)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&descriptor); err != nil {
				t.Fatal(err)
			}
			if !descriptor.PrototypeOnly || descriptor.LiveRuntime || descriptor.Role != role ||
				descriptor.Network != network || descriptor.ClaimScope != "gate-c-nonproduction-only" || stderr.Len() != 0 {
				t.Fatalf("unexpected descriptor: %+v stderr=%q", descriptor, stderr.String())
			}
		})
	}
}

func TestUnknownArgumentsAndRolesFailClosed(t *testing.T) {
	for _, test := range []struct {
		role string
		args []string
	}{
		{"writer-authority", nil},
		{"writer-broker", []string{"--serve"}},
		{"observer-broker", []string{DescribeArgument, "extra"}},
		{"unknown-role", []string{DescribeArgument}},
	} {
		var stdout, stderr bytes.Buffer
		if err := Run(test.role, test.args, &stdout, &stderr); !errors.Is(err, ErrInert) {
			t.Fatalf("role=%q args=%q err=%v", test.role, test.args, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("failed call wrote public output: %q", stdout.String())
		}
	}
}
