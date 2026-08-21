package main

import (
	"bytes"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"manvi/flags"
)

// TestVersionIsATopLevelCommand: a harness pointed at CI has to be able to say
// which build produced a result. Before this existed, `manvi --version` was
// answered with "unknown command", so a benchmark sweep across a rebuild
// described two different binaries with nothing in the output to tell them
// apart.
func TestVersionIsATopLevelCommand(t *testing.T) {
	// The build identity is not a question about this repository, so the
	// scaffolding that every other command needs is left switched off here.
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")

	for _, arg := range []string{"--version", "version"} {
		var out, notes bytes.Buffer
		if err := run(&out, &notes, []string{arg}); err != nil {
			t.Fatalf("manvi %s: %v", arg, err)
		}
		text := out.String()
		if text == "" {
			t.Fatalf("manvi %s printed nothing", arg)
		}
		// What it must actually name.
		for _, want := range []string{"manvi", runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
			if !strings.Contains(text, want) {
				t.Errorf("manvi %s output does not name %q:\n%s", arg, want, text)
			}
		}
		// And what it must never say. "(devel)" is the module version Go
		// gives every unreleased build, and "dev"/"unset" read like versions
		// while identifying nothing. A v0.0.0-<time>-<hash> pseudo-version is
		// not in this list: it is derived from a real commit, and it is what a
		// VCS-stamped build of an untagged tree legitimately reports.
		for _, banned := range []string{"(devel)", "version dev", "version unset", "unknown version"} {
			if strings.Contains(text, banned) {
				t.Errorf("manvi %s printed the placeholder %q as if it were an identity:\n%s", arg, banned, text)
			}
		}
		// No field may be printed with an empty value: a blank line where a
		// revision should be reads as "no revision", which is a claim this
		// binary has not earned either way.
		for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				t.Errorf("manvi %s printed a blank line:\n%s", arg, text)
				continue
			}
			if fields := strings.Fields(line); len(fields) < 2 && strings.HasPrefix(line, "  ") {
				t.Errorf("manvi %s printed field %q with no value:\n%s", arg, line, text)
			}
		}
	}
}

// TestBuildIdentityDegradesHonestlyWithoutAVCSStamp: this tree is not a
// repository, so nothing built here carries vcs.revision, vcs.time or
// vcs.modified, and the module version is Go's "(devel)" placeholder. Neither
// may be printed as if it were an identity, and neither may be printed as an
// empty field — a blank where a revision goes reads as a value.
func TestBuildIdentityDegradesHonestlyWithoutAVCSStamp(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Path: "manvi", Version: "(devel)"}}

	id := readBuildIdentity(info, true, "")
	if id.Version != "" {
		t.Errorf("Version = %q; an unstamped build has none, and %q is Go's placeholder", id.Version, "(devel)")
	}
	if id.Revision != "" || id.Built != "" || id.TreeState != "" {
		t.Errorf("invented VCS facts from a binary that carries none: %+v", id)
	}
	if id.Go != runtime.Version() || id.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("Go/Platform = %q/%q; want %q/%q", id.Go, id.Platform, runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH)
	}
	if summary := id.Summary(); !strings.Contains(summary, "version unknown") || !strings.Contains(summary, "no VCS stamp") {
		t.Errorf("summary does not say what it does not know: %q", summary)
	}

	// The same absence, from a binary with no build info at all.
	if none := readBuildIdentity(nil, false, ""); none.Go != runtime.Version() || none.Version != "" {
		t.Errorf("readBuildIdentity(nil, false, \"\") = %+v; want the toolchain facts and nothing invented", none)
	}
}

// TestBuildIdentityReportsAStampedBuild covers the shape this tree cannot
// produce: a release built from a VCS-stamped checkout.
func TestBuildIdentityReportsAStampedBuild(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: "manvi", Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "3f9c1ab2d4e5f60718293a4b5c6d7e8f90a1b2c3"},
			{Key: "vcs.time", Value: "2026-08-18T21:04:11Z"},
			{Key: "vcs.modified", Value: "false"},
		},
	}

	id := readBuildIdentity(info, true, "v1.2.3")
	if id.Version != "v1.2.3" || id.VersionFrom != "-ldflags" {
		t.Errorf("ldflags stamp not reported as the version: %+v", id)
	}
	if id.Revision != "3f9c1ab2d4e5f60718293a4b5c6d7e8f90a1b2c3" || id.Built != "2026-08-18T21:04:11Z" || id.TreeState != "clean" {
		t.Errorf("VCS stamps not carried through: %+v", id)
	}

	// The ldflags path is an override, not the only source: drop the stamp and
	// the revision still identifies the build.
	unstamped := readBuildIdentity(info, true, "")
	if unstamped.Version != "" || unstamped.Revision != id.Revision {
		t.Errorf("build with no ldflags stamp lost its identity: %+v", unstamped)
	}

	// A dirty tree is a different build from a clean one at the same revision,
	// and saying so is the whole point.
	info.Settings[2].Value = "true"
	if dirty := readBuildIdentity(info, true, ""); dirty.TreeState != "modified" {
		t.Errorf("TreeState = %q; want %q", dirty.TreeState, "modified")
	}

	// An unrecognised value leaves the state unknown rather than guessing the
	// answer an operator would rather hear.
	info.Settings[2].Value = "maybe"
	if odd := readBuildIdentity(info, true, ""); odd.TreeState != "" {
		t.Errorf("TreeState = %q from an unparseable vcs.modified; want unknown", odd.TreeState)
	}
}

// TestDoctorNamesTheBuild: doctor is the "what is my configuration" surface,
// and which binary produced the report is part of that answer.
func TestDoctorNamesTheBuild(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// doctor probes the store and the verifier and reports them unavailable
	// rather than failing; the build line is what this asserts.
	if err := doctor(&out, reg); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(out.String()), " ")
	if want := "build " + currentBuild().Summary(); !strings.Contains(text, strings.Join(strings.Fields(want), " ")) {
		t.Errorf("doctor does not report which build produced it:\n%s", out.String())
	}
}
