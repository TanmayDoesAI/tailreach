VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w
TARGETS := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 windows-amd64

.PHONY: all local capture reload run clean

all: $(TARGETS:%=dist/mydevices-%)

dist/mydevices-%:
	@mkdir -p dist
	GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(word 2,$(subst -, ,$*)) CGO_ENABLED=0 \
		go build -trimpath -ldflags '$(LDFLAGS)' -o $@$(if $(findstring windows,$*),.exe,) .

SIGN := mydevices local

# This Mac: the server plus the native screen helper next to it, signed with a stable
# local identity (scripts/signing-identity.sh) so macOS keeps their permissions across builds.
local: capture
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o mydevices .
	codesign --force -s "$(SIGN)" --identifier com.mydevices.agent mydevices

capture:
	swiftc -O -swift-version 5 capture/*.swift -o mydevices-capture
	codesign --force -s "$(SIGN)" --identifier com.mydevices.capture mydevices-capture

# Rebuild and restart the login item
reload: local
	launchctl kickstart -k gui/$$(id -u)/com.mydevices.agent

# Live UI hacking: edit files in web/ and refresh the browser
run:
	CGO_ENABLED=0 go run . --hub --web-dir ./web $(ARGS)

clean:
	rm -rf dist mydevices mydevices-capture
