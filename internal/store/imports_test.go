package store_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The store keeps the core's rows and knows nothing of Multica or of how
// pods run: of this module it imports only the core and the policy.
func TestTheStoreImportsOnlyTheCoreAndThePolicy(t *testing.T) {
	const self = "sandbox-manager/internal/store"
	out, err := exec.Command("go", "list", "-deps", self).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	allowed := map[string]bool{
		self:                              true,
		"sandbox-manager/internal/core":   true,
		"sandbox-manager/internal/policy": true,
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, "sandbox-manager/") && !allowed[dep] {
			t.Errorf("the store depends on %s", dep)
		}
	}
}
