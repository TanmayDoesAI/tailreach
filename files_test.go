package main

import (
	"strings"
	"testing"
)

func TestCleanFileName(t *testing.T) {
	for in, want := range map[string]string{
		"photo.jpg":          "photo.jpg",
		"../../etc/passwd":   "passwd",
		"..":                 "",
		".hidden":            "hidden",
		"a\x00b:c\\d.txt":    "a-b-c-d.txt",
		"  spaced name.pdf ": "spaced name.pdf",
	} {
		got := cleanFileName(in)
		if want == "" {
			if !strings.HasPrefix(got, "file from ") {
				t.Errorf("%q → %q, want a dated name", in, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if n := cleanFileName(strings.Repeat("é", 300) + ".txt"); len(n) > 200 || !strings.HasSuffix(n, ".txt") {
		t.Errorf("long name: %d bytes, %q", len(n), n[len(n)-8:])
	}
}
