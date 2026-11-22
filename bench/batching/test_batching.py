"""Unit tests for the batching benchmark's parser, grader, schedule and pairing.
No network, no binary. Run: python3 bench/batching/test_batching.py"""
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import run_batching as rb  # noqa: E402


def ev(**k):
    return json.dumps(k)


class Parse(unittest.TestCase):
    def test_counts_batch_inner_calls_refusals_and_final_text(self):
        lines = [
            ev(kind="tool.start", tool="devcouncil_batch", arguments={"calls": [{}, {}, {}]}),
            ev(kind="tool.start", tool="devcouncil_read_file"),
            ev(kind="tool.start", tool="devcouncil_read_file", agent="child"),
            ev(kind="tool.result", text="refused, not run: 5 steps in a row"),
            ev(kind="step.usage"), ev(kind="step.usage"),
            ev(kind="turn.usage", input_tokens=100, output_tokens=7, cache_read_tokens=40, cost_nano_usd=900),
            ev(kind="assistant.text", text="first"), ev(kind="assistant.text", text="last"),
            "not json",
        ]
        m = rb.parse_stream(lines)
        self.assertEqual((m["batch_calls"], m["batch_inner_calls"], m["tool_calls"]), (1, 3, 2))
        self.assertEqual((m["refusals"], m["refused_steps"], m["steps"], m["bad_lines"]), (1, 5, 2, 1))
        self.assertEqual((m["input_tokens"], m["cost_nano_usd"], m["final_text"]), (100, 900, "firstlast"))

    def test_final_text_joins_stream_chunks_after_the_last_tool_result(self):
        lines = [ev(kind="assistant.text", text="plan"), ev(kind="tool.result", text="x"),
                 ev(kind="assistant.text", text="The "), ev(kind="assistant.text", text="answer"),
                 ev(kind="step.usage")]
        self.assertEqual(rb.parse_stream(lines)["final_text"], "The answer")

    def test_child_usage_is_not_counted_twice(self):
        m = rb.parse_stream([ev(kind="turn.usage", input_tokens=5, agent="child"), ev(kind="turn.usage", input_tokens=7)])
        self.assertEqual(m["input_tokens"], 7)


class ParseMechanisms(unittest.TestCase):
    note = ("file contents\n\n[harness: 3 steps in a row each made a single lookup. Lookups that do not "
            "depend on each other's answers belong in one devcouncil_batch call. A lone lookup that would "
            "make 5 in a row is refused once.]")
    refusal = ("[harness: refused, not run: 4 steps in a row each made a single lookup. Send the lookups "
               "you still need together in one devcouncil_batch call.]")

    def test_script_calls_and_serial_notes_are_counted_apart_from_refusals(self):
        m = rb.parse_stream([
            ev(kind="tool.start", tool="devcouncil_script", arguments={"source": "x"}),
            ev(kind="tool.start", tool="devcouncil_script", agent="child"),
            ev(kind="tool.start", tool="devcouncil_grep"),
            ev(kind="tool.result", text=self.note),
            ev(kind="tool.result", text=self.refusal),
            ev(kind="tool.result", text="plain"),
        ])
        self.assertEqual((m["script_calls"], m["tool_calls"]), (1, 2))
        self.assertEqual((m["serial_notes"], m["refusals"], m["refused_steps"]), (1, 1, 4))

    def test_the_refusal_text_is_not_mistaken_for_the_note(self):
        m = rb.parse_stream([ev(kind="tool.result", text=self.refusal)])
        self.assertEqual((m["serial_notes"], m["refusals"]), (0, 1))


