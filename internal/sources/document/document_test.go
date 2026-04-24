package document

import (
	"strings"
	"testing"
)

func TestDetectKind(t *testing.T) {
	cases := []struct {
		ct, name string
		want     Kind
		wantErr  bool
	}{
		{"text/markdown", "x.md", KindMarkdown, false},
		{"text/markdown; charset=utf-8", "x.md", KindMarkdown, false},
		{"text/plain", "x.txt", KindText, false},
		{"text/html", "x.html", KindHTML, false},
		{"application/xhtml+xml", "x.xhtml", KindHTML, false},
		{"application/pdf", "x.pdf", KindPDF, false},
		{"application/octet-stream", "notes.md", KindMarkdown, false},
		{"application/octet-stream", "data.txt", KindText, false},
		{"application/octet-stream", "book.pdf", KindPDF, false},
		{"application/octet-stream", "page.htm", KindHTML, false},
		{"application/octet-stream", "weird.bin", "", true},
		{"application/zip", "archive.zip", "", true},
	}
	for _, c := range cases {
		got, err := DetectKind(c.ct, c.name)
		if c.wantErr {
			if err == nil {
				t.Errorf("DetectKind(%q,%q) expected error, got %q", c.ct, c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("DetectKind(%q,%q) unexpected error: %v", c.ct, c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("DetectKind(%q,%q) = %q want %q", c.ct, c.name, got, c.want)
		}
	}
}

func TestExtract_MarkdownPassThroughAndTitle(t *testing.T) {
	md := "# Credential rotation\n\nStep one.\n\nStep two.\n"
	doc, err := Extract([]byte(md), KindMarkdown, "runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Credential rotation" {
		t.Errorf("title: got %q", doc.Title)
	}
	if doc.Body != md {
		t.Errorf("body not passed through verbatim")
	}
	if doc.Extraction != KindMarkdown {
		t.Errorf("extraction: got %q", doc.Extraction)
	}
}

func TestExtract_TextFallbackTitleFromFilename(t *testing.T) {
	doc, err := Extract([]byte("no heading, just words.\n"), KindText, "my-notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	// First non-empty line becomes the title for plaintext.
	if doc.Title != "no heading, just words." {
		t.Errorf("title: got %q", doc.Title)
	}
}

func TestExtract_HTMLStripsScriptsAndGrabsTitle(t *testing.T) {
	html := `<!doctype html><html><head><title>Doc Title</title>
	<script>alert('x')</script></head>
	<body><h1>Heading</h1><p>Paragraph <b>with</b> emphasis.</p>
	<script>bad()</script><style>.x{}</style></body></html>`
	doc, err := Extract([]byte(html), KindHTML, "page.html")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Doc Title" {
		t.Errorf("title: got %q", doc.Title)
	}
	if strings.Contains(doc.Body, "alert") || strings.Contains(doc.Body, ".x{}") {
		t.Errorf("script/style content leaked into body: %q", doc.Body)
	}
	if !strings.Contains(doc.Body, "Paragraph") {
		t.Errorf("body missing real text: %q", doc.Body)
	}
}

func TestExtract_UnsupportedKind(t *testing.T) {
	_, err := Extract([]byte("x"), Kind("sneaky"), "x.bin")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestIsLikelyScanned(t *testing.T) {
	if !isLikelyScanned("", 1) {
		t.Error("empty text with 1 page should look scanned")
	}
	if !isLikelyScanned("abc", 5) {
		t.Error("3 letters across 5 pages should look scanned")
	}
	if isLikelyScanned("plenty of real words here we go", 1) {
		t.Error("obvious text should not be flagged scanned")
	}
}

func TestDeriveTitle_FilenameFallback(t *testing.T) {
	got := deriveTitle("", "plans/Q4-roadmap.pdf", KindPDF)
	if got != "Q4-roadmap" {
		t.Errorf("got %q want Q4-roadmap", got)
	}
}
