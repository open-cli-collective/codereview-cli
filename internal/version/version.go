// Package version holds the build-stamped version metadata for the cr binary.
// The values are overridden at release time via goreleaser ldflags
// (-X .../internal/version.Version=… etc.); the defaults below apply to
// `go build`/`go run` development builds.
package version

var (
	// Version is the release version (e.g. "0.1.42"); "dev" for local builds.
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "none"
	// Date is the build timestamp.
	Date = "unknown"
)