class Study(unittest.TestCase):
    """The generalised pieces a study with other arms is built from."""

    def row(self, task, rep, arm, tokens):
        return {"task": task, "rep": rep, "arm": arm, "input_tokens": tokens, "cost_nano_usd": tokens,
                "steps": 3, "output_tokens": 1, "tool_calls": 2, "correct": 5, "facts": 5, "exit_code": 0,
                "turn_usage_events": 1, "batch_calls": 0, "batch_inner_calls": 0, "refusals": 1 if arm == "R" else 0,
                "notices": [], "writes": 0, "script_calls": 2 if arm == "S" else 0}

    def rows(self):
        return [self.row("t", rep, arm, tok) for rep in range(4)
                for arm, tok in (("A", 500), ("C", 400), ("S", 300), ("R", 450))]

    def test_analyze_takes_its_own_arms_comparisons_and_alpha(self):
        rep = rb.analyze(self.rows(), arms="ACSR", comparisons=(("S", "C"), ("C", "R")), alpha=0.0167)
        self.assertEqual(sorted(rep["arms"]), ["A", "C", "R", "S"])
        self.assertEqual(sorted(rep["deltas"]), ["C-R", "S-C"])
        self.assertEqual(rep["deltas"]["S-C"]["input_tokens"]["mean"], -100.0)
        self.assertEqual(rep["arms"]["S"]["runs_with_script"], 4)
        self.assertEqual(rep["arms"]["R"]["runs_with_refusal"], 4)
        self.assertEqual(rep["arms"]["A"]["runs_with_script"], 0)

    def test_every_interval_records_the_level_it_was_drawn_at(self):
        # The key is "ci95" for compatibility with the first study's stats;
        # the level it holds is whatever alpha the analysis ran at.
        rep = rb.analyze(self.rows(), arms="ACSR", comparisons=(("S", "C"),), alpha=0.0167)
        self.assertAlmostEqual(rep["deltas"]["S-C"]["input_tokens"]["ci_level"], 0.9833)
        self.assertAlmostEqual(rep["arms"]["C"]["input_tokens"]["ci_level"], 0.9833)
        self.assertAlmostEqual(rb.analyze(self.rows(), arms="AC", comparisons=(("C", "A"),))["deltas"]["C-A"]["input_tokens"]["ci_level"], 0.95)

    def test_a_cell_missing_one_of_the_named_arms_is_dropped(self):
        rows = [r for r in self.rows() if not (r["rep"] == 3 and r["arm"] == "S")]
        rep = rb.analyze(rows, arms="ACSR", comparisons=(("S", "C"),))
        self.assertEqual((rep["n_pairs"], len(rep["dropped_pairs"])), (3, 1))

    def test_scrub_replaces_the_checkout_and_home_paths(self):
        home = os.path.expanduser("~")
        text = "open %s/work/clean/a.go and %s/.cache/x" % (home, home)
        out = rb.scrub(text, home + "/work/clean")
        self.assertNotIn(home, out)
        self.assertIn("<repo>/a.go", out)
        self.assertEqual(rb.scrub(["%s/work/clean" % home], home + "/work/clean"), ["<repo>"])

    def test_run_one_uses_the_arm_table_it_is_given(self):
        seen = {}

        class Done:
            stdout = ev(kind="step.usage") + "\n"
            stderr = "boom in /clean/repo"
            returncode = 0

        def fake(cmd, **kw):
            seen["env"] = kw["env"]
            return Done()

        import tempfile
        real = rb.subprocess.run
        rb.subprocess.run = fake
        try:
            with tempfile.TemporaryDirectory() as d:
                m = rb.run_one("bin", "/clean/repo", "p", "Z", os.path.join(d, "raw"), 5,
                               arms={"Z": {"MANVI_HARNESS_SERIAL_LOOKUPS": "off"}})
        finally:
            rb.subprocess.run = real
        self.assertEqual(seen["env"]["MANVI_HARNESS_SERIAL_LOOKUPS"], "off")
        self.assertEqual(m["stderr_tail"], "boom in <repo>")

    def test_run_study_resumes_stops_at_the_budget_and_runs_jobs_in_parallel(self):
        import tempfile
        import threading
        import types
        spec = {"tasks": [{"id": "t%d" % i, "facts": [{"file": "f.go", "symbol": "s", "value": "1"}]} for i in range(4)]}
        orders = [("t%d" % i, 0, "AB") for i in range(4)]
        calls, live, peak = [], [0], [0]
        lock = threading.Lock()

        def fake_run_one(binary, repo, prompt, arm, scratch, timeout_s, arms=None):
            with lock:
                live[0] += 1
                peak[0] = max(peak[0], live[0])
                calls.append((prompt, arm))
            import time
            time.sleep(0.05)
            with lock:
                live[0] -= 1
            m = rb.parse_stream([ev(kind="step.usage"), ev(kind="turn.usage", cost_nano_usd=1_000_000_000),
                                 ev(kind="assistant.text", text="f.go s = 1")])
            m.update(exit_code=0, wall_s=0.0, stderr_tail="")
            return m

        real = rb.run_one
        rb.run_one = fake_run_one
        try:
            with tempfile.TemporaryDirectory() as d:
                a = types.SimpleNamespace(out=d, scratch=d, manvi="b", repo="r", timeout=1, budget_usd=100.0)
                self.assertEqual(rb.run_study(a, spec, {"A": {}, "B": {}}, orders, lambda s, t, arm: t["id"] + arm, jobs=4), 0)
                self.assertEqual(len(calls), 8)
                self.assertGreater(peak[0], 1)
                rows = [json.loads(line) for line in open(os.path.join(d, "runs.jsonl"))]
                self.assertEqual(len(rows), 8)
                self.assertEqual(rb.run_study(a, spec, {"A": {}, "B": {}}, orders, lambda s, t, arm: "", jobs=2), 0)
                self.assertEqual(len(calls), 8)  # nothing is paid for twice
                a.budget_usd = 9.5  # $8 already spent, so the next run starts and the one after stops
                orders2 = orders + [("t0", 1, "AB"), ("t1", 1, "AB")]
                self.assertEqual(rb.run_study(a, spec, {"A": {}, "B": {}}, orders2, lambda s, t, arm: "", jobs=1), 3)
                self.assertEqual(len(calls), 8 + 2)  # $8 and $9 are under 9.5 and start a run; $10 stops the next
        finally:
            rb.run_one = real

    def test_an_invalid_run_is_retried_once_each_job_keeps_its_own_checkout_and_cleanup_runs(self):
        import tempfile
        import threading
        import types
        spec = {"tasks": [{"id": "t%d" % i, "facts": [{"file": "f.go", "symbol": "s", "value": "1"}]} for i in range(6)]}
        orders = [("t%d" % i, 0, "A") for i in range(6)]
        seen, cleaned, flaky = [], [], {"t0": 2}  # t0 fails twice, so its retry also fails
        lock = threading.Lock()

        def fake_run_one(binary, repo, prompt, arm, scratch, timeout_s, arms=None):
            task = prompt
            with lock:
                seen.append((task, repo))
                bad = flaky.get(task, 0) > 0
                if bad:
                    flaky[task] -= 1
            events = [ev(kind="turn.usage", cost_nano_usd=100)] if not bad else []
            m = rb.parse_stream(events + ([ev(kind="step.usage")] if not bad else []))
            m.update(exit_code=0 if not bad else 1, wall_s=0.0, stderr_tail="")
            return m

        real = rb.run_one
        rb.run_one = fake_run_one
        try:
            with tempfile.TemporaryDirectory() as d:
                a = types.SimpleNamespace(out=d, scratch=d, manvi="b", repo="unused", timeout=1, budget_usd=100.0)
                rc = rb.run_study(a, spec, {"A": {}}, orders, lambda s, t, arm: t["id"], jobs=2,
                                  repos=["r1", "r2"], after_run=cleaned.append, retries=1)
                self.assertEqual(rc, 0)
                rows = {r["task"]: r for r in map(json.loads, open(os.path.join(d, "runs.jsonl")))}
        finally:
            rb.run_one = real
        self.assertEqual({repo for _, repo in seen}, {"r1", "r2"})
        self.assertEqual(len(cleaned), len(seen))  # after every run, retries included
        self.assertEqual(len([1 for t, _ in seen if t == "t0"]), 2)  # one retry, no more
        self.assertEqual((rows["t0"]["attempts"], rows["t0"]["exit_code"]), (2, 1))  # still invalid: stored as such
        self.assertEqual({rows["t%d" % i]["attempts"] for i in range(1, 6)}, {1})
        flaky.update(t1=1)
        self.assertFalse(rb.run_valid(rows["t0"]))
        self.assertTrue(rb.run_valid(rows["t1"]))


