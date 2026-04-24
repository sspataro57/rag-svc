package jira

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestADFGoldenFiles runs every .json fixture in testdata/adf and compares the
// rendered markdown against its sibling .md file. Adding a new case is as
// simple as dropping two files in that directory.
func TestADFGoldenFiles(t *testing.T) {
	entries, err := os.ReadDir("testdata/adf")
	if err != nil {
		t.Fatalf("read testdata/adf: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		t.Run(name, func(t *testing.T) {
			in, err := os.ReadFile(filepath.Join("testdata/adf", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata/adf", name+".md"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ADFToMarkdown(in)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			if got != string(want) {
				t.Errorf("mismatch for %s\n--- want ---\n%s--- got ---\n%s", name, want, got)
			}
		})
	}
}

func TestADFToMarkdown_Empty(t *testing.T) {
	got, err := ADFToMarkdown(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("want empty string, got %q", got)
	}
}
