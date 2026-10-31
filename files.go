package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Files sent from the viewer (dropped on the screen, or picked in the panel) land in the
// Mac's Downloads folder. That is all the file access a viewer has: it can add a new file
// there, never read, list, open, replace or delete one.

const maxUpload = 4 << 30 // 4 GB

func downloadsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads")
}

// receiveFile takes POST /upload?name=… with the file as the body.
func (a *app) receiveFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Only the viewer page itself may send (the session cookie is SameSite=Strict too).
	if o, err := url.Parse(r.Header.Get("Origin")); err != nil || !strings.HasSuffix(o.Hostname(), "."+a.domain) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if streamsBlocked() {
		http.Error(w, "stopped on the Mac", http.StatusLocked)
		return
	}
	if r.ContentLength > maxUpload {
		http.Error(w, "too big: up to 4 GB", http.StatusRequestEntityTooLarge)
		return
	}
	name := cleanFileName(r.URL.Query().Get("name"))
	f, path, err := createNew(downloadsDir(), name)
	if err != nil {
		log.Printf("file from viewer: %v", err)
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, errSlow) {
			http.Error(w, "the Mac doesn't let mydevices save to Downloads: run `mydevices doctor --ask` on the Mac", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "couldn't save the file", http.StatusInternalServerError)
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxUpload))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		http.Error(w, "the upload didn't finish", http.StatusBadRequest)
		return
	}
	who := a.viewerName(r)
	base := filepath.Base(path)
	log.Printf("▶ received %s (%d KB) from %s", base, n/1000, who)
	a.audit("file-received", who, fmt.Sprintf("%s, %d bytes", base, n))
	notify("Received "+base, "from "+who+", in Downloads")
	writeJSON(w, map[string]any{"name": base, "size": n})
}

var errSlow = errors.New("Downloads didn't answer (macOS may be asking for permission on the Mac)")

// createNew makes a file that doesn't exist yet: "name", else "name 2", "name 3"… It gives
// up after a few seconds: macOS may be holding the call while it asks about Downloads.
func createNew(dir, name string) (*os.File, string, error) {
	type res struct {
		f    *os.File
		path string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		for i := 1; i < 1000; i++ {
			p := filepath.Join(dir, name)
			if i > 1 {
				p = filepath.Join(dir, fmt.Sprintf("%s %d%s", stem, i, ext))
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			ch <- res{f, p, err}
			return
		}
		ch <- res{err: errors.New("too many files with that name")}
	}()
	select {
	case r := <-ch:
		return r.f, r.path, r.err
	case <-time.After(5 * time.Second):
		go func() { // answered later: don't leave an empty file behind
			if r := <-ch; r.f != nil {
				r.f.Close()
				os.Remove(r.path)
			}
		}()
		return nil, "", errSlow
	}
}

// cleanFileName keeps a file name to one plain path element.
func cleanFileName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || unicode.IsControl(r) {
			return '-'
		}
		return r
	}, filepath.Base(s))
	s = strings.TrimLeft(strings.TrimSpace(s), ".") // no hidden files, no "." or ".."
	if len(s) > 200 {
		ext := filepath.Ext(s)
		if len(ext) > 20 {
			ext = ""
		}
		s = strings.ToValidUTF8(s[:200-len(ext)], "") + ext
	}
	if s == "" {
		s = "file from " + time.Now().Format("2006-01-02 15.04.05")
	}
	return s
}

// downloadsWritable checks that mydevices may save to Downloads (`mydevices doctor --ask`):
// the first try makes macOS ask, on the Mac.
func downloadsWritable() bool {
	f, p, err := createNew(downloadsDir(), ".mydevices-check")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(p)
	return true
}
