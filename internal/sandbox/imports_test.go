package sandbox_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The service must be able to leave the manager's process unchanged: it
// depends on the standard library and generic helpers only, never on the
// dispatcher, the lending policy, the daemon wire types or Multica.
func TestThePackageImportsNothingOfTheManagerOrMultica(t *testing.T) {
	const self = "sandbox-manager/internal/sandbox"
	out, err := exec.Command("go", "list", "-deps", self).CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 2 {
		t.Fatalf("go list listed too little: %q", out)
	}
	for _, dep := range deps {
		if dep == self {
			continue
		}
		if strings.HasPrefix(dep, "sandbox-manager/") || strings.HasPrefix(dep, "github.com/multica-ai/") {
			t.Errorf("the sandbox package depends on %s", dep)
		}
	}
}
