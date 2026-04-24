package retrieve

import "testing"

func TestParseTicketKey(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"PLAT-482", "PLAT-482"},
		{"  PLAT-482  ", "PLAT-482"},
		{"SANDBOX-1", "SANDBOX-1"},
		{"A1-9", "A1-9"},
		{"ABC_DEF-123", "ABC_DEF-123"},
		{"", ""},
		{"plat-482", ""},            // must be uppercase
		{"PLAT482", ""},             // missing hyphen
		{"PLAT-", ""},               // missing number
		{"-482", ""},                // missing prefix
		{"1PLAT-482", ""},           // must start with letter
		{"PLAT-482 extra text", ""}, // must match whole string
		{"how do we set up PLAT-482?", ""},
	}
	for _, c := range cases {
		got := ParseTicketKey(c.in)
		if got != c.out {
			t.Errorf("ParseTicketKey(%q): got %q want %q", c.in, got, c.out)
		}
	}
}
