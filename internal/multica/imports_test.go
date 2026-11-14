package multica_test

import (
	"os/exec"
	"strings"
	"testing"
)

// Package multica is what the manager knows of Multica, and nothing else: it
// may use the lending policy's types, never the manager or the sandbox
// service.
func TestThePackageImportsOnlyTheLendingPolicy(t *testing.T) {
	const self = "sandbox-manager/internal/multica"
	out, err := exec.Command("go", "list", "-deps", self).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	allowed := map[string]bool{self: true, self + "/daemonwire": true, "sandbox-manager/internal/lending": true}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, "sandbox-manager/") && !allowed[dep] {
			t.Errorf("package multica depends on %s", dep)
		}
	}
}
