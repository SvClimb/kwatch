package main

import (
	"context"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// completionResources is the static list offered when completing the first argument.
var completionResources = []string{
	"pods", "po",
	"deployments", "deploy",
	"replicasets", "rs",
	"statefulsets", "sts",
	"daemonsets", "ds",
	"jobs",
	"cronjobs", "cj",
	"services", "svc",
	"ingresses", "ing",
	"configmaps", "cm",
	"secrets",
	"namespaces", "ns",
	"nodes", "no",
	"persistentvolumeclaims", "pvc",
	"persistentvolumes", "pv",
	"horizontalpodautoscalers", "hpa",
	"endpoints", "ep",
	"events", "ev",
	"serviceaccounts", "sa",
}

// fetchNamespaces returns all namespace names from the cluster for shell completion.
// Returns nil on error so the shell falls back to no completion.
func fetchNamespaces(config *rest.Config) []string {
	cs, err := buildClientset(config)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		names = append(names, ns.Name)
	}
	sort.Strings(names)
	return names
}

// fetchContexts returns context names from the kubeconfig for shell completion.
func fetchContexts(kubeconfigPath string) []string {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}
	cfg, err := loadingRules.Load()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
