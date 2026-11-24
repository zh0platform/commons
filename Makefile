# The Go release the checks run with; go.mod's go line stays the lowest version the module needs.
GO_TOOLCHAIN := 1.27.4
LINT := bin/golangci-lint
GO_TEST := go test -count=1 -race -shuffle=on
# The build tags that together compile every test.
TAGS := database,stack,keycloak
# The module's own runner, with its test product (internal/selftest).
RUNNER := go run ./internal/selftest/withdb
COUNT ?= 20
BASE ?= origin/main
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0
# The release binaries scripts/install-release.sh installs, pinned per platform: the canonical platform from uname, the
# asset name for it and the asset's SHA-256. A bump changes a version and its values together, 7 days after the release.
PLATFORM := $(shell uname -s | tr '[:upper:]' '[:lower:]')-$(shell uname -m | sed -e 's/^x86_64$$/amd64/' -e 's/^aarch64$$/arm64/')
INSTALL_RELEASE := sh scripts/install-release.sh
BINDIR ?= $(CURDIR)/bin
BETTERLEAKS_VERSION := 1.10.1
BETTERLEAKS_ASSET_darwin-arm64 := darwin_arm64
BETTERLEAKS_ASSET_linux-amd64 := linux_x64
BETTERLEAKS_ASSET_linux-arm64 := linux_arm64
BETTERLEAKS_SHA256_darwin-arm64 := ca91eb3b006f15478411f235b5e2712fb15cea74f6701ebadc2364efe0af1403
BETTERLEAKS_SHA256_linux-amd64 := c174b42d7aa60caf58c0c3e555c0e4bed01fe6b0d99a412bce777b5af4ce913e
BETTERLEAKS_SHA256_linux-arm64 := 62a63199f9f61a6b5e5a6f58f048493db20fbc8c770014f4d903a4d97886e1bf

export GOTOOLCHAIN := local
export GOFLAGS := -mod=readonly

.NOTPARALLEL:
.PHONY: qa fix unit stack keycloak tests flake mutate lint check-tools fmt-check vet tidy-check names vuln hooks \
	install-betterleaks betterleaks-version allowlists allowlists-proof secrets-staged secrets-history

qa: check-tools fmt-check vet lint tidy-check names vuln allowlists allowlists-proof tests stack

fix: check-tools
	$(LINT) fmt
	$(LINT) run --fix
	go fix -tags $(TAGS) ./...
	go mod tidy

# Unit tests and the database tests of testdb, against the test product's own server.
unit:
	$(RUNNER) -- $(GO_TEST) -tags database ./...

# The runner as a real process: it starts runs of its own. Its runs may each take 12 minutes on a cold machine
# (withdb/stack_test.go, runBound), so the test gets 20, inside the CI job's 30.
stack:
	$(GO_TEST) -timeout 20m -tags stack ./withdb

# The module's Keycloak, started once.
keycloak:
	$(RUNNER) --keycloak -- $(GO_TEST) -tags keycloak ./keycloak

# unit and keycloak on one stack, as make qa runs them.
tests:
	$(RUNNER) --keycloak -- $(GO_TEST) -tags database,keycloak ./...

flake:
	@if [ -z "$(TESTS)" ]; then echo "usage: make flake TESTS='<regexp>' [COUNT=20]"; exit 2; fi
	$(RUNNER) -- $(GO_TEST) -count=$(COUNT) -tags database -run '$(TESTS)' ./...

# Mutation testing on the lines changed since BASE (.gomutants.yml).
mutate:
	@test -z "$$(git status --porcelain)" || { echo "make mutate needs a clean tree: commit first"; exit 1; }
	$(RUNNER) -- go run github.com/gomutants/gomutants@v0.7.3 --workers 2 --changed-since $(BASE) --tags database \
		--cache=bin/gomutants-cache.json --output bin/mutation-report.json ./...

lint: check-tools
	$(LINT) run

check-tools:
	@./scripts/check-tools.sh $(GO_TOOLCHAIN)

