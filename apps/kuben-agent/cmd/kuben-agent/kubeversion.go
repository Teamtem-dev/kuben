package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	k8sversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/rest"
)

// kubernetesVersion is the apiserver's GET /version gitVersion, asked as
// client-go's DiscoveryClient.ServerVersion does (same request, timeout and
// user agent). The discovery package is not used for it: it registers every
// built-in Kubernetes API type (client-go/kubernetes/scheme), about 10 MB
// of the agent, for this one request.
func kubernetesVersion(ctx context.Context, c *rest.Config) (string, error) {
	config := *c
	config.APIPath = ""
	config.GroupVersion = nil
	if config.Timeout == 0 {
		config.Timeout = 32 * time.Second // discovery's defaultTimeout
	}
	if config.Burst == 0 {
		config.Burst = 300 // discovery's defaultBurst
	}
	// Only answers that are not 2xx are decoded, into the error.
	codec := runtime.NoopEncoder{Decoder: unstructured.UnstructuredJSONScheme}
	config.NegotiatedSerializer = serializer.NegotiatedSerializerWrapper(runtime.SerializerInfo{Serializer: codec})
	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	client, err := rest.UnversionedRESTClientFor(&config)
	if err != nil {
		return "", fmt.Errorf("kubernetes client: %w", err)
	}
	body, err := client.Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("kubernetes version: %w", err)
	}
	var info k8sversion.Info
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("unable to parse the server version: %w", err)
	}
	return info.GitVersion, nil
}
