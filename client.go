package main

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// loadClientConfig builds a deferred kubeconfig loader honoring an optional
// explicit kubeconfig path and context override, same as kubectl.
func loadClientConfig(kubeconfigPath, contextName string) clientcmd.ClientConfig {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
}

func buildRestConfig(kubeconfigPath, contextName string) (*rest.Config, error) {
	return loadClientConfig(kubeconfigPath, contextName).ClientConfig()
}

// resolveNamespace returns the namespace from the active kubeconfig context,
// falling back to "default" if unset.
func resolveNamespace(kubeconfigPath, contextName string) string {
	ns, _, err := loadClientConfig(kubeconfigPath, contextName).Namespace()
	if err != nil || ns == "" {
		return "default"
	}
	return ns
}

func buildClientset(config *rest.Config) (*kubernetes.Clientset, error) {
	return kubernetes.NewForConfig(config)
}

func buildDynamicClient(config *rest.Config) (dynamic.Interface, error) {
	return dynamic.NewForConfig(config)
}

// resolveGVR maps a resource name (plural, singular, short name, or kind) to its
// GroupVersionResource and reports whether the resource is namespaced.
//
// Resolution order:
//  1. REST mapper (handles plural and singular names)
//  2. Discovery API resource list (handles short names like "po", "pv", "svc")
func resolveGVR(config *rest.Config, resource string) (schema.GroupVersionResource, bool, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return schema.GroupVersionResource{}, false, fmt.Errorf("discovery client: %w", err)
	}
	cachedDC := memory.NewMemCacheClient(dc)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDC)

	// Try plural/singular resolution via the REST mapper first.
	gvr, err := mapper.ResourceFor(schema.GroupVersionResource{Resource: resource})
	if err == nil {
		resolved, namespaced, serr := gvrWithScope(mapper, gvr)
		if serr == nil {
			return resolved, namespaced, nil
		}
		// Mapper had the GVR but couldn't resolve the scope (stale discovery
		// cache, CRD just registered). Fall through to ServerPreferredResources
		// which carries Namespaced directly.
	}

	// Fall back to iterating discovery resources to match short names.
	lists, derr := dc.ServerPreferredResources()
	if derr != nil && len(lists) == 0 {
		return schema.GroupVersionResource{}, false, fmt.Errorf("unknown resource %q", resource)
	}
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			if r.Name == resource || r.SingularName == resource {
				gvr = schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: r.Name}
				return gvr, r.Namespaced, nil
			}
			for _, short := range r.ShortNames {
				if short == resource {
					gvr = schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: r.Name}
					return gvr, r.Namespaced, nil
				}
			}
		}
	}

	return schema.GroupVersionResource{}, false, fmt.Errorf("unknown resource %q", resource)
}

func gvrWithScope(mapper *restmapper.DeferredDiscoveryRESTMapper, gvr schema.GroupVersionResource) (schema.GroupVersionResource, bool, error) {
	gvk, err := mapper.KindFor(gvr)
	if err != nil {
		return schema.GroupVersionResource{}, false, fmt.Errorf("kind for %v: %w", gvr, err)
	}
	mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}, gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, false, fmt.Errorf("REST mapping for %v: %w", gvr, err)
	}
	return gvr, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}
