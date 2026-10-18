VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/irnoti/asterisk-agent/internal/version.Version=$(VERSION)

.PHONY: test vet build clean
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
clean:
	rm -rf dist
