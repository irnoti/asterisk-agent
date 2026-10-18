// Package version holds the build version, set at link time with -ldflags.
package version

// Version is overridden by the release build: -ldflags "-X github.com/irnoti/asterisk-agent/internal/version.Version=1.2.3".
var Version = "dev"
