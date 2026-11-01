package web

import "embed"

// FS is the web UI: the viewer, dashboard and sign-in page, built into the binary.
//
//go:embed all:viewer all:dashboard login.html manifest.webmanifest all:icons
var FS embed.FS
