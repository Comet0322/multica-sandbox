package module_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The module stays apart from Multica's: it must not require any Multica
// module, directly or indirectly.
func TestTheModuleRequiresNoMulticaModule(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "all").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -m all: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 || lines[0] != "sandbox-manager" {
		t.Fatalf("go list -m all listed an unexpected module set: %q", out)
	}
	for _, l := range lines {
		if strings.Contains(l, "multica-ai") {
			t.Errorf("the module requires %s", l)
		}
	}
}
