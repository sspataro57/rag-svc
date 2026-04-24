package ingest

import (
	"testing"
	"time"
)

func TestBuildJQL_AllProjectsNoWatermark(t *testing.T) {
	got := buildJQL(nil, time.Time{})
	want := "order by updated ASC"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestBuildJQL_WithProjectsAndWatermark(t *testing.T) {
	w := time.Date(2026, 4, 20, 10, 30, 0, 0, time.UTC)
	got := buildJQL([]string{"PLAT", "OPS"}, w)
	// Watermark is shifted back by the slack; allow some flexibility on
	// exact minutes by checking substring shape.
	wantSubs := []string{
		`project in ("PLAT","OPS")`,
		`updated >= "2026-04-20 10:29"`,
		`order by updated ASC`,
	}
	for _, s := range wantSubs {
		if !contains(got, s) {
			t.Errorf("buildJQL missing %q in %q", s, got)
		}
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
