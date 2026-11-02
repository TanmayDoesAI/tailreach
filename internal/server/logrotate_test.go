package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotateFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	os.WriteFile(p, make([]byte, 100), 0o600)
	rotateFile(p, 200)
	if _, err := os.Stat(p + ".1"); err == nil {
		t.Fatal("rotated a log under the limit")
	}
	os.WriteFile(p, make([]byte, 300), 0o600)
	rotateFile(p, 200)
	if fi, err := os.Stat(p + ".1"); err != nil || fi.Size() != 300 {
		t.Fatalf("over the limit: want it moved to .1, got %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the log should start again empty")
	}
}
