package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"sandbox-manager/internal/manager"
	"sandbox-manager/internal/multica"
	"sandbox-manager/internal/policy"
)

func envLookup(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func baseEnv() map[string]string {
	return map[string]string{
		"SANDBOX_POOL_TOKEN": "mul_pat_pool",
		"MULTICA_SERVER_URL": "http://multica:8080",
	}
}

func TestLoadSettingsReadsPlacementLifetimeAndAudit(t *testing.T) {
	env := baseEnv()
	env["SANDBOX_POOL_HEARTBEAT_TIMEOUT"] = "30s"
	env["SANDBOX_POOL_RESET_TIMEOUT"] = "3m"
	env["SANDBOX_POOL_LOST_RETENTION"] = "7m"
	env["SANDBOX_PLACEMENT"] = "person-agent"
	env["SANDBOX_POD_CAPACITY"] = "8"
	env["SANDBOX_PLACEMENT_WAIT"] = "20s"
	env["SANDBOX_HOLD_TIMEOUT"] = "5m"
	env["SANDBOX_POD_MAX_LIFETIME"] = "1h"
	env["SANDBOX_POD_DRAIN_GRACE"] = "2m"
	env["SANDBOX_POD_MAX_IDLE"] = "6h"
	env["SANDBOX_AUDIT_FILE"] = "/var/log/audit.jsonl"
	s, err := loadSettings(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	c := s.config
	if c.HeartbeatTimeout != 30*time.Second || c.ResetTimeout != 3*time.Minute || c.LostRetention != 7*time.Minute || c.AffinityRule != multica.RulePersonAgent ||
		c.Capacity != 8 || c.PlacementWait != 20*time.Second || c.HoldTimeout != 5*time.Minute ||
		c.MaxLifetime != time.Hour || c.DrainGrace != 2*time.Minute || c.PodMaxIdle != 6*time.Hour || s.auditFile != "/var/log/audit.jsonl" {
		t.Fatalf("unexpected settings: %+v", s)
	}
}

func TestLoadSettingsDefaultsToPersonIssueWithAHold(t *testing.T) {
	env := baseEnv()
	s, err := loadSettings(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if s.config.HoldTimeout != manager.DefaultHoldTimeout || s.config.AffinityRule != "" {
		t.Fatalf("unset: hold %v, placement %q", s.config.HoldTimeout, s.config.AffinityRule)
	}
	env["SANDBOX_HOLD_TIMEOUT"] = "0"
	if s, err = loadSettings(envLookup(env)); err != nil || s.config.HoldTimeout != 0 {
		t.Fatalf("0 must reset at once: %v, %v", s.config.HoldTimeout, err)
	}
}

func TestLoadSettingsReadsTheRuntimeKindsAndTheirNames(t *testing.T) {
	for v, want := range map[string][]policy.Kind{
		"":             {policy.KindPool},
		"pool":         {policy.KindPool},
		"clean":        {policy.KindClean},
		"pool,clean":   {policy.KindPool, policy.KindClean},
		" Clean, pool": {policy.KindClean, policy.KindPool},
	} {
		env := baseEnv()
		env["SANDBOX_RUNTIMES"] = v
		s, err := loadSettings(envLookup(env))
		if err != nil || !slices.Equal(s.config.Runtimes, want) {
			t.Fatalf("%q: %v, %v; want %v", v, s.config.Runtimes, err, want)
		}
	}
	for _, v := range []string{"pool,pool", "dirty"} {
		env := baseEnv()
		env["SANDBOX_RUNTIMES"] = v
		if _, err := loadSettings(envLookup(env)); err == nil {
			t.Fatalf("%q was accepted", v)
		}
	}
	env := baseEnv()
	env["SANDBOX_MANAGER_CLEAN_NAME"] = "Clean Sandbox"
	env["SANDBOX_MANAGER_CLEAN_DEVICE_NAME"] = "clean pods"
	s, err := loadSettings(envLookup(env))
	if err != nil || s.config.CleanName != "Clean Sandbox" || s.config.CleanDeviceName != "clean pods" {
		t.Fatalf("clean names: %q %q, %v", s.config.CleanName, s.config.CleanDeviceName, err)
	}
}

func TestLoadSettingsReadsTheDaemonIdentity(t *testing.T) {
	env := baseEnv()
	s, err := loadSettings(envLookup(env))
	if err != nil || s.config.DaemonID != "" {
		t.Fatalf("unset: %q, %v; want it left to derivation", s.config.DaemonID, err)
	}
	env["SANDBOX_MANAGER_DAEMON_ID"] = " company-sandbox-prod "
	if s, err = loadSettings(envLookup(env)); err != nil || s.config.DaemonID != "company-sandbox-prod" {
		t.Fatalf("set: %q, %v", s.config.DaemonID, err)
	}
}

// The manager handles no git or LLM credentials (ADR 0002), so every setting
// it still reads must be enough to start it.
func TestManagerStartsWithEveryRemainingSetting(t *testing.T) {
	up := newFakeMultica(t, testWorkspaceA)
	env := map[string]string{
		"MULTICA_SERVER_URL":              up.URL,
		"SANDBOX_POOL_TOKEN":              "mul_pat_pool",
		"SANDBOX_MANAGER_LISTEN":          "127.0.0.1:0",
		"SANDBOX_POOL_ADMIN_LISTEN":       "127.0.0.1:0",
		"SANDBOX_AUDIT_FILE":              "-",
		"SANDBOX_POOL_QUOTA":              "6",
		"SANDBOX_POOL_SYNC_INTERVAL":      "45s",
		"SANDBOX_POOL_RUNTIME_VISIBILITY": "public",
		"SANDBOX_MANAGER_NAME":            "Managed Sandbox",
		"SANDBOX_POOL_SIZE":               "4",
		"SANDBOX_PROVIDERS":               "claude,codex",
		"SANDBOX_POOL_DRAIN_TIMEOUT":      "5m",
		"SANDBOX_POOL_HEARTBEAT_TIMEOUT":  "30s",
		"SANDBOX_POOL_RESET_TIMEOUT":      "3m",
		"SANDBOX_POOL_LOST_RETENTION":     "7m",
		"SANDBOX_PLACEMENT":               "person-issue",
		"SANDBOX_POD_CAPACITY":            "8",
		"SANDBOX_PLACEMENT_WAIT":          "20s",
		"SANDBOX_POOL_WAKE_INTERVAL":      "2s",
		"SANDBOX_HOLD_TIMEOUT":            "5m",
		"SANDBOX_POD_MAX_LIFETIME":        "1h",
		"SANDBOX_POD_DRAIN_GRACE":         "2m",
		"SANDBOX_POOL_RECONCILE_INTERVAL": "1m",
		"SANDBOX_POOL_LEASE_MAX":          "3h",
		"SANDBOX_MODELS":                  "claude=claude-sonnet-4-6,claude-opus-4-7;codex=gpt-5.2",
	}
	s, err := loadSettings(envLookup(env))
	if err != nil {
		t.Fatalf("a remaining setting was refused: %v", err)
	}
	pool, err := manager.New(s.config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("the pool refused the settings: %v", err)
	}
	if _, err := pool.Sync(context.Background()); err != nil {
		t.Fatalf("first workspace listing: %v", err)
	}
}

func TestLoadSettingsRefusesBadSettings(t *testing.T) {
	cases := map[string]struct {
		set  map[string]string
		want string
	}{
		"bad duration":      {map[string]string{"SANDBOX_HOLD_TIMEOUT": "soon"}, "SANDBOX_HOLD_TIMEOUT"},
		"negative duration": {map[string]string{"SANDBOX_POD_DRAIN_GRACE": "-1m"}, "SANDBOX_POD_DRAIN_GRACE"},
		"bad capacity":      {map[string]string{"SANDBOX_POD_CAPACITY": "-2"}, "SANDBOX_POD_CAPACITY"},
		"bad placement":     {map[string]string{"SANDBOX_PLACEMENT": "pod"}, "SANDBOX_PLACEMENT"},
		"old issue rule":    {map[string]string{"SANDBOX_PLACEMENT": "issue"}, "SANDBOX_PLACEMENT"},
		"bad models":        {map[string]string{"SANDBOX_MODELS": "claude"}, "SANDBOX_MODELS"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := baseEnv()
			for k, v := range tc.set {
				env[k] = v
			}
			_, err := loadSettings(envLookup(env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestStartupRejectsSettingsThePoolRefuses(t *testing.T) {
	env := baseEnv()
	env["MULTICA_SERVER_URL"] = "ftp://nope"
	s, err := loadSettings(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	s.podListen, s.adminListen = "127.0.0.1:0", "127.0.0.1:0"
	err = runManager(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), s)
	if err == nil || !strings.Contains(err.Error(), "Multica URL") {
		t.Fatalf("a bad Multica URL must stop startup, got %v", err)
	}
}
