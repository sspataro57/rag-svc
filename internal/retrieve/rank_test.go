package retrieve

import (
	"slices"
	"testing"
)

func TestExtractTicketKeys(t *testing.T) {
	cases := []struct {
		in  string
		out []string
	}{
		{"PLAT-482", []string{"PLAT-482"}},
		{"  PLAT-482  ", []string{"PLAT-482"}},
		{"SANDBOX-1", []string{"SANDBOX-1"}},
		{"A1-9", []string{"A1-9"}},
		{"ABC_DEF-123", []string{"ABC_DEF-123"}},
		{"", nil},
		{"plat-482", nil},  // must be uppercase
		{"PLAT482", nil},   // missing hyphen
		{"PLAT-", nil},     // missing number
		{"-482", nil},      // missing prefix
		{"1PLAT-482", nil}, // must start with letter
		{"X-1", nil},       // project needs two characters
		{"PLAT-482 extra text", []string{"PLAT-482"}},
		{"how do we set up PLAT-482?", []string{"PLAT-482"}},
		{"What is WEB-10500 about and what is its current status?", []string{"WEB-10500"}},
		{"does API-4491 block WEB-10501, or is WEB-10501 separate?", []string{"API-4491", "WEB-10501"}},
		{"(see OPS-7).", []string{"OPS-7"}},
		{"A-1 B-2 AA-1 AA-2 AA-3 AA-4 AA-5 AA-6", []string{"AA-1", "AA-2", "AA-3", "AA-4", "AA-5"}}, // capped
	}
	for _, c := range cases {
		got := ExtractTicketKeys(c.in)
		if !slices.Equal(got, c.out) {
			t.Errorf("ExtractTicketKeys(%q): got %q want %q", c.in, got, c.out)
		}
	}
}
