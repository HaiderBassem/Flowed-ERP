// Package buildinfo resolves which build of this program is running.
//
// A financial system is asked "which version produced this row?" during every
// incident, and the honest answer has to survive three ways of building the
// binary: the release pipeline (which stamps everything through -ldflags), a
// developer's `go build` (which stamps nothing but leaves VCS data in the
// binary), and `go run` in a dirty tree. The previous arrangement collapsed all
// three into the string "dev", so a production binary that missed its ldflags
// reported exactly what a laptop build reports — and the health endpoint,
// the logs and every metric label then agreed on a version that meant nothing.
//
// Resolution order, most trustworthy first:
//
//  1. values stamped at link time by the release build;
//  2. the VCS revision Go embeds automatically since 1.18;
//  3. Unknown, which is a distinct value rather than a plausible-looking one.
//
// Production refuses to start on Unknown. That is the point of the package:
// an unidentifiable build serving money is a worse outage than not serving.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Stamped by the release build:
//
//	go build -ldflags "-X github.com/swibit/flowed/internal/platform/buildinfo.version=1.0.0 ..."
//
// Left empty by every other build, which is what lets the resolver tell a
// release apart from a local compile instead of guessing.
var (
	version   string
	commit    string
	buildTime string
	treeState string
)

// Unknown is the version reported when nothing identifies the build. It is
// deliberately not a version-shaped string: no deployment should be able to
// mistake it for a release, and config validation refuses it in production.
const Unknown = "unknown"

// Info describes the running build.
type Info struct {
	// Version is the release version, e.g. "1.0.0", or Unknown.
	Version string
	// Commit is the VCS revision, short form, or Unknown.
	Commit string
	// BuildTime is when the binary was linked, in RFC3339, or empty.
	BuildTime string
	// Dirty reports that the working tree had uncommitted changes at build
	// time. A dirty production build is not reproducible from any commit,
	// which is worth saying out loud in the logs.
	Dirty bool
	// GoVersion is the toolchain that compiled the binary.
	GoVersion string
}

// resolved is computed once. Reading build info walks a table in the binary,
// and this is consulted from log lines and the health endpoint.
var resolved = resolve()

// Get returns the running build's identity.
func Get() Info { return resolved }

// Version returns just the version string, which is what most callers want.
func Version() string { return resolved.Version }

// resolve applies the precedence described in the package comment.
func resolve() Info {
	info := Info{
		Version:   strings.TrimSpace(version),
		Commit:    strings.TrimSpace(commit),
		BuildTime: strings.TrimSpace(buildTime),
		Dirty:     strings.TrimSpace(treeState) == "dirty",
		GoVersion: runtime.Version(),
	}

	// Go embeds the revision, its timestamp and whether the tree was modified
	// into every binary built inside a repository. That covers the developer
	// build the release pipeline did not produce, and it cannot be faked by
	// forgetting a flag.
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = shortCommit(setting.Value)
				}
			case "vcs.time":
				if info.BuildTime == "" {
					info.BuildTime = setting.Value
				}
			case "vcs.modified":
				if setting.Value == "true" {
					info.Dirty = true
				}
			}
		}
		// A module built as a dependency carries its own version tag. Ignore
		// the pseudo-version Go invents for an untagged build: "(devel)" is a
		// placeholder, not a release.
		if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
	}

	if info.Version == "" {
		info.Version = Unknown
	}
	if info.Commit == "" {
		info.Commit = Unknown
	}
	return info
}

// shortCommit trims a full hash to the twelve characters humans compare.
func shortCommit(full string) string {
	if len(full) > 12 {
		return full[:12]
	}
	return full
}

// IsIdentified reports whether this build can be traced back to a release.
//
// The check production runs at start-up. A build that cannot say what it is
// cannot be rolled back to a known-good predecessor, and "which version wrote
// this payment row" becomes unanswerable for every row it writes.
func (i Info) IsIdentified() bool {
	return i.Version != Unknown && i.Version != "" && i.Version != "dev"
}

// String renders the identity for a log line: 1.0.0+a1b2c3d4e5f6 (dirty).
func (i Info) String() string {
	var b strings.Builder
	b.WriteString(i.Version)
	if i.Commit != Unknown && i.Commit != "" {
		b.WriteString("+")
		b.WriteString(i.Commit)
	}
	if i.Dirty {
		b.WriteString(" (dirty)")
	}
	return b.String()
}

// Age reports how long ago the binary was built, and whether that is known.
// Used by the health endpoint, where a replica running a fortnight-old build
// after a deploy is the fastest way to spot a node that missed the rollout.
func (i Info) Age(now time.Time) (time.Duration, bool) {
	if i.BuildTime == "" {
		return 0, false
	}
	built, err := time.Parse(time.RFC3339, i.BuildTime)
	if err != nil {
		return 0, false
	}
	return now.Sub(built), true
}

// Describe renders the full identity, for `api version` and the health body.
func (i Info) Describe() string {
	return fmt.Sprintf("version=%s commit=%s built=%s dirty=%t go=%s",
		i.Version, i.Commit, orNone(i.BuildTime), i.Dirty, i.GoVersion)
}

func orNone(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
