package core_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The core knows tenants, opaque tasks and pod IDs, nothing of Multica or of
// how pods run: it imports only the policy.
func TestTheCoreImportsOnlyThePolicy(t *testing.T) {
	const self = "sandbox-manager/internal/core"
	out, err := exec.Command("go", "list", "-deps", self).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	allowed := map[string]bool{self: true, "sandbox-manager/internal/policy": true}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, "sandbox-manager/") && !allowed[dep] {
			t.Errorf("the core depends on %s", dep)
		}
	}
}
