package mcp

import (
	"slices"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// On Windows a declared `Path` and an inherited `PATH` are one variable; exec's
// last-wins rule would let the inherited value override the declared one if
// both were emitted. On unix they are two variables and both stay.
func TestBuildEnvDeduplicatesNamesAsTheHostOSDoes(t *testing.T) {
	t.Setenv("PATH", "/inherited")
	cfg := ServerConfig{Env: map[string]string{"Path": "/declared"}}

	got := buildEnvKeyed(cfg, "", childenv.KeyFor(true))
	if !slices.Contains(got, "Path=/declared") || slices.Contains(got, "PATH=/inherited") {
		t.Fatalf("fold=true: declared Path must be the only spelling, got %q", got)
	}
	got = buildEnvKeyed(cfg, "", childenv.KeyFor(false))
	if !slices.Contains(got, "Path=/declared") || !slices.Contains(got, "PATH=/inherited") {
		t.Fatalf("fold=false: names are distinct on unix, got %q", got)
	}
}
