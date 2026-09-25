package api_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// A preview shows its countdown only while it is active, and its closing
// once it has closed (PreviewDto::of).
func TestPreviewsAsTheAPIShowsThem(t *testing.T) {
	active := store.PreviewRecord{
		Environment: "pr12-1", Repository: "acme/shop", PRNumber: 12, PreviewEpoch: 1,
		HeadRepository: "acme/shop", Branch: "feature", CommitSha: "89abcdef0123456789abcdef0123456789abcdef",
		Trusted: true, State: "active", AutoDelete: true, ExpiresAt: 7_201_500, CreatedAt: 0,
	}
	closed := active
	closed.State, closed.ClosedAt, closed.CloseReason = "closed", opt.Some[int64](1_000), opt.Some("manual")
	expired := active
	expired.ExpiresAt = 500
	cases := []struct {
		name string
		p    store.PreviewRecord
		want map[string]any
	}{
		{"active", active, map[string]any{
			"environment": "pr12-1", "repository": "acme/shop", "pullRequest": 12.0, "epoch": 1.0,
			"headRepository": "acme/shop", "branch": "feature", "commit": "89abcdef0123456789abcdef0123456789abcdef",
			"trusted": true, "state": "active", "autoDelete": true, "expiresAt": "1970-01-01T02:00:01.5Z",
			"remainingSeconds": 7200.0, "createdAt": "1970-01-01T00:00:00Z", "closedAt": nil, "closeReason": nil,
		}},
		{"closed", closed, map[string]any{
			"environment": "pr12-1", "repository": "acme/shop", "pullRequest": 12.0, "epoch": 1.0,
			"headRepository": "acme/shop", "branch": "feature", "commit": "89abcdef0123456789abcdef0123456789abcdef",
			"trusted": true, "state": "closed", "autoDelete": true, "expiresAt": "1970-01-01T02:00:01.5Z",
			"remainingSeconds": 0.0, "createdAt": "1970-01-01T00:00:00Z", "closedAt": "1970-01-01T00:00:01Z",
			"closeReason": "manual",
		}},
	}
	for _, c := range cases {
		dto := api.PreviewDtoOf(c.p, 1_000)
		raw, err := dto.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(c.want, got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", c.name, diff)
		}
	}
	if got := api.PreviewDtoOf(expired, 1_000).RemainingSeconds; got != 0 {
		t.Errorf("expired: %d seconds left", got)
	}
}

// The preview settings' lifetime and limit, with their defaults and bounds
// (routes/previews.rs put_policy).
func TestPreviewSettingsAreBounded(t *testing.T) {
	cases := []struct {
		name           string
		ttl, maxActive gen.OptInt32
		wantTTL        uint32
		wantMax        uint32
		wantErr        string
	}{
		{name: "defaults", wantTTL: 72, wantMax: 10},
		{name: "set", ttl: gen.NewOptInt32(2), maxActive: gen.NewOptInt32(5), wantTTL: 2, wantMax: 5},
		{name: "longest", ttl: gen.NewOptInt32(720), maxActive: gen.NewOptInt32(100), wantTTL: 720, wantMax: 100},
		{name: "no lifetime", ttl: gen.NewOptInt32(0), wantErr: "validation failed: ttlHours must be 1 to 720"},
		{name: "too long", ttl: gen.NewOptInt32(721), wantErr: "validation failed: ttlHours must be 1 to 720"},
		{name: "none allowed", maxActive: gen.NewOptInt32(0), wantErr: "validation failed: maxActive must be 1 to 100"},
		{name: "too many", maxActive: gen.NewOptInt32(101), wantErr: "validation failed: maxActive must be 1 to 100"},
	}
	for _, c := range cases {
		ttl, maxActive, err := api.CheckPreviewPolicy(&gen.PutPreviewPolicy{
			Enabled: true, SourceEnvironment: "prod", TtlHours: c.ttl, MaxActive: c.maxActive,
		})
		if c.wantErr != "" {
			if err == nil || err.Error() != c.wantErr || !errors.Is(err, kerr.ErrValidation) {
				t.Errorf("%s: %v, want %q", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil || ttl != c.wantTTL || maxActive != c.wantMax {
			t.Errorf("%s: %d %d %v", c.name, ttl, maxActive, err)
		}
	}
}
