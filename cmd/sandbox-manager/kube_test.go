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

	env["SANDBOX_WARM_PODS"] = "0"
	env["SANDBOX_NAMESPACE"] = "other"
	s, err = loadSettings(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if s.config.Kube.WarmPods != 0 || s.config.Kube.Namespace != "other" {
		t.Fatalf("warm %d, ns %q; want 0 and other", s.config.Kube.WarmPods, s.config.Kube.Namespace)
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
