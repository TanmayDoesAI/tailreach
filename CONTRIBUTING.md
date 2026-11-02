# Contributing

Issues and pull requests are welcome.

## Before you start

Read [`AGENTS.md`](AGENTS.md): the layout, how to build and run, and the invariants the code must
keep (what a viewer may and may not do, how the capture helper stops, and so on). Changes that
touch security need to keep every one of them.

## Build and test

```sh
make local                                    # build + sign tailreach and tailreach-capture
make reload                                   # build, sign and restart the login item
go run ./cmd/tailreach --hub --name mac --web-dir ./web   # edit the UI live, refresh the browser
CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test ./...
swiftc -O -swift-version 5 macos/*.swift -o tailreach-capture
```

CI runs the vet, tests, the Go build, the Swift build and `node --check` on the viewer's
JavaScript on every pull request.

The website lives in [`site/`](site/) (Astro + Starlight): `cd site && npm ci && npm run dev`.

## Pull requests

- Keep commits small and clear: one change each, with a message that says what and why.
- Test against the login item (`make reload`), not a terminal-run hub.
- Never commit binaries, state files, keys, logs, or a real tailnet name, Tailscale IP, device name
  or e-mail. Use placeholders like `<tailnet>`.
- Update `README.md` (and the matching page in `site/`) when behaviour users see changes.

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