fmt-check: check-tools
	$(LINT) fmt --diff

vet:
	go vet -tags $(TAGS) ./...

tidy-check:
	go mod tidy -diff

# The module is shared by several products: no Go, YAML, shell, TOML or template file of it names one. The test
# product carries the agent's parameters on purpose, so its folder and the tests that drive it (withdb's and testdb's)
# may name the agent.
NAMED := '*.go' '*.yaml' '*.yml' '*.sh' '*.toml' '*.tmpl'
names:
	@! git grep --untracked -il -e bff -e membership -- $(NAMED) || { echo "names: the files above name a product; describe it in a testdb.Product instead"; exit 1; }
	@! git grep --untracked -il -e agent -- $(NAMED) ':!internal/selftest' ':!withdb/*_test.go' ':!testdb/*_test.go' || { echo "names: the files above name a product; describe it in a testdb.Product instead"; exit 1; }

vuln:
	go run $(GOVULNCHECK) -tags $(TAGS) ./...

hooks:
	git config core.hooksPath .githooks

# Each install target stops naming the platform when no build is pinned for it.
PINNED = test -n "$(1)" || { echo "$(2): no pinned build for $(PLATFORM)"; exit 1; }
# A pinned tool's version command must print the expected line; otherwise the check names the install target.
REQUIRE_VERSION = out=$$("$(BINDIR)/$(1)" $(2) 2>&1) && printf '%s\n' "$$out" | grep -qxE '$(subst .,\.,$(3))' || { echo "$(1) $(4) is required at $(BINDIR)/$(1): run make install-$(1)"; exit 1; }

install-betterleaks:
	@$(call PINNED,$(BETTERLEAKS_SHA256_$(PLATFORM)),betterleaks)
	@$(INSTALL_RELEASE) betterleaks "$(BINDIR)" \
		https://github.com/betterleaks/betterleaks/releases/download/v$(BETTERLEAKS_VERSION)/betterleaks_$(BETTERLEAKS_VERSION)_$(BETTERLEAKS_ASSET_$(PLATFORM)).tar.gz \
		"$(BETTERLEAKS_SHA256_$(PLATFORM))" betterleaks

# The pinned binary the secret scripts run, and its version check.
export BETTERLEAKS = $(BINDIR)/betterleaks
betterleaks-version:
	@$(call REQUIRE_VERSION,betterleaks,version,$(BETTERLEAKS_VERSION),$(BETTERLEAKS_VERSION))

# The allow lists (README, "Allow lists"): canonical, none expired, no other ignore file, and the probe fires.
allowlists: betterleaks-version
	@sh scripts/check-allowlists.sh

# Proves the allow-list check catches what no tool catches by itself: each fixture in test/allowlists must fail with the
# cause its first line names ("# expect: <cause>").
allowlists-proof:
	@for fixture in test/allowlists/*; do \
		cause=$$(sed -n '1s/^# expect: //p' "$$fixture"); \
		out=$$(sh scripts/check-allowlists.sh "$$fixture" 2>&1) && status=0 || status=$$?; \
		if [ -z "$$cause" ] || [ $$status -ne 1 ] || ! printf '%s\n' "$$out" | grep -qF "$$cause"; then \
			printf '%s\n' "$$out"; echo "the allow-list check did not flag $$fixture with its expected cause"; exit 1; fi; \
	done

# Scans what is staged for the commit being made (the pre-commit hook).
secrets-staged: allowlists
	@sh scripts/scan-secrets.sh staged

# The last commit before the secret scans began. Its history was scanned in full before publishing and holds three
# findings, each a test value (the triage, finding by finding, is in the closure record of the story that set it,
# 2026-11-24). It never moves forward; only a full re-triage of the history before a new value may change it.
SCAN_SINCE := f41edaec1cd2751dccb33213c037399630fae6f5

# Scans every commit since SCAN_SINCE that HEAD reaches, as the Secrets workflow does.
secrets-history: allowlists
	@sh scripts/scan-secrets.sh history $(SCAN_SINCE)
