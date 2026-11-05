// Package version carries the build version. The build task links the value
// from the repository VERSION file through -ldflags, so the source has one
// placeholder for tests and for plain `go build`.
package version

// Version is the release version, injected with
// -X lingzhu/internal/version.Version=<version>.
var Version = "0.1.0-dev"
