package server_test

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"
)

// module is the import path of the module; its packages live under the
// module root, two directories above this one.
const module = "github.com/Teamtem-dev/kuben/"

// TestTheServerDoesNotDependOnTheCLI keeps the server free of internal/cli:
// what both need lives in internal/maintenance, internal/install and the like, and
// the CLI only parses flags and prints. It walks the non-test imports of
// this package transitively within the module, from the source on disk, so
// it needs neither the network nor `go list`.
func TestTheServerDoesNotDependOnTheCLI(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	start := module + "internal/server"
	via := map[string]string{start: ""}
	queue := []string{start}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		pkg, err := build.Default.ImportDir(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, module))), 0)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, imp := range pkg.Imports {
			if !strings.HasPrefix(imp, module) {
				continue
			}
			if _, seen := via[imp]; seen {
				continue
			}
			via[imp] = path
			if strings.HasPrefix(imp, module+"internal/cli") {
				t.Errorf("internal/server depends on %s (imported by %s)", imp, path)
				continue
			}
			queue = append(queue, imp)
		}
	}
	if len(via) < 10 {
		t.Fatalf("only %d packages reached: the walk is broken", len(via))
	}
}
