package confluence

import "testing"

func TestResolveLinks_ByID(t *testing.T) {
	r := NewMapResolver()
	r.Record("12345", "OPS", "Runbook", "https://x/wiki/spaces/OPS/pages/12345")
	in := "See ⟦pg-id:12345|Runbook⟧ for details."
	out := ResolveLinks(in, r)
	want := "See [Runbook](https://x/wiki/spaces/OPS/pages/12345) for details."
	if out != want {
		t.Errorf("got %q want %q", out, want)
	}
}

func TestResolveLinks_ByTitleInSameSpace(t *testing.T) {
	r := NewMapResolver()
	r.Record("999", "OPS", "Target", "https://x/wiki/spaces/OPS/pages/999")
	in := "⟦pg-title:OPS|Target|Target⟧"
	out := ResolveLinks(in, r)
	want := "[Target](https://x/wiki/spaces/OPS/pages/999)"
	if out != want {
		t.Errorf("got %q want %q", out, want)
	}
}

func TestResolveLinks_ByTitle_EmptySpaceKeyMatchesAny(t *testing.T) {
	r := NewMapResolver()
	r.Record("42", "OPS", "Credential Rotation", "https://x/wiki/spaces/OPS/pages/42")
	in := "⟦pg-title:|Credential Rotation|Credential Rotation⟧"
	out := ResolveLinks(in, r)
	want := "[Credential Rotation](https://x/wiki/spaces/OPS/pages/42)"
	if out != want {
		t.Errorf("got %q want %q", out, want)
	}
}

func TestResolveLinks_UnresolvedDegradesToLabel(t *testing.T) {
	r := NewMapResolver()
	in := "See ⟦pg-id:999|Missing⟧ and ⟦pg-title:ENG|Gone|Gone⟧ instead."
	out := ResolveLinks(in, r)
	want := "See Missing and Gone instead."
	if out != want {
		t.Errorf("got %q want %q", out, want)
	}
}

func TestResolveLinks_NoSentinelsNoChange(t *testing.T) {
	r := NewMapResolver()
	in := "Just some text with a [real link](https://example.com)."
	out := ResolveLinks(in, r)
	if out != in {
		t.Errorf("unexpected rewrite: %q", out)
	}
}
