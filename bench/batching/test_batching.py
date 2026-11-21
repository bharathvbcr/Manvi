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
