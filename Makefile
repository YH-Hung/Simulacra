GOBIN := $(CURDIR)/bin
# Invoke the pinned buf by absolute path, with ./bin prepended to PATH so buf
# can find its protoc-gen-* plugins. Setting PATH inline rather than with a
# global `export` is required for GNU Make 3.81, the version macOS ships,
# which does not propagate exported variables to recipe lines that contain no
# shell metacharacters.
BUF := PATH="$(GOBIN):$$PATH" $(GOBIN)/buf

.DEFAULT_GOAL := lint-api

.PHONY: tools generate lint-api breaking-api

# $(GOBIN)/buf is a file target, not phony: make skips the recipe when the
# binaries are already newer than tools/go.mod and tools/go.sum, so routine
# invocations of generate/lint-api/breaking-api don't reinstall the toolchain
# every time. protoc-gen-go and protoc-gen-connect-go are installed by the
# same `go install tool` call, so buf's mtime stands in for all three; GNU
# Make 3.81 (stock macOS) has no grouped-target (`&:`) syntax to declare that
# explicitly.
$(GOBIN)/buf: tools/go.mod tools/go.sum
	GOBIN=$(GOBIN) go -C tools install tool

# tools is the phony convenience alias for a fresh clone's manual setup.
tools: $(GOBIN)/buf

# generate regenerates gen/ from api/. gen/ is committed; never hand-edit it.
generate: $(GOBIN)/buf
	$(BUF) generate

lint-api: $(GOBIN)/buf
	$(BUF) lint

# breaking-api compares api/ against the published baseline. It requires a
# baseline that already contains api/; see the guard in .github/workflows/ci.yml.
breaking-api: $(GOBIN)/buf
	$(BUF) breaking --against '.git#ref=origin/main'
