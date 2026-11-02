package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TanmayDoesAI/tailreach/web"
)

// The offline page and its service worker are built in, routed in every mode (a route
// clash would panic here), and reachable without a session: the browser fetches them for itself.
func TestOfflinePage(t *testing.T) {
	for _, f := range []string{"sw.js", "offline.html"} {
		if _, err := fs.ReadFile(web.FS, f); err != nil {
			t.Fatalf("%s isn't built in: %v", f, err)
		}
	}
	for _, hub := range []bool{true, false} {
		a := &app{opt: options{hub: hub, name: "mac"}, web: web.FS, auth2: &authState{sessions: map[string]*session{}}, sec: &security{}}
		a.handler()
	}
	a := &app{auth2: &authState{sessions: map[string]*session{}}, sec: &security{}}
	h := a.requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }))
	for path, open := range map[string]bool{"/sw.js": true, "/offline.html": true, "/": false, "/mac/": false} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if (w.Code == 299) != open {
			t.Errorf("%s without a session: %d", path, w.Code)
		}
	}
}
