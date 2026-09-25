package firstrun_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/firstrun"
)

// The setup link carries the token in its fragment, never in the query
// (setup.rs setup_url).
func TestSetupLinks(t *testing.T) {
	if firstrun.SetupURLAt("http://localhost:3000/", opt.None[string]()) != "http://localhost:3000/setup" ||
		firstrun.SetupURLAt("https://k.example", opt.Some("t0k")) != "https://k.example/setup#token=t0k" {
		t.Fatal("setup links")
	}
}
