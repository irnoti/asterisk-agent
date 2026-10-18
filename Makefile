VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/irnoti/asterisk-agent/internal/version.Version=$(VERSION)

.PHONY: test vet build packages clean
test:
	go test ./... -race
vet:
	go vet ./...
# static binaries (no libc): they run on old CentOS-based FreePBX/Issabel images too
build:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/irnoti-asterisk-linux-amd64 ./cmd/irnoti-asterisk
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/irnoti-asterisk-linux-arm64 ./cmd/irnoti-asterisk
	cd dist && sha256sum irnoti-asterisk-linux-* > SHA256SUMS
# .rpm and .deb for both architectures, from the binaries `make build` produced. PKG_VERSION must be plain semver (no leading v).
PKG_VERSION ?= $(patsubst v%,%,$(shell git describe --tags --abbrev=0 2>/dev/null || echo 0.0.0))
packages: build
	@command -v nfpm >/dev/null || { echo "nfpm not found: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.41.1"; exit 1; }
	for arch in amd64 arm64; do \
		mkdir -p dist/stage && cp dist/irnoti-asterisk-linux-$$arch dist/stage/irnoti-asterisk || exit 1; \
		for fmt in rpm deb; do GOARCH=$$arch PKG_VERSION=$(PKG_VERSION) nfpm package -p $$fmt -f packaging/nfpm.yaml -t dist/ || exit 1; done; \
	done; rm -rf dist/stage
	cd dist && sha256sum *.rpm *.deb >> SHA256SUMS
clean:
	rm -rf dist
