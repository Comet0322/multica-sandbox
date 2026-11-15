package tasksource_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The task source talks to Multica and to the core; it never reaches the
// pods' side (the sandbox service, the pool) except through what the
// manager wires in.
func TestTheSourceImportsNeitherThePoolNorTheSandboxService(t *testing.T) {
	const self = "sandbox-manager/internal/multica/tasksource"
	out, err := exec.Command("go", "list", "-deps", self).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	allowed := map[string]bool{
		self: true, "sandbox-manager/internal/core": true, "sandbox-manager/internal/policy": true,
		"sandbox-manager/internal/multica": true, "sandbox-manager/internal/multica/daemonwire": true,
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, "sandbox-manager/") && !allowed[dep] {
			t.Errorf("the task source depends on %s", dep)
		}
	}
}