class Grade(unittest.TestCase):
    facts = [{"symbol": "defaultMaxBytes", "value": r"(2\s*<<\s*20|2\s*MiB)", "file": "fetch.go"},
             {"symbol": "ProtectedTail", "value": r"\b6\b", "file": "compaction.go"}]

    def test_right_wrong_and_missing_file(self):
        good = "defaultMaxBytes = 2 << 20 (fetch/fetch.go)\nProtectedTail = 6 in agent/compaction.go"
        self.assertEqual(rb.grade(good, self.facts)[0], 2)
        wrong = "defaultMaxBytes = 4 << 20 (fetch/fetch.go)\nProtectedTail = 8 in agent/compaction.go"
        self.assertEqual(rb.grade(wrong, self.facts)[0], 0)
        self.assertEqual(rb.grade("defaultMaxBytes = 2 MiB, ProtectedTail = 6", self.facts)[0], 0)
        self.assertEqual(rb.grade("", self.facts)[0], 0)

    def test_value_far_from_symbol_does_not_count(self):
        text = "fetch.go compaction.go ProtectedTail " + "x" * 400 + " 6"
        self.assertEqual(rb.grade(text, self.facts[1:])[0], 0)

    def test_every_task_key_is_satisfied_by_its_own_gold_answer(self):
        spec = rb.load_tasks()
        gold = {"defaultTimeout": "20 * time.Second", "defaultMaxBytes": "2 << 20", "defaultMaxRedirects": "5",
                "maxPipedPromptBytes": "1 << 20", "ProtectedTail": "6", "maxImageTokens": "4784",
                "serverKeepToolUses": "3", "minThreshold": "4096", "maxEscalationCarryRunes": "1200",
                "DefaultMaxSessions": "50", "DefaultMaxBytes": "8 << 20", "maxLoadBytes": "8 * DefaultMaxBytes",
                "CalibrationMin": "0.4", "CalibrationMax": "2.5", "APIVersion": "2023-06-01"}
        urls = ["https://api.anthropic.com", "https://api.x.ai/v1",
                "https://generativelanguage.googleapis.com/v1beta", "http://127.0.0.1:8000/v1"]
        for t in spec["tasks"]:
            url_i = 0
            lines = []
            for f in t["facts"]:
                if f["symbol"] == "DefaultBaseURL":
                    val = urls[url_i]
                    url_i += 1
                else:
                    val = gold[f["symbol"]]
                lines.append("%s = %s (%s)" % (f["symbol"], val, f["file"]))
            n, flags = rb.grade("\n".join(lines), t["facts"])
            self.assertEqual(n, len(t["facts"]), (t["id"], flags))


