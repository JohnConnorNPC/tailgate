# Trimmed tailscale build: only what tsnet + userspace port mapping needs.
# After upgrading tailscale.com, regenerate the tag list with `make tags`.
#
# Static linux/amd64, GOAMD64=v1: runs on old CPUs without SSE4.2/POPCNT
# (e.g. Atom D510); a v2/v3 build refuses to start there.
GO      ?= go
FEATURES = netstack,dns,nattraversal,udptransport,portmapper,serve,acme,ipnbus
BUILDENV = CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1

tailgate: *.go go.mod go.sum .buildtags
	$(BUILDENV) $(GO) build -tags "$$(cat .buildtags)" -trimpath -ldflags='-s -w' -o $@ .

tags .buildtags:
	$(GO) run tailscale.com/cmd/featuretags --min --add=$(FEATURES) > .buildtags

install: tailgate
	install -Dm755 tailgate $(HOME)/.local/bin/tailgate
	install -Dm644 tailgate.service $(HOME)/.config/systemd/user/tailgate.service
	install -Dm644 tailgate@.service $(HOME)/.config/systemd/user/tailgate@.service

.PHONY: tags install
