package ingest

import (
	"testing"
	"time"
)

func TestBuildJQL_AllProjectsNoWatermark(t *testing.T) {
	got := buildJQL(nil, time.Time{}, time.Now())
	// Atlassian Cloud rejects unbounded JQL; we inject an epoch floor.
	want := `updated >= "1970-01-01 00:00" order by updated ASC`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestBuildJQL_WithProjectsAndWatermark(t *testing.T) {
	w := time.Date(2026, 4, 20, 10, 30, 0, 0, time.UTC)
	now := w.Add(2 * time.Hour)
	got := buildJQL([]string{"PLAT", "OPS"}, w, now)
	// 120 minutes since the watermark plus the one-minute slack.
	want := `project in ("PLAT","OPS") AND updated >= -121m order by updated ASC`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// The bound must be a relative offset: an absolute date-time is read in the
// Jira account's timezone, which is how updates were skipped before.
func TestBuildJQL_WatermarkBoundIsTimezoneFree(t *testing.T) {
	now := time.Date(2026, 10, 9, 19, 12, 30, 0, time.UTC)
	cases := []struct {
		name      string
		watermark time.Time
		want      string
	}{
		{"same instant in another zone", time.Date(2026, 10, 9, 6, 14, 0, 0, time.FixedZone("PDT", -7*3600)), "updated >= -360m"},
		{"partial minute rounds up", time.Date(2026, 10, 9, 13, 14, 38, 0, time.UTC), "updated >= -359m"},
		{"backfill months back", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), "updated >= -232994m"},
		{"watermark ahead of now", now.Add(10 * time.Minute), "updated >= -1m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildJQL(nil, tc.watermark, now)
			if want := tc.want + " order by updated ASC"; got != want {
				t.Errorf("got %q want %q", got, want)
			}
		})
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && indexOf(hay, needle) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