class Schedule(unittest.TestCase):
    def test_arm_positions_are_balanced_over_twelve_units(self):
        s = rb.schedule(rb.load_tasks()["tasks"], 3)
        self.assertEqual(len(s), 12)
        for pos in range(3):
            counts = {a: sum(1 for _, _, o in s if o[pos] == a) for a in "ABC"}
            self.assertEqual(set(counts.values()), {4}, counts)
        self.assertEqual(len({(t, r) for t, r, _ in s}), 12)


class Pairing(unittest.TestCase):
    def row(self, task, rep, arm, tokens, exit_code=0):
        return {"task": task, "rep": rep, "arm": arm, "input_tokens": tokens, "cost_nano_usd": tokens * 1000,
                "steps": 3, "output_tokens": 1, "tool_calls": 2, "correct": 5, "facts": 5, "exit_code": exit_code,
                "turn_usage_events": 1, "batch_calls": 0, "batch_inner_calls": 0, "refusals": 0, "notices": [], "writes": 0}

    def test_failed_or_incomplete_pairs_are_dropped_not_zeroed(self):
        rows = []
        for rep in range(3):
            for arm, tok in (("A", 300), ("B", 200), ("C", 100)):
                rows.append(self.row("t", rep, arm, tok, exit_code=1 if (rep == 2 and arm == "B") else 0))
        rows.append(self.row("u", 0, "A", 1))
        rep = rb.analyze(rows)
        self.assertEqual(rep["n_pairs"], 2)
        self.assertEqual(len(rep["dropped_pairs"]), 2)
        d = rep["deltas"]["C-A"]["input_tokens"]
        self.assertEqual((d["mean"], d["negative"], d["n"]), (-200.0, 2, 2))


if __name__ == "__main__":
    # verify.sh reads the count from the last line, so a run that collected
    # nothing is told apart from one that passed.
    result = unittest.TextTestRunner(verbosity=1).run(
        unittest.defaultTestLoader.loadTestsFromModule(sys.modules[__name__]))
    failed = len(result.failures) + len(result.errors)
    print(f"{result.testsRun - failed} passed, {failed} failed")
    sys.exit(0 if result.wasSuccessful() and result.testsRun > 0 else 1)
