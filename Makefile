GOBIN := $(CURDIR)/bin
export PATH := $(GOBIN):$(PATH)

.PHONY: tools generate lint-api breaking-api

# tools installs the pinned codegen binaries into ./bin. Everything below
# depends on it so a fresh clone needs no manual setup.
tools:
	GOBIN=$(GOBIN) go -C tools install tool

# generate regenerates gen/ from api/. gen/ is committed; never hand-edit it.
generate: tools
	buf generate

lint-api: tools
	buf lint

# breaking-api compares api/ against the published baseline. It requires a
# baseline that already contains api/; see the guard in .github/workflows/ci.yml.
breaking-api: tools
	buf breaking --against '.git#ref=origin/main'
