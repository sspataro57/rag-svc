package http

import "testing"

func TestParseSourceID(t *testing.T) {
	cases := []struct {
		in            string
		wantType, key string
		wantOK        bool
	}{
		{"jira:API-2302", "jira", "API-2302", true},
		{"document:scheduling-portfolio-2026", "document", "scheduling-portfolio-2026", true},
		{"confluence:ENG-OnboardingChecklist", "confluence", "ENG-OnboardingChecklist", true},
		// Document keys may contain further colons — only the first one splits.
		{"document:ns:weird-key", "document", "ns:weird-key", true},

		{"", "", "", false},
		{"jira:", "", "", false},
		{":API-2302", "", "", false},
		{"unknown:x", "", "", false},
		{"jiraAPI-2302", "", "", false},
	}
	for _, c := range cases {
		gotType, gotKey, gotOK := parseSourceID(c.in)
		if gotType != c.wantType || gotKey != c.key || gotOK != c.wantOK {
			t.Errorf("parseSourceID(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, gotType, gotKey, gotOK, c.wantType, c.key, c.wantOK)
		}
	}
}
