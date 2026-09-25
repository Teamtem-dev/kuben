// Package bootstrap is the agent inside the cluster Kuben runs on (M2.8;
// crates/kuben-agent/src/bootstrap.rs): no one hands it a token by hand.
//
//   - [Enrollment]: the hub publishes where it listens, its CA, this
//     cluster's id and, while the agent has no valid certificate, a
//     bootstrap token, as the files of a Secret mounted into the pod. The
//     agent waits for them.
//   - [IdentitySecret]: the device key and the certificate live in a Secret
//     of the agent's own, so a restarted or rescheduled pod keeps its
//     identity instead of needing a new token.
package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/Teamtem-dev/kuben/internal/agent/state"
)

// The files of the enrollment directory.
const (
	HubFile     = "hub"
	HubCAFile   = "hub-ca.crt"
	ClusterFile = "cluster"
	TokenFile   = "token"
)

const (
	// namespaceFile is where a pod finds its own namespace.
	namespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	// Poll is how often the agent looks for the published enrollment.
	Poll         = 5 * time.Second
	fieldManager = "kuben-agent"
)

// Enrollment is what the hub published for this cluster's agent.
type Enrollment struct {
	Hub     string
	HubCA   string
	Cluster string
	// TokenFile is the bootstrap token's file; it may be absent once the
	// agent enrolled.
	TokenFile string
}

func readTrimmed(path string) (string, bool) {
	text, err := os.ReadFile(path) //nolint:gosec // a path of the mounted Secret
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(text))
	return s, s != ""
}

// ReadEnrollment is the enrollment in dir, once the hub published it.
func ReadEnrollment(dir string) (Enrollment, bool) {
	hubCA := filepath.Join(dir, HubCAFile)
	if _, ok := readTrimmed(hubCA); !ok {
		return Enrollment{}, false
	}
	hub, ok := readTrimmed(filepath.Join(dir, HubFile))
	if !ok {
		return Enrollment{}, false
	}
	cluster, ok := readTrimmed(filepath.Join(dir, ClusterFile))
	if !ok {
		return Enrollment{}, false
	}
	return Enrollment{Hub: hub, HubCA: hubCA, Cluster: cluster, TokenFile: filepath.Join(dir, TokenFile)}, true
}

// WaitEnrollment waits until the hub has published the enrollment in dir
// (a mounted Secret appears in the pod a little after it is written),
// looking every poll. False when ctx ends first.
func WaitEnrollment(ctx context.Context, dir string, poll time.Duration, logger *slog.Logger) (Enrollment, bool) {
	said := false
	for {
		if e, ok := ReadEnrollment(dir); ok {
			return e, true
		}
		if !said {
			logger.Info("waiting for the hub to publish this cluster's enrollment", "dir", dir)
			said = true
		}
		wait := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			wait.Stop()
			return Enrollment{}, false
		case <-wait.C:
		}
	}
}

// OwnNamespace is the pod's own namespace.
func OwnNamespace() (string, bool) { return readTrimmed(namespaceFile) }

// files are the identity files kept in the Secret.
func files() []string { return []string{state.DeviceKey, state.Certificate} }

// IdentitySecret is the Secret holding the agent's device key and
// certificate.
type IdentitySecret struct {
	api  dynamic.ResourceInterface
	name string
}

// NewIdentitySecret is the Secret name of namespace.
func NewIdentitySecret(client dynamic.Interface, namespace, name string) IdentitySecret {
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	return IdentitySecret{api: client.Resource(secrets).Namespace(namespace), name: name}
}

// Restore writes the stored identity into the state directory dir, over
// what is there. False when the Secret holds none yet.
func (s IdentitySecret) Restore(ctx context.Context, dir string) (bool, error) {
	secret, err := s.api.Get(ctx, s.name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the identity Secret: %w", err)
	}
	data, ok := secret.Object["data"].(map[string]any)
	if !ok {
		return false, nil
	}
	restored := false
	for _, file := range files() {
		encoded, ok := data[file].(string)
		if !ok {
			continue
		}
		content, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return false, fmt.Errorf("the identity Secret's %s: %w", file, err)
		}
		if err := state.WritePrivate(filepath.Join(dir, file), content); err != nil {
			return false, err //nolint:wrapcheck // says which file
		}
		restored = true
	}
	return restored, nil
}

// Save keeps the identity files of dir in the Secret.
func (s IdentitySecret) Save(ctx context.Context, dir string) error {
	data := map[string]string{}
	for _, file := range files() {
		if content, err := os.ReadFile(filepath.Join(dir, file)); err == nil { //nolint:gosec // the agent's own path
			data[file] = base64.StdEncoding.EncodeToString(content)
		}
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":   s.name,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "kuben"},
		},
		"type": "Opaque",
		"data": data,
	})
	if err != nil {
		return fmt.Errorf("the identity Secret: %w", err)
	}
	force := true
	_, err = s.api.Patch(ctx, s.name, types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: fieldManager, Force: &force})
	if err != nil {
		return fmt.Errorf("keep the identity in its Secret: %w", err)
	}
	return nil
}
