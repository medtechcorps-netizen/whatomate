package protocol

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestGateAPackageContainsNoNetworkOrProcessClient(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"net": true, "net/http": true, "net/rpc": true, "os/exec": true,
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if forbidden[path] {
				t.Fatalf("Gate-A package file %s imports forbidden client package %s", entry.Name(), path)
			}
		}
	}
}
