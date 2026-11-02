package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// A device behind the hub must never see the browser's hub cookies, nor set any.
func TestDeviceProxyStripsCredentials(t *testing.T) {
	var got http.Header
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Set-Cookie", "md_session=evil; Path=/")
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<script>steal()</script>"))
	}))
	defer dev.Close()
	host := strings.TrimPrefix(dev.URL, "http://")

	r := httptest.NewRequest("GET", "https://devices.example.ts.net/mac2/caps", nil)
	r.Header.Set("Cookie", "md_session=secret; md_trust=secret2")
	r.Header.Set("Authorization", "Bearer x")
	r.Header.Set("X-Tailreach-Viewer", "forged")
	w := httptest.NewRecorder()
	deviceProxy("mac2", host, "phone", http.DefaultTransport).ServeHTTP(w, r)

	if got == nil {
		t.Fatalf("not forwarded: %d %s", w.Code, w.Body)
	}
	if got.Get("Cookie") != "" || got.Get("Authorization") != "" {
		t.Errorf("credentials forwarded: %v", got)
	}
	if got.Get("X-Tailreach-Hub") != "1" || got.Get("X-Tailreach-Viewer") != "phone" {
		t.Errorf("hub headers: hub=%q viewer=%q", got.Get("X-Tailreach-Hub"), got.Get("X-Tailreach-Viewer"))
	}
	res := w.Result()
	if len(res.Cookies()) != 0 || res.Header.Get("Set-Cookie") != "" {
		t.Errorf("device set a cookie on the hub: %v", res.Header)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("device HTML served as %q", ct)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("no sandbox CSP: %q", csp)
	}
}

func TestStripProxyResponseKeepsJSON(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"a=b"}}}
	stripProxyResponse(resp)
	if resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("%v", resp.Header)
	}
}

// The hub serves its own viewer for /<name>/…, and forwards only the dynamic endpoints,
// only to the node pinned for that name.
func TestServeDeviceRouting(t *testing.T) {
	var forwarded []string
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = append(forwarded, r.URL.Path)
		w.Write([]byte("from device"))
	}))
	defer dev.Close()
	host := strings.TrimPrefix(dev.URL, "http://")

	a := &app{
		web: fstest.MapFS{
			"viewer/index.html":      {Data: []byte("hub viewer")},
			"viewer/app.js":          {Data: []byte("hub js")},
			"viewer/novnc/core/x.js": {Data: []byte("hub novnc")},
			"viewer/classic/app.js":  {Data: []byte("hub classic")},
		},
		devices: map[string]*device{"mac2": {Name: "mac2", Online: true, IP: host, Node: "nGood"}},
	}
	nodeAt := "nGood"
	a.nodeAt = func(_ context.Context, ip string) string {
		if ip != host {
			t.Errorf("WhoIs on %q, want %q", ip, host)
		}
		return nodeAt
	}
	files := a.viewerFiles()
	get := func(path string) (int, string) {
		r := httptest.NewRequest("GET", "https://devices.example.ts.net"+path, nil)
		w := httptest.NewRecorder()
		a.serveDevice(w, r, "mac2", files)
		b, _ := io.ReadAll(w.Result().Body)
		return w.Code, string(b)
	}

	for path, want := range map[string]string{
		"/mac2/":                "hub viewer",
		"/mac2/app.js":          "hub js",
		"/mac2/novnc/core/x.js": "hub novnc",
		"/mac2/classic/app.js":  "hub classic",
	} {
		if code, body := get(path); code != 200 || body != want {
			t.Errorf("%s: %d %q, want the hub's own %q", path, code, body, want)
		}
	}
	if len(forwarded) != 0 {
		t.Errorf("static files went to the device: %v", forwarded)
	}

	// The dynamic endpoints go to the pinned device, without the path rewritten.
	for _, p := range []string{"/mac2/caps", "/mac2/stream", "/mac2/upload"} {
		if code, body := get(p); code != 200 || body != "from device" {
			t.Errorf("%s: %d %q, want it forwarded", p, code, body)
		}
	}
	if strings.Join(forwarded, ",") != "/mac2/caps,/mac2/stream,/mac2/upload" {
		t.Errorf("forwarded %v", forwarded)
	}
	forwarded = nil

	// Another machine now holds the device's address: refused, nothing forwarded.
	nodeAt = "nEvil"
	for _, p := range []string{"/mac2/caps", "/mac2/stream", "/mac2/websockify", "/mac2/upload"} {
		if code, _ := get(p); code != http.StatusBadGateway {
			t.Errorf("%s to an unpinned node: %d", p, code)
		}
	}
	if len(forwarded) != 0 {
		t.Errorf("forwarded to an unpinned node: %v", forwarded)
	}

	// Unknown and offline devices.
	if code, _ := (func() (int, string) {
		r := httptest.NewRequest("GET", "/mac3/caps", nil)
		w := httptest.NewRecorder()
		a.serveDevice(w, r, "mac3", files)
		return w.Code, ""
	})(); code != http.StatusServiceUnavailable {
		t.Errorf("unknown device: %d", code)
	}
}

func TestPinNode(t *testing.T) {
	a := &app{}
	a.nodePins, a.pinWarned = map[string]string{}, map[string]bool{}
	peers := map[string]bool{"n1": true, "n2": true}
	if !a.pinNode("mac2", "n1", "h1", peers) {
		t.Fatal("first sighting not pinned")
	}
	if !a.pinNode("mac2", "n1", "h1", peers) {
		t.Fatal("pinned node refused")
	}
	if a.pinNode("mac2", "n2", "h2", peers) {
		t.Fatal("another node took a pinned name")
	}
	delete(peers, "n1") // the old machine was removed from the tailnet
	if !a.pinNode("mac2", "n2", "h2", peers) || a.nodePins["mac2"] != "n2" {
		t.Fatal("pin didn't move after the old machine left the tailnet")
	}
	if a.pinNode("mac2", "", "h", peers) {
		t.Fatal("empty node accepted")
	}
}
