package version

// Version information set via ldflags during build
var (
	// Version is the semantic version
	Version = "dev"

	// Commit is the git commit SHA
	Commit = "unknown"

	// BuildDate is the build timestamp
	BuildDate = "unknown"
)
