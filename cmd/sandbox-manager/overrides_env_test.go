package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSettingsReadsTheOverridesFileOnEveryCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.yaml")
	if err := os.WriteFile(path, []byte("alpha: {quota: 1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := baseEnv()
	env["SANDBOX_WORKSPACE_OVERRIDES_FILE"] = path
	s, err := loadSettings(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	read := s.config.ReadOverrides
	if read == nil {
		t.Fatal("no overrides reader")
	}
	if raw, err := read(); err != nil || string(raw) != "alpha: {quota: 1}\n" {
		t.Fatalf("first read: %q, %v", raw, err)
	}
	if err := os.WriteFile(path, []byte("beta: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err := read(); err != nil || string(raw) != "beta: {}\n" {
		t.Fatalf("an edited file must be read again: %q, %v", raw, err)
	}
}

func TestLoadSettingsWithoutAnOverridesFileHasNone(t *testing.T) {
	s, err := loadSettings(envLookup(baseEnv()))
	if err != nil || s.config.ReadOverrides != nil {
		t.Fatalf("want no overrides reader: %v", err)
	}
}

func TestLoadSettingsRefusesARelativeOverridesFile(t *testing.T) {
	env := baseEnv()
	env["SANDBOX_WORKSPACE_OVERRIDES_FILE"] = "overrides.yaml"
	_, err := loadSettings(envLookup(env))
	if err == nil || !strings.Contains(err.Error(), "SANDBOX_WORKSPACE_OVERRIDES_FILE") {
		t.Fatalf("want the setting named, got %v", err)
	}
}
