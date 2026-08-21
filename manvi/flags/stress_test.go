package flags

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConcurrentSetAndReadStaysConsistent.
//
// The registry became a live object the moment a setting could be moved from a
// running face: one goroutine writes an override while gates, prompts and
// reports read through it. Run this with -race. What it asserts beyond the race
// detector is that a read never observes a value the catalogue does not permit
// — a torn read of an enum is not a crash, it is a gate mode nothing validated.
func TestConcurrentSetAndReadStaysConsistent(t *testing.T) {
	r := testRegistry(t)
	legal := map[string]bool{ModeEnforce: true, ModeAdvisory: true, ModeOff: true}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		values := []string{ModeEnforce, ModeAdvisory, ModeOff}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := r.Set(Human, PolicyFileMode, values[i%len(values)]); err != nil {
				t.Errorf("set: %v", err)
				return
			}
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				v, err := r.Lookup(PolicyFileMode)
				if err != nil {
					t.Errorf("lookup: %v", err)
					return
				}
				if !legal[v.Raw] {
					t.Errorf("read %q, which is not a legal value for %s", v.Raw, PolicyFileMode)
					return
				}
				// Weakened walks the whole catalogue while the writer moves one
				// of its rows. It must never return a value for a key that is
				// not a safety flag, whatever it catches mid-flight.
				for _, w := range r.Weakened() {
					d, ok := r.Def(w.Key)
					if !ok || !d.Safety {
						t.Errorf("Weakened reported %q, which is not a safety flag", w.Key)
						return
					}
				}
			}
		}()
	}

	time.Sleep(250 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestSealUnderConcurrentSets. Seal is the boundary between boot and run, and
// it is taken while other goroutines may already be reading. Every Set that
// lands after it must be refused for the startup flags and permitted for the
// human ones — never a mix, and never a panic.
func TestSealUnderConcurrentSets(t *testing.T) {
	r := testRegistry(t)
	var wg sync.WaitGroup
	start := make(chan struct{})

	var mu sync.Mutex
	var startupAccepted int

	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				if err := r.Set(Human, PolicyHardRules, "false"); err == nil {
					mu.Lock()
					startupAccepted++
					mu.Unlock()
				}
				return
			}
			if err := r.Set(Human, PolicyFileMode, ModeAdvisory); err != nil {
				t.Errorf("a human-mutable flag was refused: %v", err)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		r.Seal()
	}()

	close(start)
	wg.Wait()

	// After the seal it is refused, whatever raced before it.
	if err := r.Set(Human, PolicyHardRules, "false"); err == nil {
		t.Fatal("a startup flag was movable after Seal returned")
	}
	t.Logf("%d startup sets landed before the seal (all before it, by definition)", startupAccepted)
}

// TestSetRejectsHostileValues. Every value reaching Set comes from a human
// typing into a composer or a shell. None of these may become a stored value:
// a stored value is what Lookup reports, what the flag table prints, and what
// every consumer parses.
func TestSetRejectsHostileValues(t *testing.T) {
	r := testRegistry(t)
	cases := []struct {
		name, key, raw string
	}{
		{"enum with a newline", PolicyFileMode, "enforce\noff"},
		{"enum with a null byte", PolicyFileMode, "enforce\x00"},
		{"enum with an inner space", PolicyFileMode, "en force"},
		{"enum case", PolicyFileMode, "ENFORCE"},
		{"bool word", GrantsEnabled, "affirmative"},
		{"bool empty", GrantsEnabled, ""},
		{"int with a sign and text", MaxSteps, "12steps"},
		{"int overflow text", MaxSteps, strings.Repeat("9", 400)},
		{"duration without a unit", GrantsAgentMaxTTL, "15"},
		{"duration negative text", GrantsAgentMaxTTL, "--5m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, err := r.Lookup(c.key)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Set(Human, c.key, c.raw); err == nil {
				after, _ := r.Lookup(c.key)
				t.Fatalf("accepted %q; the stored value is now %q", c.raw, after.Raw)
			}
			after, err := r.Lookup(c.key)
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("a refused set changed the value: %+v → %+v", before, after)
			}
		})
	}
}

// TestSetNormalisesSurroundingWhitespace. "enforce " with a trailing space from
// a heredoc or a YAML value must not be a different setting from "enforce".
func TestSetNormalisesSurroundingWhitespace(t *testing.T) {
	r := testRegistry(t)
	if err := r.Set(Human, PolicyFileMode, "  advisory\t"); err != nil {
		t.Fatalf("a padded value was refused: %v", err)
	}
	v, _ := r.Lookup(PolicyFileMode)
	if v.Raw != ModeAdvisory {
		t.Fatalf("stored %q, want the trimmed value", v.Raw)
	}
}

// TestEveryCatalogueValueRoundTrips walks the whole catalogue rather than a
// sample: for each flag, its own default must be a value Set accepts and Lookup
// returns unchanged. A catalogue entry whose default its own validator rejects
// is a setting nobody can restore after moving it.
func TestEveryCatalogueValueRoundTrips(t *testing.T) {
	r := testRegistry(t)
	for _, key := range r.Keys() {
		d, _ := r.Def(key)
		if d.Mutable == Startup {
			continue
		}
		if err := r.Set(Human, key, d.Default); err != nil {
			t.Errorf("%s: its own default %q was refused: %v", key, d.Default, err)
			continue
		}
		v, err := r.Lookup(key)
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if v.Raw != strings.TrimSpace(d.Default) {
			t.Errorf("%s: stored %q, want %q", key, v.Raw, d.Default)
		}
		if d.Kind == KindEnum {
			for _, choice := range d.Values {
				if err := r.Set(Human, key, choice); err != nil {
					t.Errorf("%s: declared choice %q was refused: %v", key, choice, err)
				}
			}
			if err := r.Set(Human, key, fmt.Sprintf("%s-not-a-choice", d.Values[0])); err == nil {
				t.Errorf("%s: accepted a value outside its own enumeration", key)
			}
		}
	}
}
