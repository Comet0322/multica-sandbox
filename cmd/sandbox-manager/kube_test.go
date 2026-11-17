package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: dev
    cluster: {server: "https://127.0.0.1:6443"}
users:
  - name: dev-user
    user: {token: "not-a-real-token"}
contexts:
  - name: dev-user
    context: {cluster: dev, user: dev-user, namespace: multica-sandbox}
current-context: dev-user
`

func TestLoadSettingsCreatesPodsThroughKubernetesWithATemplate(t *testing.T) {
	dir := t.TempDir()
	env := baseEnv()
	env["SANDBOX_POD_TEMPLATE"] = writeFile(t, dir, "pod.yaml", "kind: Pod\nspec:\n  containers:\n    - {name: daemon, image: pod:1}\n")
	env["KUBECONFIG"] = writeFile(t, dir, "kubeconfig", testKubeconfig)
	env["SANDBOX_MAX_PODS"] = "6"
	env["SANDBOX_POD_START_TIMEOUT"] = "90s"
	s, err := loadSettings(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	kc := s.config.Kube
	if kc == nil {
		t.Fatal("no Kubernetes settings")
	}
	if kc.Namespace != "multica-sandbox" || kc.MaxPods != 6 || kc.WarmPods != 2 || kc.StartTimeout != 90*time.Second {
		t.Fatalf("settings = ns %q, max %d, warm %d, start %s", kc.Namespace, kc.MaxPods, kc.WarmPods, kc.StartTimeout)
	}
	if kc.Template.Spec.Containers[0].Image != "pod:1" || kc.Client == nil {
		t.Fatal("the template or the client is missing")
	}
	if kc.StartFailures != 0 {
		t.Fatalf("start failures = %d, want 0 (the service's default)", kc.StartFailures)
	}

	env["SANDBOX_WARM_PODS"] = "1"
	env["SANDBOX_NAMESPACE"] = "other"
	env["SANDBOX_POD_START_FAILURES"] = "5"
	s, err = loadSettings(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if s.config.Kube.WarmPods != 1 || s.config.Kube.Namespace != "other" || s.config.Kube.StartFailures != 5 {
		t.Fatalf("warm %d, ns %q, start failures %d; want 1, other, 5", s.config.Kube.WarmPods, s.config.Kube.Namespace, s.config.Kube.StartFailures)
	}

	env["SANDBOX_POD_START_FAILURES"] = "-1"
	if _, err := loadSettings(envLookup(env)); err == nil || !strings.Contains(err.Error(), "SANDBOX_POD_START_FAILURES") {
		t.Fatalf("err = %v, want SANDBOX_POD_START_FAILURES refused", err)
	}
}

// The manager learns a workspace's runtimes from a pod's daemon registering
// them, so with no warm pod and no pod at all nothing would ever be claimed.
func TestLoadSettingsRefusesNoWarmPod(t *testing.T) {
	dir := t.TempDir()
	env := baseEnv()
	env["SANDBOX_POD_TEMPLATE"] = writeFile(t, dir, "pod.yaml", "kind: Pod\nspec:\n  containers:\n    - {name: daemon, image: pod:1}\n")
	env["KUBECONFIG"] = writeFile(t, dir, "kubeconfig", testKubeconfig)
	env["SANDBOX_WARM_PODS"] = "0"
	if _, err := loadSettings(envLookup(env)); err == nil || !strings.Contains(err.Error(), "SANDBOX_WARM_PODS must be at least 1") {
		t.Fatalf("err = %v, want SANDBOX_WARM_PODS refused", err)
	}
}

func TestLoadSettingsRequiresAPodTemplate(t *testing.T) {
	env := baseEnv()
	delete(env, "SANDBOX_POD_TEMPLATE")
	if _, err := loadSettings(envLookup(env)); err == nil || !strings.Contains(err.Error(), "SANDBOX_POD_TEMPLATE is required") {
		t.Fatalf("err = %v, want the template required", err)
	}
}

func TestLoadSettingsRefusesABadPodTemplate(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := writeFile(t, dir, "kubeconfig", testKubeconfig)
	for name, tmpl := range map[string]string{
		"relative": "pod.yaml",
		"missing":  filepath.Join(dir, "none.yaml"),
		"typo":     writeFile(t, dir, "typo.yaml", "kind: Pod\nspec:\n  containerz: []\n"),
	} {
		env := baseEnv()
		env["SANDBOX_POD_TEMPLATE"], env["KUBECONFIG"] = tmpl, kubeconfig
		if _, err := loadSettings(envLookup(env)); err == nil || !strings.Contains(err.Error(), "SANDBOX_POD_TEMPLATE") {
			t.Errorf("%s: err = %v, want one naming SANDBOX_POD_TEMPLATE", name, err)
		}
	}
}

func TestLoadSettingsReadsThePodOwner(t *testing.T) {
	dir := t.TempDir()
	env := baseEnv()
	env["SANDBOX_POD_TEMPLATE"] = writeFile(t, dir, "pod.yaml", "kind: Pod\nspec:\n  containers:\n    - {name: daemon, image: pod:1}\n")
	env["KUBECONFIG"] = writeFile(t, dir, "kubeconfig", testKubeconfig)
	for v, want := range map[string]string{"": "", "deployment/sandbox-manager": "sandbox-manager", "deploy/m-1": "m-1", " Deployments/x ": "x"} {
		env["SANDBOX_POD_OWNER"] = v
		s, err := loadSettings(envLookup(env))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if got := s.config.Kube.OwnerDeployment; got != want {
			t.Fatalf("%q: owner %q, want %q", v, got, want)
		}
	}
	for _, v := range []string{"sandbox-manager", "statefulset/x", "deployment/", "deployment/Not_A_Name"} {
		env["SANDBOX_POD_OWNER"] = v
		if _, err := loadSettings(envLookup(env)); err == nil || !strings.Contains(err.Error(), "SANDBOX_POD_OWNER") {
			t.Fatalf("%q: err = %v, want SANDBOX_POD_OWNER refused", v, err)
		}
	}
}
