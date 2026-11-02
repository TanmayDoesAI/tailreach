package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Update notice: once a day (and a minute after start) ask GitHub for the latest release,
// over the normal internet, never the tailnet. This is the only request tailreach makes
// besides Tailscale's own. It sends nothing but "tailreach/<version>" as its User-Agent,
// and it can be turned off (`--no-update-check`, or the settings window).

const (
	releasesURL      = "https://api.github.com/repos/TanmayDoesAI/tailreach/releases/latest"
	updateFirst      = time.Minute
	updateEvery      = 24 * time.Hour
	updateUpgradeHB  = "tailreach update"
	updateUpgradeGit = "git pull && make setup"
)

// updateInfo is a newer release than this build, as the control socket and dashboard show it.
type updateInfo struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

type updateChecker struct {
	url     string
	current string
	client  *http.Client
	kick    chan struct{}

	mu     sync.Mutex
	etag   string
	latest *updateInfo // the newest release GitHub reported, newer or not
}

func newUpdateChecker(current string) *updateChecker {
	return &updateChecker{
		url:     releasesURL,
		current: current,
		// The default transport: the normal internet, not the tailnet. Short timeout, no cookies.
		client: &http.Client{Timeout: 15 * time.Second},
		kick:   make(chan struct{}, 1),
	}
}

// available is the newer release, or nil if this build is the latest (or nothing is known yet).
func (u *updateChecker) available() *updateInfo {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.latest == nil || compareVersions(u.latest.Version, u.current) <= 0 {
		return nil
	}
	c := *u.latest
	return &c
}

// check asks GitHub once. A 304 (same ETag) keeps what was known.
func (u *updateChecker) check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "tailreach/"+u.current)
	req.Header.Set("Accept", "application/vnd.github+json")
	u.mu.Lock()
	if u.etag != "" {
		req.Header.Set("If-None-Match", u.etag)
	}
	u.mu.Unlock()
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	case http.StatusOK:
	case http.StatusNotFound: // no release published yet
		return nil
	default:
		return fmt.Errorf("GitHub answered %s", resp.Status)
	}
	rel, err := parseRelease(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	u.mu.Lock()
	u.etag = resp.Header.Get("ETag")
	if rel != nil {
		u.latest = rel
	}
	u.mu.Unlock()
	return nil
}

// parseRelease reads GitHub's "latest release" reply. Drafts, pre-releases and tags that
// aren't plain x.y.z come back as nil.
func parseRelease(r io.Reader) (*updateInfo, error) {
	var rel struct {
		TagName    string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(r).Decode(&rel); err != nil {
		return nil, err
	}
	v := strings.TrimPrefix(rel.TagName, "v")
	if rel.Draft || rel.Prerelease || parseVersion(v) == nil {
		return nil, nil
	}
	if !strings.HasPrefix(rel.HTMLURL, "https://github.com/") {
		rel.HTMLURL = "https://github.com/TanmayDoesAI/tailreach/releases/tag/" + rel.TagName
	}
	return &updateInfo{Version: v, URL: rel.HTMLURL}, nil
}

// parseVersion reads "x.y.z" (an optional leading v). Anything else, a pre-release
// suffix included, is nil.
func parseVersion(s string) []int {
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return nil
		}
		out[i] = n
	}
	return out
}

// compareVersions is -1, 0 or 1 as a is older, the same or newer than b. A version that
// can't be read counts as older than any that can.
func compareVersions(a, b string) int {
	va, vb := parseVersion(a), parseVersion(b)
	switch {
	case va == nil && vb == nil:
		return 0
	case va == nil:
		return -1
	case vb == nil:
		return 1
	}
	for i := range va {
		if va[i] != vb[i] {
			if va[i] < vb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// updateLoop checks a minute after start, then once a day, while checks are on.
// Turning checks back on (settings) checks soon after.
func (a *app) updateLoop(ctx context.Context) {
	if a.opt.noUpdateCheck {
		return
	}
	u := a.updates
	wait := updateFirst
	told := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-u.kick:
		}
		wait = updateEvery
		if !a.updateCheckOn() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := u.check(cctx)
		cancel()
		if err != nil {
			dlog("update check: %v", err)
			continue
		}
		if up := u.available(); up != nil && up.Version != told {
			told = up.Version
			log.Printf("ℹ tailreach %s is out (this is %s): %s — update with `%s` (or `%s` in a clone)", up.Version, Version, up.URL, updateUpgradeHB, updateUpgradeGit)
		}
	}
}

// updateCheckOn: not switched off by --no-update-check or in settings.
func (a *app) updateCheckOn() bool {
	if a.opt.noUpdateCheck {
		return false
	}
	a.sec.mu.Lock()
	defer a.sec.mu.Unlock()
	return !a.sec.cfg.NoUpdateCheck
}

// updateNotice is what to tell the owner, or nil (nothing newer, or checks are off).
func (a *app) updateNotice() *updateInfo {
	if a.updates == nil || !a.updateCheckOn() {
		return nil
	}
	return a.updates.available()
}
