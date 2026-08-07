GOBIN := $(CURDIR)/bin
# Invoke the pinned buf by absolute path, with ./bin prepended to PATH so buf
# can find its protoc-gen-* plugins. Setting PATH inline rather than with a
# global `export` is required for GNU Make 3.81, the version macOS ships,
# which does not propagate exported variables to recipe lines that contain no
# shell metacharacters.
BUF := PATH="$(GOBIN):$$PATH" $(GOBIN)/buf

.PHONY: tools generate lint-api breaking-api

# tools installs the pinned codegen binaries into ./bin. Everything below
# depends on it so a fresh clone needs no manual setup.
tools:
	GOBIN=$(GOBIN) go -C tools install tool

# generate regenerates gen/ from api/. gen/ is committed; never hand-edit it.
generate: tools
	$(BUF) generate

lint-api: tools
	$(BUF) lint

# breaking-api compares api/ against the published baseline. It requires a
# baseline that already contains api/; see the guard in .github/workflows/ci.yml.
breaking-api: tools
	$(BUF) breaking --against '.git#ref=origin/main'
