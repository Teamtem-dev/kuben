package controller

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// crdManifest is a byte-identical copy of the frozen CRD manifest
// charts/kuben/crds/kuben.dev_all.yaml (a test keeps them equal while the
// chart's file exists).
//
//go:embed kuben.dev_all.yaml
var crdManifest string

// CRDManifest is the manifest of every Kuben CRD, as the binary applies it.
func CRDManifest() []byte { return []byte(crdManifest) }

// CRDs is every CRD of the manifest, in its order.
func CRDs() ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(crdManifest), 4096)
	var out []*unstructured.Unstructured
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading the CRD manifest: %w", err)
		}
		if len(doc) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: doc})
	}
}

// EnsureCRDs applies every CRD with server-side apply at boot (ADR-017):
// Helm never upgrades `crds/`, so the binary owns its own schema
// lifecycle. Idempotent; safe to call on every start.
func EnsureCRDs(ctx context.Context, c client.Client, logger *slog.Logger) error {
	crds, err := CRDs()
	if err != nil {
		return err
	}
	for _, crd := range crds {
		if err := apply(ctx, c, crd.Object); err != nil {
			return err
		}
		logger.Info("crd applied", "crd", crd.GetName())
	}
	return nil
}
