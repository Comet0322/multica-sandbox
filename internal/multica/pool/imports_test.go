package pool_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The pool holds the sandbox service and serves the daemon protocol; it
// reaches the task source only through what the manager wires in.
func TestThePoolDoesNotImportTheTaskSource(t *testing.T) {
	const self = "sandbox-manager/internal/multica/pool"
	out, err := exec.Command("go", "list", "-deps", self).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	allowed := map[string]bool{
		self: true, "sandbox-manager/internal/core": true, "sandbox-manager/internal/lending": true,
		"sandbox-manager/internal/multica": true, "sandbox-manager/internal/multica/daemonwire": true,
		"sandbox-manager/internal/sandbox": true,
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, "sandbox-manager/") && !allowed[dep] {
			t.Errorf("the pool depends on %s", dep)
		}
	}
}
