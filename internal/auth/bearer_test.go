package auth

import "testing"

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"}, // case-insensitive scheme
		{"BEARER  abc", "abc"},
		{"Basic abc", ""},
		{"Bearer", ""},
		{"Bearer ", ""},
		{"Bearer abc, Basic def", "abc, Basic def"}, // we only split once
	}
	for _, c := range cases {
		got := extractBearer(c.in)
		if got != c.want {
			t.Errorf("extractBearer(%q) = %q want %q", c.in, got, c.want)
		}
	}
}
