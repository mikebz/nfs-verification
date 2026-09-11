package framework

import (
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client bundles the typed client and the rest config, which the exec helper
// needs to open a SPDY stream to a pod.
type Client struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	Rest    *rest.Config
	// Context is the kubeconfig context in use. It names the cluster a cached
	// preflight belongs to.
	Context string
}

var (
	clientOnce sync.Once
	client     *Client
	clientErr  error
)

// NewClient builds clients from the configured kubeconfig. The result is
// cached: every test in a run shares one connection pool.
func NewClient() (*Client, error) {
	clientOnce.Do(func() {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		if cfg.Kubeconfig != "" {
			rules.ExplicitPath = cfg.Kubeconfig
		}
		overrides := &clientcmd.ConfigOverrides{}
		if cfg.Context != "" {
			overrides.CurrentContext = cfg.Context
		}
		clientCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
		restCfg, err := clientCfg.ClientConfig()
		if err != nil {
			clientErr = fmt.Errorf("building rest config: %w", err)
			return
		}
		contextName := cfg.Context
		if contextName == "" {
			if raw, err := clientCfg.RawConfig(); err == nil {
				contextName = raw.CurrentContext
			}
		}
		// The harness drives dozens of pods at once; the client-go defaults
		// throttle hard enough to distort timing measurements.
		restCfg.QPS = 50
		restCfg.Burst = 100
		kube, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			clientErr = fmt.Errorf("building kube client: %w", err)
			return
		}
		dyn, err := dynamic.NewForConfig(restCfg)
		if err != nil {
			clientErr = fmt.Errorf("building dynamic client: %w", err)
			return
		}
		client = &Client{Kube: kube, Dynamic: dyn, Rest: restCfg, Context: contextName}
	})
	return client, clientErr
}

// ServerVersion returns the cluster's reported Kubernetes version.
func (c *Client) ServerVersion() (string, error) {
	v, err := c.Kube.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

// HasAPI reports whether a GroupVersionResource is served by the cluster. Used
// to gate snapshot cases instead of assuming the CRDs are installed.
func (c *Client) HasAPI(gvr schema.GroupVersionResource) bool {
	gv := schema.GroupVersion{Group: gvr.Group, Version: gvr.Version}
	list, err := c.Kube.Discovery().ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		return false
	}
	for _, r := range list.APIResources {
		if r.Name == gvr.Resource {
			return true
		}
	}
	return false
}

// IgnoreNotFound collapses a NotFound error to nil, for cleanup paths.
func IgnoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ListOptions builds list options for a label selector.
func ListOptions(selector string) metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: selector}
}

// DeleteNow is a delete with grace period zero, for force-delete cases.
func DeleteNow() metav1.DeleteOptions {
	zero := int64(0)
	policy := metav1.DeletePropagationBackground
	return metav1.DeleteOptions{GracePeriodSeconds: &zero, PropagationPolicy: &policy}
}
