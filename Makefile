VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w
TARGETS := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 windows-amd64

# Debug logging: `make DEBUG=1 reload` (or local/run/all). Off by default, and then it isn't
# compiled in at all. Lines go to ~/Library/Application Support/tailreach/tailreach.log, marked "·".
DEBUG ?= 0
GOTAGS := $(if $(filter 1,$(DEBUG)),-tags debug)
SWIFTDEBUG := $(if $(filter 1,$(DEBUG)),-D DEBUG_LOG)

.PHONY: all local capture reload run clean setup doctor

all: $(TARGETS:%=dist/tailreach-%)

dist/tailreach-%:
	@mkdir -p dist
	GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(word 2,$(subst -, ,$*)) CGO_ENABLED=0 \
		go build $(GOTAGS) -trimpath -ldflags '$(LDFLAGS)' -o $@$(if $(findstring windows,$*),.exe,) .

SIGN := tailreach local

# This Mac: the server plus the native screen helper next to it, signed with a stable
# local identity (scripts/signing-identity.sh) so macOS keeps their permissions across builds.
# Built for this Mac's own CPU even by an Intel Go on Apple silicon (no Rosetta while streaming).
NATIVE := $(shell uname -m | sed 's/x86_64/amd64/')
local: capture
	CGO_ENABLED=0 GOARCH=$(NATIVE) go build $(GOTAGS) -trimpath -ldflags '$(LDFLAGS)' -o tailreach .
	codesign --force -s "$(SIGN)" --identifier com.tailreach.agent tailreach

capture:
	swiftc -O -swift-version 5 $(SWIFTDEBUG) capture/*.swift -o tailreach-capture
	codesign --force -s "$(SIGN)" --identifier com.tailreach.capture tailreach-capture

# Set up this Mac: everything that doesn't need you, in order, and safe to run again.
# Signing identity, build, login item (the first time; an existing one keeps its flags and is
# just restarted), then `tailreach doctor`, which lists what's left for you to do.
# Another computer behind the first one's dashboard: make setup FLAGS="--name mac2"
NAME ?= mac
FLAGS ?= --hub --name $(NAME)
setup:
	@command -v swiftc >/dev/null || { echo "Needs the Xcode Command Line Tools: xcode-select --install"; exit 1; }
	@command -v go >/dev/null || { echo "Needs Go: https://go.dev/dl (on Apple silicon, the darwin-arm64 one)"; exit 1; }
	@sh scripts/signing-identity.sh
	@$(MAKE) --no-print-directory local
	@if [ -f "$$HOME/Library/LaunchAgents/com.tailreach.agent.plist" ]; then \
		launchctl kickstart -k gui/$$(id -u)/com.tailreach.agent && echo "✓ login item restarted"; \
	else ./tailreach install $(FLAGS); fi
	@./tailreach doctor

doctor:
	@./tailreach doctor

# Rebuild and restart the login item
reload: local
	launchctl kickstart -k gui/$$(id -u)/com.tailreach.agent

# Live UI hacking: edit files in web/ and refresh the browser
run:
	CGO_ENABLED=0 go run $(GOTAGS) . --hub --web-dir ./web $(ARGS)

clean:
	rm -rf dist tailreach tailreach-capture
