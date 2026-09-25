package support

// The cluster's part of a bundle: the API server, nodes, Kuben's pods and
// warnings, the KubenConfigs, and Kuben's own logs.

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sdiscovery "k8s.io/client-go/discovery"

	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// failed is a part that could not be read.
func failed(err error) map[string]any { return map[string]any{"error": err.Error()} }

// orNull is s, or null when empty (an absent Option in Rust).
func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// clusterSection is the state of the cluster and of Kuben's namespace.
func clusterSection(ctx context.Context, c registry.Cluster, namespace string) map[string]any {
	var apiserver any
	if disc := k8sdiscovery.ToDiscoveryInterfaceWithContext(c.Typed.Discovery()); disc == nil {
		apiserver = map[string]any{"error": "no discovery client"}
	} else if v, err := disc.ServerVersionWithContext(ctx); err != nil {
		apiserver = failed(err)
	} else if v != nil {
		apiserver = v.GitVersion
	}
	core := c.Typed.CoreV1()
	var nodes, pods, events, configs any
	if list, err := core.Nodes().List(ctx, metav1.ListOptions{}); err != nil {
		nodes = failed(err)
	} else {
		out := make([]any, 0, len(list.Items))
		for _, n := range list.Items {
			out = append(out, node(n))
		}
		nodes = out
	}
	if list, err := core.Pods(namespace).List(ctx, metav1.ListOptions{}); err != nil {
		pods = failed(err)
	} else {
		out := make([]any, 0, len(list.Items))
		for _, p := range list.Items {
			out = append(out, pod(p))
		}
		pods = out
	}
	if list, err := core.Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"}); err != nil {
		events = failed(err)
	} else {
		events = warnings(list.Items)
	}
	gvr := v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.KubenConfigResource)
	if list, err := c.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{}); err != nil {
		configs = failed(err)
	} else {
		out := make([]any, 0, len(list.Items))
		for _, item := range list.Items {
			out = append(out, map[string]any{"name": item.GetName(), "status": item.Object["status"]})
		}
		configs = out
	}
	return map[string]any{
		"apiserver":      apiserver,
		"namespace":      namespace,
		"nodes":          nodes,
		"pods":           pods,
		"warning_events": events,
		"kuben_configs":  configs,
	}
}

// node is a node's versions, capacity and conditions.
func node(n corev1.Node) map[string]any {
	var allocatable, conditions, unschedulable any
	if n.Status.Allocatable != nil {
		quantities := make(map[string]any, len(n.Status.Allocatable))
		for name, q := range n.Status.Allocatable {
			quantities[string(name)] = q.String()
		}
		allocatable = quantities
	}
	if n.Status.Conditions != nil {
		list := make([]any, 0, len(n.Status.Conditions))
		for _, c := range n.Status.Conditions {
			list = append(list, map[string]any{"type": string(c.Type), "status": string(c.Status), "reason": orNull(c.Reason)})
		}
		conditions = list
	}
	if n.Spec.Unschedulable {
		unschedulable = true
	}
	return map[string]any{
		"kubelet":       n.Status.NodeInfo.KubeletVersion,
		"os_image":      n.Status.NodeInfo.OSImage,
		"allocatable":   allocatable,
		"conditions":    conditions,
		"unschedulable": unschedulable,
	}
}

// pod is a pod's phase, node and containers.
func pod(p corev1.Pod) map[string]any {
	var containers any
	if p.Status.ContainerStatuses != nil {
		list := make([]any, 0, len(p.Status.ContainerStatuses))
		for _, c := range p.Status.ContainerStatuses {
			var last any
			if t := c.LastTerminationState.Terminated; t != nil {
				last = map[string]any{"reason": orNull(t.Reason), "exit_code": t.ExitCode}
			}
			list = append(list, map[string]any{
				"name": c.Name, "image": c.Image, "ready": c.Ready,
				"restarts": c.RestartCount, "last_termination": last,
			})
		}
		containers = list
	}
	return map[string]any{
		"name":       p.Name,
		"phase":      orNull(string(p.Status.Phase)),
		"node":       orNull(p.Spec.NodeName),
		"containers": containers,
	}
}

// warnings is the newest warning events first, at most maxEvents; events
// without a time come last.
func warnings(events []corev1.Event) []any {
	sorted := slices.Clone(events)
	slices.SortStableFunc(sorted, func(a, b corev1.Event) int {
		at, bt := a.LastTimestamp, b.LastTimestamp
		switch {
		case at.IsZero() && bt.IsZero():
			return 0
		case at.IsZero():
			return 1
		case bt.IsZero():
			return -1
		default:
			return bt.Compare(at.Time)
		}
	})
	out := make([]any, 0, min(len(sorted), maxEvents))
	for _, e := range sorted[:min(len(sorted), maxEvents)] {
		kind, name := e.InvolvedObject.Kind, e.InvolvedObject.Name
		if kind == "" {
			kind = "?"
		}
		if name == "" {
			name = "?"
		}
		var count, last any
		if e.Count != 0 {
			count = e.Count
		}
		if !e.LastTimestamp.IsZero() {
			last = e.LastTimestamp.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, map[string]any{
			"object":  kind + "/" + name,
			"reason":  orNull(e.Reason),
			"message": orNull(e.Message),
			"count":   count,
			"last":    last,
		})
	}
	return out
}

// podLogs is the newest lines of every container of Kuben's own pods.
func podLogs(ctx context.Context, c registry.Cluster, namespace string, lines uint32) map[string]any {
	pods := c.Typed.CoreV1().Pods(namespace)
	list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=kuben"})
	if err != nil {
		return failed(err)
	}
	out := map[string]any{}
	tail := int64(lines)
	for _, p := range list.Items {
		for _, container := range p.Spec.Containers {
			raw, err := pods.GetLogs(p.Name, &corev1.PodLogOptions{
				Container: container.Name, TailLines: &tail, Timestamps: true,
			}).DoRaw(ctx)
			text := strings.ToValidUTF8(string(raw), string(utf8.RuneError))
			if err != nil {
				text = "(no logs: " + err.Error() + ")"
			}
			out[p.Name+"/"+container.Name] = rustLines(text)
		}
	}
	return out
}

// hostLogs is the newest lines of the host's `kuben` service.
func hostLogs(ctx context.Context, lines uint32) map[string]any {
	cmd := exec.CommandContext(ctx, "journalctl", "-u", "kuben", "--no-pager", "-o", "short-iso", "-n",
		strconv.FormatUint(uint64(lines), 10))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	switch {
	case err == nil:
		return map[string]any{"kuben.service": rustLines(strings.ToValidUTF8(string(out), string(utf8.RuneError)))}
	case isExit(err):
		return map[string]any{"error": strings.TrimSpace(strings.ToValidUTF8(stderr.String(), string(utf8.RuneError)))}
	default:
		return map[string]any{"error": "journalctl: " + err.Error()}
	}
}

// isExit reports whether err is a command that ran and failed.
func isExit(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit)
}
