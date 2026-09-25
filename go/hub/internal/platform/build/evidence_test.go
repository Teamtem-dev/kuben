package build_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/scan"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
)

// Ported from evidence.rs.

const (
	begin = "kuben-sbom-begin\n"
	end   = "\nkuben-sbom-end"
)

func TestSbomsAreTakenFromBetweenTheMarkers(t *testing.T) {
	gz := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3, 1, 2, 3}
	encoded := base64.StdEncoding.EncodeToString(gz)
	log := "Downloading DB...\n" + begin + encoded + "\n" + end[1:] + "\nafter"
	if got, ok := build.SbomOf(log).Get(); !ok || !bytes.Equal(got, gz) {
		t.Errorf("sbom %v", got)
	}
	cases := map[string]string{
		"no markers":  "no markers",
		"not gzip":    begin + base64.StdEncoding.EncodeToString([]byte("plain json")) + end,
		"broken":      begin + "!!!" + end,
		"truncated":   begin + encoded,
		"line breaks": begin + encoded[:4] + "\n" + encoded[4:] + end,
	}
	for name, log := range cases {
		if got := build.SbomOf(log); got.IsSome() {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestReportsBecomeSummaries(t *testing.T) {
	ok := build.ReportOf(opt.Some(`{"status":"ok","scanner":"trivy 0.74.0","db":"2026-09-17T06:00:00Z",
		"counts":{"critical":1,"high":0,"medium":3,"low":0,"unknown":0},
		"findings":["CRITICAL:CVE-2026-1"]}`))
	s := build.SummaryOf(ok, 42)
	if s.Status != scan.StatusOK || s.DBUpdatedAt != opt.Some[int64](1_789_624_800_000) {
		t.Errorf("summary %+v", s)
	}
	if diff := cmp.Diff([]scan.Finding{{Severity: scan.SeverityCritical, ID: "CVE-2026-1"}}, s.Findings); diff != "" {
		t.Errorf("findings (-want +got):\n%s", diff)
	}
	if s.Counts.Critical != 1 || s.Counts.Medium != 3 || s.ScannedAt != 42 {
		t.Errorf("counts %+v at %d", s.Counts, s.ScannedAt)
	}
	for _, message := range []opt.Val[string]{opt.None[string](), opt.Some(""), opt.Some("not json")} {
		r := build.ReportOf(message)
		if r.Status != scan.StatusUnavailable || build.SummaryOf(r, 1).Counts.Critical != 0 {
			t.Errorf("%v: %+v", message, r)
		}
	}
	unavailable := build.ReportOf(opt.Some(`{"status":"unavailable","scanner":"trivy","detail":"no feed"}`))
	if unavailable.Detail != opt.Some("no feed") {
		t.Errorf("detail %v", unavailable.Detail)
	}
}

// The report the Rust scan script writes: its sed turns the database date
// into the byte 0x01, which no JSON reader accepts, so such a report reads
// as unavailable in Rust and in Go alike.
func TestAReportWithAControlByteDoesNotParse(t *testing.T) {
	r := build.ReportOf(opt.Some("{\"status\":\"ok\",\"scanner\":\"trivy 0.74.0\",\"db\":\"\x01\"," +
		"\"counts\":{\"critical\":0,\"high\":0,\"medium\":0,\"low\":0,\"unknown\":0},\"findings\":[]}"))
	if r.Status != scan.StatusUnavailable || r.Detail != opt.Some("the scan report does not parse") {
		t.Errorf("report %+v", r)
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func scanPod(t *testing.T, name, created string, terminated *corev1.ContainerStateTerminated) *corev1.Pod {
	t.Helper()
	ts := metav1.NewTime(time.UnixMilli(ms(t, created)))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "kuben-builds", CreationTimestamp: ts,
			Labels: map[string]string{"kuben.dev/rescan": "x"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "scan", State: corev1.ContainerState{Terminated: terminated},
		}}},
	}
}

func TestCollectReadsTheNewestScanPod(t *testing.T) {
	report := `{"status":"ok","scanner":"trivy","counts":{"critical":2,"high":0,"medium":0,"low":0,"unknown":0},"findings":[]}`
	client := fake.NewClientset(
		scanPod(t, "old", "2026-09-17T10:00:00Z", &corev1.ContainerStateTerminated{Message: `{"status":"unavailable","scanner":"x"}`}),
		scanPod(t, "new", "2026-09-17T10:05:00Z", &corev1.ContainerStateTerminated{Message: report}),
	)
	pods := client.CoreV1().Pods("kuben-builds")
	got, sbom, err := build.Collect(t.Context(), pods, "kuben.dev/rescan=x", discard())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != scan.StatusOK || got.Counts.Critical != 2 {
		t.Errorf("report %+v", got)
	}
	// The fake client's log is "fake logs": no SBOM in it.
	if sbom.IsSome() {
		t.Errorf("sbom %v", sbom)
	}
	missing, _, err := build.Collect(t.Context(), pods, "kuben.dev/rescan=none", discard())
	if err != nil || missing.Detail != opt.Some("the scan pod is gone") {
		t.Errorf("no pod: %+v, %v", missing, err)
	}
	running := fake.NewClientset(scanPod(t, "p", "2026-09-17T10:00:00Z", nil)).CoreV1().Pods("kuben-builds")
	notRun, _, err := build.Collect(t.Context(), running, "kuben.dev/rescan=x", discard())
	if err != nil || notRun.Detail != opt.Some("the scan did not run") {
		t.Errorf("not run: %+v, %v", notRun, err)
	}
}
