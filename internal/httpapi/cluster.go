package api

// The cluster as the routes reach it (routes/scope.rs cluster, kube_error).

import (
	"errors"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
)

// cluster is the primary cluster; unavailable without one.
func (s *Server) cluster() (registry.Cluster, error) {
	r, ok := s.deps.Cluster.Get()
	if !ok || r == nil {
		return registry.Cluster{}, kerrors.New(kerrors.Unavailable, "no kubernetes cluster configured")
	}
	return r.Primary(), nil
}

// kubeError maps a Kubernetes API error about the object name to a
// user-facing problem.
func kubeError(err error, name string) error {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return kerrors.Wrap(err, "kubernetes API")
	}
	switch s := status.Status(); s.Code {
	case http.StatusNotFound:
		return scopeNotFound("object", name)
	case http.StatusConflict:
		return kerrors.New(kerrors.Conflict, "`%s` already exists or was changed concurrently; retry", name)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return kerrors.New(kerrors.Validation, "%s", s.Message)
	}
	return kerrors.Wrap(err, "kubernetes API")
}
