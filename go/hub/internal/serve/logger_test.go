package serve_test

import (
	"log/slog"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/serve"
)

func TestTheLogLevelIsTheGlobalDirective(t *testing.T) {
	for directives, want := range map[string]slog.Level{
		"":                                    slog.LevelInfo,
		"info":                                slog.LevelInfo,
		"debug,hyper=info,h2=info,tower=info": slog.LevelDebug,
		"kuben=debug":                         slog.LevelInfo,
		"warn":                                slog.LevelWarn,
		"TRACE":                               slog.LevelDebug,
		"error":                               slog.LevelError,
	} {
		if got := serve.LogLevel(directives); got != want {
			t.Errorf("%q: %v, want %v", directives, got, want)
		}
	}
}
