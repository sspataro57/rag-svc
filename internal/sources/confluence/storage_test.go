package confluence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageGoldenFiles(t *testing.T) {
	entries, err := os.ReadDir("testdata/storage")
	if err != nil {
		t.Fatalf("read testdata/storage: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".xhtml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".xhtml")
		t.Run(name, func(t *testing.T) {
			in, err := os.ReadFile(filepath.Join("testdata/storage", name+".xhtml"))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata/storage", name+".md"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := StorageToMarkdown(string(in))
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			if got != string(want) {
				t.Errorf("mismatch for %s\n--- want ---\n%s--- got ---\n%s", name, want, got)
			}
		})
	}
}

func TestStorageToMarkdown_Empty(t *testing.T) {
	got, err := StorageToMarkdown("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("empty input should produce empty output, got %q", got)
	}
}
