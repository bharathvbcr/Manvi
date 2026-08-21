package main

import (
	"errors"
	"strings"
	"testing"
)

// The transcript that prompted these:
//
//	the repository index could not be built: devmap manifest failed: exit
//	status 1 (Error: refuse to overwrite a non-devmap-rust repo map at …)
//
// The index had been built. `devmap build` had committed a generation and the
// navigation tools were answering from it; what had failed was `devmap
// manifest`, the second command, which writes the code graph the gate's scope
// rung reads. The sentence named the wrong command, asserted a failure that had
// not happened, and left the capability that was actually degraded unnamed —
// then degraded "navigation tools and the neighbour rule", of which navigation
// was working. An operator following it went to look at an index that was fine.

// TestAFailedArtifactWriteIsNotReportedAsAFailedIndexBuild.
func TestAFailedArtifactWriteIsNotReportedAsAFailedIndexBuild(t *testing.T) {
	cause := errors.New("devmap manifest failed: exit status 1 (Error: refuse to overwrite …)")
	text, degraded := refreshFailure(&refreshError{stage: stageArtifacts, err: cause})

	if strings.Contains(text, "index could not be built") {
		t.Fatalf("a failed artifact write must not be reported as a failed index build: %q", text)
	}
	if !strings.Contains(text, cause.Error()) {
		t.Fatalf("the producer's own words must survive: %q", text)
	}
	if !strings.Contains(text, "artifacts") {
		t.Fatalf("the message must name what failed: %q", text)
	}
	said := strings.Join(degraded, " ")
	if !strings.Contains(said, "scope rung") {
		t.Fatalf("the degradation must name the capability that is actually wrong: %q", said)
	}
	if strings.Contains(said, "navigation tools and the neighbour rule report unavailable") ||
		strings.Contains(said, "navigation tools and the neighbour rule will report unavailable") {
		t.Fatalf("navigation answers from the index this stage did not touch: %q", said)
	}
}

// TestAFailedIndexBuildStillReportsAsOne. The repair must not have moved the
// inaccuracy to the other stage.
func TestAFailedIndexBuildStillReportsAsOne(t *testing.T) {
	cause := errors.New("devmap build failed: exit status 101")
	text, degraded := refreshFailure(&refreshError{stage: stageIndex, err: cause})

	if !strings.Contains(text, "index could not be built") {
		t.Fatalf("a failed build must say so: %q", text)
	}
	if !strings.Contains(text, cause.Error()) {
		t.Fatalf("the producer's own words must survive: %q", text)
	}
	if !strings.Contains(strings.Join(degraded, " "), "navigation tools") {
		t.Fatalf("with no index, navigation is degraded and must be named: %q", degraded)
	}
}

// TestAnUnstagedFailureIsStillReported. refreshIndex is not the only thing that
// can fail on this path, and an error of a shape this build does not recognise
// must reach the operator rather than being dropped for not being expected.
func TestAnUnstagedFailureIsStillReported(t *testing.T) {
	text, degraded := refreshFailure(errors.New("context deadline exceeded"))
	if !strings.Contains(text, "context deadline exceeded") {
		t.Fatalf("an unrecognised failure must carry its own reason: %q", text)
	}
	if len(degraded) == 0 {
		t.Fatal("a failure with nothing named as degraded reads as a failure that cost nothing")
	}
}

// TestARefreshErrorUnwrapsToItsCause, so a caller can still test for what went
// wrong underneath rather than matching on the sentence.
func TestARefreshErrorUnwrapsToItsCause(t *testing.T) {
	cause := errors.New("root cause")
	err := error(&refreshError{stage: stageArtifacts, err: cause})
	if !errors.Is(err, cause) {
		t.Fatal("the staged error must unwrap to what caused it")
	}
	var staged *refreshError
	if !errors.As(err, &staged) || staged.stage != stageArtifacts {
		t.Fatal("the stage must survive being wrapped")
	}
}
