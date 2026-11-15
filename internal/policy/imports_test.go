package policy_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The policy is pure and knows nothing of where tasks come from or
// where pods run: it imports nothing else of this module.
func TestThePolicyImportsNothingElseOfTheModule(t *testing.T) {
	const self = "sandbox-manager/internal/policy"
	out, err := exec.Command("go", "list", "-deps", self).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep != self && strings.HasPrefix(dep, "sandbox-manager/") {
			t.Errorf("the policy depends on %s", dep)
		}
	}
}
