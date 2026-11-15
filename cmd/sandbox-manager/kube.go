package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"sandbox-manager/internal/sandbox"
)

// inClusterNamespace is where Kubernetes tells a pod its own namespace.
const inClusterNamespace = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// kubeSettings reads the settings of the sandbox service, which creates the
// pods through the Kubernetes API from SANDBOX_POD_TEMPLATE.
//
// The credentials come from KUBECONFIG when it is set (the operator's own,
// for now), else from the pod's service account. The namespace is
// SANDBOX_NAMESPACE, else the kubeconfig context's, else the pod's own.
func kubeSettings(getenv func(string) string, maxPods, warmPods int, startTimeout time.Duration) (*sandbox.KubeConfig, error) {
	path := getenv("SANDBOX_POD_TEMPLATE")
	if !filepath.IsAbs(path) {
		return nil, errors.New("SANDBOX_POD_TEMPLATE must be an absolute path")
	}
	tmpl, err := sandbox.LoadPodTemplate(path)
	if err != nil {
		return nil, fmt.Errorf("SANDBOX_POD_TEMPLATE: %w", err)
	}
	var rc *rest.Config
	ns := strings.TrimSpace(getenv("SANDBOX_NAMESPACE"))
	if kc := getenv("KUBECONFIG"); kc != "" {
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: kc}, &clientcmd.ConfigOverrides{})
		if rc, err = loader.ClientConfig(); err != nil {
			return nil, fmt.Errorf("KUBECONFIG: %w", err)
		}
		if ns == "" {
			ns, _, _ = loader.Namespace()
		}
	} else {
		if rc, err = rest.InClusterConfig(); err != nil {
			return nil, fmt.Errorf("no KUBECONFIG and not running in a cluster: %w", err)
		}
		if ns == "" {
			if raw, err := os.ReadFile(inClusterNamespace); err == nil {
				ns = strings.TrimSpace(string(raw))
			}
		}
	}
	if ns == "" || ns == "default" && getenv("SANDBOX_NAMESPACE") == "" {
		return nil, errors.New("SANDBOX_NAMESPACE is required: the kubeconfig names no namespace of its own")
	}
	rc.UserAgent = "sandbox-manager"
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("Kubernetes client: %w", err)
	}
	return &sandbox.KubeConfig{
		Client: client, Namespace: ns, Template: tmpl,
		MaxPods: maxPods, WarmPods: warmPods, StartTimeout: startTimeout,
	}, nil
}
