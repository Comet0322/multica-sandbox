package manager_test

import (
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
)

func TestOverridesFileFormat(t *testing.T) {
	t.Parallel()
	raw := strings.Join([]string{
		`zero: {hold_timeout: 0, max_lifetime: 0}`,
		`json: {"quota": 2, "hold_timeout": "90s"}`,
		`empty:`,
		`unitless: {hold_timeout: 5}`,
	}, "\n")
	got, problems, err := manager.ParseWorkspaceOverrides([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if o := got["zero"]; o.HoldTimeout == nil || *o.HoldTimeout != 0 || o.MaxLifetime == nil || *o.MaxLifetime != 0 {
		t.Errorf("a bare 0 should mean zero, as in the environment: %+v", o)
	}
	if o := got["json"]; o.Quota == nil || *o.Quota != 2 || o.HoldTimeout == nil || *o.HoldTimeout != 90*time.Second {
		t.Errorf("JSON entry: %+v", o)
	}
	if o, ok := got["empty"]; !ok || o != (manager.WorkspaceOverride{}) {
		t.Errorf("an empty entry overrides nothing: %+v %v", o, ok)
	}
	if err := problems["unitless"]; err == nil || !strings.Contains(err.Error(), "missing unit") || strings.Contains(err.Error(), "\n") {
		t.Errorf("want a one-line missing-unit problem, got %v", err)
	}
}
