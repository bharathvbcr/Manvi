package main

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"manvi/flags"
)

// TestFlagsSetSweepsTheWholeCatalogue.
//
// Every setting the catalogue says a human may move is moved, through the real
// command dispatch, to every value the catalogue declares legal for it. A
// sample would miss the one flag whose default its own validator rejects, or
// whose enum lists a value Set refuses — and either of those is a setting an
// operator can move and then cannot put back.
func TestFlagsSetSweepsTheWholeCatalogue(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	moved, startup := 0, 0
	for _, key := range reg.Keys() {
		def, _ := reg.Def(key)
		values := []string{def.Default}
		values = append(values, def.Values...)

		for _, value := range values {
			t.Chdir(t.TempDir())
			out, err := runFlags(t, "flags", "set", key, value)
			if def.Mutable == flags.Startup {
				startup++
				if err == nil {
					t.Errorf("%s is startup-only but was moved after boot:\n%s", key, out)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s = %q was refused: %v", key, value, err)
				continue
			}
			moved++
			if !strings.Contains(out, key) {
				t.Errorf("%s: the report does not name the key:\n%s", key, out)
			}
			// A safety flag must always say which way it moved. This is the
			// line an operator reads to know whether the run is still strict.
			if def.Safety && !strings.Contains(out, "safest value") {
				t.Errorf("%s: a safety flag moved without saying which direction:\n%s", key, out)
			}
		}
	}
	if moved == 0 || startup == 0 {
		t.Fatalf("swept %d movable and %d startup settings; this test asserts nothing", moved, startup)
	}
	t.Logf("swept %d movable settings and %d startup refusals", moved, startup)
}

// TestFlagsSetThroughTheTUIQuoting. Inside the TUI there is no shell, so the
// arguments are split here. A value with a space in it must survive, and an
// unterminated quote must be an error rather than a silently truncated value —
// a setting quietly set to a prefix of what was typed is worse than one that
// refused.
func TestFlagsSetThroughTheTUIQuoting(t *testing.T) {
	cases := []struct {
		in    string
		want  []string
		isErr bool
	}{
		{`set llm.local.stop "</s> <|im_end|>"`, []string{"set", "llm.local.stop", "</s> <|im_end|>"}, false},
		{`set llm.local.stop '</s> x'`, []string{"set", "llm.local.stop", "</s> x"}, false},
		{`set policy.file.mode enforce`, []string{"set", "policy.file.mode", "enforce"}, false},
		{`set llm.local.stop ""`, []string{"set", "llm.local.stop", ""}, false},
		{`set llm.local.stop ''`, []string{"set", "llm.local.stop", ""}, false},
		{`set llm.local.stop "unterminated`, nil, true},
		{`--all`, []string{"--all"}, false},
		{``, nil, false},
	}
	for _, c := range cases {
		got, err := splitArgs(c.in)
		if c.isErr {
			if err == nil {
				t.Errorf("%q was accepted as %v; an unterminated quote must refuse", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%q split into %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q split into %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

// TestClearingAnOptionalSettingIsExpressible. Several llm.local.* settings
// document "" as "unset, omit the field", so an empty value is a real value —
// and one an argument splitter that dropped empty tokens would make
// unreachable from the composer, where there is no shell to do the quoting.
func TestClearingAnOptionalSettingIsExpressible(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := runFlags(t, "flags", "set", flags.LLMLocalTemperature, "0.2"); err != nil {
		t.Fatal(err)
	}
	out, err := runFlags(t, "flags", "set", flags.LLMLocalTemperature, "")
	if err != nil {
		t.Fatalf("clearing an optional setting was refused: %v", err)
	}
	if !strings.Contains(out, flags.LLMLocalTemperature) {
		t.Errorf("the report does not name the cleared key:\n%s", out)
	}
}

// TestApplyFlagChangeUnderConcurrentAttachment.
//
// The reload drops each session's cached provider so the next turn resolves it
// again. That write runs on the settings command's goroutine while other
// sessions may be reading the same block to start a turn. Run with -race. The
// invariant beyond the detector is that a reader never sees half a change: a
// provider from before the drop next to a model from after it would start a
// turn under a configuration that never existed.
func TestApplyFlagChangeUnderConcurrentAttachment(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	defer setProjectRootForTest(root)()

	h := &harnessHost{reg: newTestRegistry(t), sessions: map[string]*tuiSession{}}
	for _, id := range []string{"S1", "S2", "S3"} {
		s, err := newSessionState(h.reg, id, nil)
		if err != nil {
			t.Fatalf("session %s: %v", id, err)
		}
		h.sessions[id] = s
	}

	stop := make(chan struct{})
	var readers, writer sync.WaitGroup

	for _, s := range h.sessions {
		readers.Add(1)
		go func(s *tuiSession) {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				at := s.attachment()
				// Nil provider and an empty model go together, and so do a set
				// provider and a set model. A mix is a torn read.
				if (at.provider == nil) != (at.model == "") {
					t.Errorf("torn attachment: provider=%v model=%q", at.provider, at.model)
					return
				}
			}
		}(s)
	}

	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; i < 400; i++ {
			h.applyFlagChange(flags.LLMDefaultProvider)
			h.applyFlagChange(flags.GrantsAgentEnabled)
			h.applyFlagChange(flags.PolicyFileMode)
		}
	}()
	writer.Wait()
	close(stop)
	readers.Wait()
}

// TestApplyFlagChangeReloadsTheGrantPolicy. The announcement is only honest if
// the reload behind it happened.
func TestApplyFlagChangeReloadsTheGrantPolicy(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	defer setProjectRootForTest(root)()

	h := &harnessHost{reg: newTestRegistry(t), sessions: map[string]*tuiSession{}}
	s, err := newSessionState(h.reg, "S1", nil)
	if err != nil {
		t.Fatal(err)
	}
	h.sessions["S1"] = s

	if !s.gate.Ledger.AgentMayGrant("scope.unplanned") {
		t.Fatal("agent grants should start enabled; the fixture asserts nothing otherwise")
	}
	if err := h.reg.Set(flags.Human, flags.GrantsAgentEnabled, "false"); err != nil {
		t.Fatal(err)
	}
	h.applyFlagChange(flags.GrantsAgentEnabled)
	if s.gate.Ledger.AgentMayGrant("scope.unplanned") {
		t.Fatal("the session's ledger still permits agent grants after the flag was switched off")
	}
}

// TestSettingsNoticeCarriesPostureAndTheWeakenedList. The status bar folds both
// in, and a notice that named only the changed key would leave every other
// session's bar describing a strictness that had stopped being true.
func TestSettingsNoticeCarriesPostureAndTheWeakenedList(t *testing.T) {
	h := &harnessHost{reg: newTestRegistry(t)}
	if err := h.reg.Set(flags.Human, flags.HarnessPosture, flags.PostureYolo); err != nil {
		t.Fatal(err)
	}
	ev := h.settingsNotice(flags.HarnessPosture, reloadPlanFor(flags.HarnessPosture))

	if ev.Posture != flags.PostureYolo {
		t.Errorf("notice posture = %q, want yolo", ev.Posture)
	}
	if !strings.Contains(ev.Text, flags.HarnessPosture) || !strings.Contains(ev.Text, flags.PostureYolo) {
		t.Errorf("notice text = %q, want the key and its new value", ev.Text)
	}
	var found bool
	for _, w := range ev.Weakened {
		if strings.HasPrefix(w, flags.HarnessPosture+"=") {
			found = true
		}
	}
	if !found {
		t.Errorf("notice weakened = %v, want it to list the relaxed posture", ev.Weakened)
	}
	if !ev.Qualified() {
		t.Error("a notice carrying a weakened list does not report as qualified")
	}
}
