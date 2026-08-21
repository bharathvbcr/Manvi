package main

// Build identity: which binary produced a result.
//
// A harness that is pointed at CI and at benchmarks has to be able to name its
// own build, because the alternative is what actually happened — a sweep run
// across a rebuild, whose results silently described two different binaries.
//
// Everything here is read out of the binary rather than kept in a constant
// somebody has to remember to bump. A constant would be stale the first time
// it was forgotten, and stale in the direction that looks correct.

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// stampedVersion is the release stamp, and is set at link time:
//
//	go build -ldflags "-X main.stampedVersion=v1.2.3" ./cmd/manvi
//
// It is empty in every build that does not pass that flag, and empty is
// reported as unknown. It is deliberately not the only source: an unstamped
// build still has the module version, the VCS revision, the toolchain and the
// platform to say something true with.
var stampedVersion string

// buildIdentity is what this binary can truthfully say about itself. Every
// string field is empty when the corresponding fact is not recorded in the
// binary, so that "not known" and "known to be absent" cannot be confused at
// the point of printing.
type buildIdentity struct {
	Version     string // release version, from -ldflags or the module
	VersionFrom string // where Version came from; empty exactly when Version is
	Revision    string // vcs.revision
	Built       string // vcs.time
	TreeState   string // "clean" or "modified", from vcs.modified
	Module      string // main module path
	Go          string // toolchain that compiled this binary
	Platform    string // GOOS/GOARCH
}

// currentBuild reads the identity of the running binary.
func currentBuild() buildIdentity {
	info, ok := debug.ReadBuildInfo()
	return readBuildIdentity(info, ok, stampedVersion)
}

// readBuildIdentity is the whole of the logic, kept separate from the reading
// so that both the stamped and the unstamped shape can be exercised from a
// test — this tree is not a repository, so a test that only ever saw the
// build it runs in would only ever see one of the two.
func readBuildIdentity(info *debug.BuildInfo, ok bool, stamped string) buildIdentity {
	id := buildIdentity{
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if v := strings.TrimSpace(stamped); v != "" {
		id.Version, id.VersionFrom = v, "-ldflags"
	}
	if !ok || info == nil {
		return id
	}
	id.Module = info.Main.Path
	// "(devel)" is what Go reports for every build that is not an installed
	// module release, which is every build made here. It is a placeholder, and
	// printing it in a version field would be printing a fake version.
	if id.Version == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		id.Version, id.VersionFrom = info.Main.Version, "module"
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			id.Revision = s.Value
		case "vcs.time":
			id.Built = s.Value
		case "vcs.modified":
			// Only the two values the toolchain documents are believed. An
			// unrecognised one leaves the tree state unknown, which is what it
			// is; guessing "clean" here would be the one wrong guess.
			switch s.Value {
			case "true":
				id.TreeState = "modified"
			case "false":
				id.TreeState = "clean"
			}
		}
	}
	return id
}

// unstamped explains the absence rather than leaving a blank. A build made
// outside a repository, or with -buildvcs=false, carries no vcs.* settings at
// all, and the honest report is that this binary does not know — not an empty
// field, which reads as a value.
const unstamped = "unknown — no VCS stamp in this binary (built outside a repository, or with -buildvcs=false)"

// writeVersion prints the full report, one fact per line.
func writeVersion(out io.Writer) {
	id := currentBuild()
	fmt.Fprintln(out, "manvi")
	switch {
	case id.Version != "":
		fmt.Fprintf(out, "  version    %s (%s)\n", id.Version, id.VersionFrom)
	default:
		fmt.Fprintln(out, "  version    unknown — no -ldflags stamp, and the module reports no release version")
	}
	if id.Revision != "" {
		if id.TreeState != "" {
			fmt.Fprintf(out, "  revision   %s (%s)\n", id.Revision, id.TreeState)
		} else {
			fmt.Fprintf(out, "  revision   %s (working tree state not recorded)\n", id.Revision)
		}
	} else {
		fmt.Fprintf(out, "  revision   %s\n", unstamped)
	}
	if id.Built != "" {
		fmt.Fprintf(out, "  committed  %s\n", id.Built)
	} else {
		fmt.Fprintf(out, "  committed  %s\n", unstamped)
	}
	if id.Module != "" {
		fmt.Fprintf(out, "  module     %s\n", id.Module)
	}
	fmt.Fprintf(out, "  go         %s\n", id.Go)
	fmt.Fprintf(out, "  platform   %s\n", id.Platform)
}

// Summary is the same identity on one line, for doctor — where "which build"
// is one answer among the many that command gives.
func (id buildIdentity) Summary() string {
	version := "version unknown"
	if id.Version != "" {
		version = fmt.Sprintf("%s (%s)", id.Version, id.VersionFrom)
	}
	revision := "no VCS stamp"
	if id.Revision != "" {
		revision = "revision " + id.Revision
		if id.TreeState != "" {
			revision += " (" + id.TreeState + ")"
		}
	}
	return fmt.Sprintf("%s, %s, %s %s", version, revision, id.Go, id.Platform)
}
