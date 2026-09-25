package build

// What a scan container left behind (M4.6; evidence.rs): its report in the
// termination log and the image's SBOM in its log, between markers. Both
// are read once the pod is done and kept in SQL; the pod is deleted
// afterwards.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/scan"
)

const (
	// MaxSbomBytes is the largest SBOM kept (gzip).
	MaxSbomBytes = 8 << 20
	// maxLogBytes is the most log read from a scan container.
	maxLogBytes = 12 << 20
	sbomBegin   = "kuben-sbom-begin\n"
	sbomEnd     = "\nkuben-sbom-end"
	// maxScanner is the longest scanner name kept, in characters.
	maxScanner = 128
	// maxDetail is the longest scan detail kept, in characters.
	maxDetail = 2048
)

// SbomOf is the gzip SBOM between the markers of log, if it is well formed.
func SbomOf(log string) opt.Val[[]byte] {
	none := opt.None[[]byte]()
	_, rest, found := strings.Cut(log, sbomBegin)
	if !found {
		return none
	}
	encoded, _, found := strings.Cut(rest, sbomEnd)
	if !found {
		return none
	}
	encoded = strings.TrimSpace(encoded)
	// The base64 crate refused line breaks inside; Go's decoder skips them.
	if strings.ContainsAny(encoded, "\r\n") {
		return none
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) > MaxSbomBytes || !bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		return none
	}
	return opt.Some(data)
}

// ReportOf is the report in message, or an unavailable one saying why not.
func ReportOf(message opt.Val[string]) scan.Report {
	m := strings.TrimSpace(message.Or(""))
	if m == "" {
		return scan.Unavailable("the scan left no report")
	}
	var report scan.Report
	if err := json.Unmarshal([]byte(m), &report); err != nil {
		return scan.Unavailable("the scan report does not parse")
	}
	return report
}

// SummaryOf is the summary of report, scanned at nowMs.
func SummaryOf(report scan.Report, nowMs int64) scan.Summary {
	updated := opt.None[int64]()
	if db, ok := report.DB.Get(); ok {
		if t, err := time.Parse(time.RFC3339Nano, db); err == nil {
			updated = opt.Some(t.UnixMilli())
		}
	}
	summary := scan.Summary{
		Status:      report.Status,
		Scanner:     firstChars(report.Scanner, maxScanner),
		DBUpdatedAt: updated,
		Findings:    []scan.Finding{},
		ScannedAt:   nowMs,
	}
	if report.Status == scan.StatusOK {
		summary.Counts = report.Counts
		summary.Findings = report.Listed()
	}
	return summary
}

// firstChars is the first n characters (Unicode scalar values) of s.
func firstChars(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// detailOf is the stored detail of report: its first 2048 characters.
func detailOf(report scan.Report) opt.Val[string] {
	if d, ok := report.Detail.Get(); ok {
		return opt.Some(firstChars(d, maxDetail))
	}
	return opt.None[string]()
}

// Collect is the report and SBOM of the newest pod selector matches in pods.
func Collect(
	ctx context.Context, pods typedcorev1.PodInterface, selector string, logger *slog.Logger,
) (scan.Report, opt.Val[[]byte], error) {
	none := opt.None[[]byte]()
	listed, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return scan.Report{}, none, fmt.Errorf("listing the scan pods: %w", err)
	}
	pod, ok := newestPod(listed.Items).Get()
	if !ok {
		return scan.Unavailable("the scan pod is gone"), none, nil
	}
	var terminated *corev1.ContainerStateTerminated
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == outcome.ScanContainer {
			terminated = c.State.Terminated
			break
		}
	}
	if terminated == nil {
		return scan.Unavailable("the scan did not run"), none, nil
	}
	report := ReportOf(nonEmpty(terminated.Message))
	if report.Status != scan.StatusOK {
		return report, none, nil
	}
	limit := int64(maxLogBytes)
	log, err := pods.GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: outcome.ScanContainer, LimitBytes: &limit,
	}).DoRaw(ctx)
	if err == nil && !utf8.Valid(log) {
		err = errors.New("the log is not UTF-8")
	}
	if err != nil {
		logger.Warn("the SBOM of a scan could not be read", "pod", pod.Name, "error", err)
		return report, none, nil
	}
	return report, SbomOf(string(log)), nil
}
