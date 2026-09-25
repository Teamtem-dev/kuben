package ui

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestPlainOutputHasNoEscapeCodes(t *testing.T) {
	plain := With(io.Discard, false)
	if got := plain.paint(green, "ok"); got != "ok" {
		t.Errorf("plain %q", got)
	}
	tty := With(io.Discard, true)
	if got := tty.paint(green, "ok"); got != "\x1b[32mok\x1b[0m" {
		t.Errorf("tty %q", got)
	}
	if got := tty.paint(tty.accent, "❯"); got != "\x1b[38;5;208m❯\x1b[0m" {
		t.Errorf("orange, not blue: %q", got)
	}

	var out bytes.Buffer
	u := With(&out, false)
	u.Done("Installed k3s.", "v1.36.4+k3s1")
	u.Warn("Firewall", "")
	u.Fail("Service", "exit 1")
	u.Note("first\r\nsecond\n")
	u.Command("systemctl restart kuben")
	u.Heading("Next")
	u.Blank()
	step := u.Step("Writing the config")
	step.Command("install -m 600")
	step.Done("")
	want := "✔ Installed k3s. v1.36.4+k3s1\n" +
		"⚠ Firewall.\n" +
		"✖ Service. exit 1\n" +
		"  first\n  second\n" +
		"❯ systemctl restart kuben\n" +
		"\nNext\n" +
		"\n" +
		"… Writing the config\n" +
		"  ❯ install -m 600\n" +
		"✔ Writing the config.\n"
	if got := out.String(); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Error("escape codes without a terminal")
	}
}

func TestAStepCanBeDroppedUnfinished(t *testing.T) {
	var plainOut bytes.Buffer
	plain := With(&plainOut, false)
	plain.Step("Doing something").Close()
	plain.Step("Doing something else").Done("fine")
	if got := plainOut.String(); got != "… Doing something\n… Doing something else\n✔ Doing something else. fine\n" {
		t.Errorf("plain %q", got)
	}

	var ttyOut bytes.Buffer
	tty := With(&ttyOut, true)
	step := tty.Step("Spinning")
	step.Close()
	step.Close() // closing twice is harmless
	if got := ttyOut.String(); !strings.HasSuffix(got, clearLine) {
		t.Errorf("the spinner line is not cleared: %q", got)
	}
	ttyOut.Reset()
	tty.Step("Spinning again").Warn("slow")
	if got := ttyOut.String(); !strings.HasSuffix(got, clearLine+"\x1b[33m⚠\x1b[0m Spinning again. \x1b[2mslow\x1b[0m\n") {
		t.Errorf("finished %q", got)
	}
}

func TestTheSpinnerShowsTheCommandAndTheTime(t *testing.T) {
	s := &Step{ui: With(io.Discard, true), label: "Installing k3s"}
	if got := s.frame("⠋", time.Second); got != clearLine+orange256+"⠋"+reset+" Installing k3s" {
		t.Errorf("short %q", got)
	}
	s.Command("curl -sfL https://get.k3s.io")
	want := clearLine + orange256 + "⠙" + reset + " Installing k3s " + orange256 + "❯" + reset + " " + dim +
		"curl -sfL https://get.k3s.io" + reset + " " + dim + "41s" + reset
	if got := s.frame("⠙", 41*time.Second+500*time.Millisecond); got != want {
		t.Errorf("long %q", got)
	}
}

func TestLongStepsShowHowLongTheyTook(t *testing.T) {
	tests := []struct {
		detail  string
		elapsed time.Duration
		want    string
	}{
		{"v1.36.4+k3s1", time.Second, "v1.36.4+k3s1"},
		{"v1.36.4+k3s1", 41 * time.Second, "v1.36.4+k3s1 · 41s"},
		{"", 75 * time.Second, "1m15s"},
	}
	for _, tt := range tests {
		if got := withElapsed(tt.detail, tt.elapsed); got != tt.want {
			t.Errorf("withElapsed(%q, %s) = %q, want %q", tt.detail, tt.elapsed, got, tt.want)
		}
	}
}

func TestPromptsTakeTheDefaultOnEnter(t *testing.T) {
	tests := []struct {
		name, typed string
		tty         bool
		want        string
		prompt      string
	}{
		{"enter", "\n", false, "kuben.example.com", "? Domain › kuben.example.com "},
		{"answer", "  shop.example.com \n", false, "shop.example.com", "? Domain › kuben.example.com "},
		{"end of input", "", false, "kuben.example.com", "? Domain › kuben.example.com "},
		{"coloured", "\n", true, "kuben.example.com", orange256 + "?" + reset + " Domain " + dim + "› kuben.example.com" + reset + " "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var shown bytes.Buffer
			tty := struct {
				io.Reader
				io.Writer
			}{strings.NewReader(tt.typed), &shown}
			got, ok := With(io.Discard, tt.tty).askOn(tty, "Domain", "kuben.example.com")
			if !ok || got != tt.want {
				t.Errorf("answer %q %v", got, ok)
			}
			if shown.String() != tt.prompt {
				t.Errorf("prompt %q", shown.String())
			}
		})
	}
	for answer, yes := range map[string]bool{"y": true, "YES": true, "n": false, "N": false, "sure": false} {
		if isYes(answer) != yes {
			t.Errorf("isYes(%q)", answer)
		}
	}
	for answer, no := range map[string]bool{"n": true, "No": true, "Y": false, "": false} {
		if isNo(answer) != no {
			t.Errorf("isNo(%q)", answer)
		}
	}
}
