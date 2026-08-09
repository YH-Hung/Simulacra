GOBIN := $(CURDIR)/bin
# Invoke the pinned buf by absolute path, with ./bin prepended to PATH so buf
# can find its protoc-gen-* plugins. Setting PATH inline rather than with a
# global `export` is required for GNU Make 3.81, the version macOS ships,
# which does not propagate exported variables to recipe lines that contain no
# shell metacharacters.
BUF := PATH="$(GOBIN):$$PATH" $(GOBIN)/buf

TOOLS := $(GOBIN)/buf $(GOBIN)/protoc-gen-go $(GOBIN)/protoc-gen-connect-go

# BREAKING_AGAINST is the git ref breaking-api compares api/ against. It is
# overridable because origin/main is the right baseline only on pull requests:
# on a push, origin/main already points at the pushed tip, so CI passes the
# pre-push commit instead. See the buf breaking step in .github/workflows/ci.yml.
BREAKING_AGAINST ?= origin/main

.DEFAULT_GOAL := lint-api

.PHONY: tools generate lint-api breaking-api

# These are file targets, not phony: make skips the recipe when the binaries
# are already newer than tools/go.mod and tools/go.sum, so routine invocations
# of generate/lint-api/breaking-api don't reinstall the toolchain every time.
# All three are listed, not just buf, so deleting any one of them reinstalls;
# keying on buf alone let a partial bin/ pass `make tools` and then fail in
# `buf generate` with a missing plugin. GNU Make 3.81 (stock macOS) has no
# grouped-target (`&:`) syntax, so this is a multi-target rule: the recipe
# runs once per out-of-date target, but since one `go install tool` writes all
# three, the later targets are already up to date and it runs just once.
$(TOOLS): tools/go.mod tools/go.sum
	GOBIN=$(GOBIN) go -C tools install tool

# tools is the phony convenience alias for a fresh clone's manual setup.
tools: $(TOOLS)

# generate regenerates gen/ from api/. gen/ is committed; never hand-edit it.
generate: $(TOOLS)
	$(BUF) generate

lint-api: $(TOOLS)
	$(BUF) lint

# breaking-api compares api/ against BREAKING_AGAINST. It requires a baseline
# that already contains api/; see the guard in .github/workflows/ci.yml.
breaking-api: $(TOOLS)
	$(BUF) breaking --against '.git#ref=$(BREAKING_AGAINST)'
