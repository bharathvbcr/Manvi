"""Unit tests for the five-arm batching study: its task keys (checked against the
source they claim to describe), arms, schedule, prompts, power arithmetic and
verdict rules. No network, no binary. Run: python3 bench/batching2/test_batching2.py"""
import json
import os
import re
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import run_batching2 as b  # noqa: E402

ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
SPEC = b.load_spec()
TASKS = {t["id"]: t for t in SPEC["tasks"]}

# What a correct answer says for every fact of the code-reading tasks. The
# trails' answers are read from the fixtures, so they cannot drift from them.
GOLD = {
    "batchToolKey": "harness.batch_tool", "batchToolWithheld": "batchToolOff",
    "WithholdBatch": "devcouncil/tools.go", "ScriptToolName": "devcouncil_script",
    "scriptMaxSteps": "1_000_000", "serialLookupsKey": "harness.serial_lookups",
    "serialLookupMode": "agent.SerialLookupsRefuse", "SerialLookups": "deps.serialLookups",
    "serialLookupRefuse": "5", "refuseSerialLookup": "refused, not run", "serialLookups": "int",
    "defaultTimeout": "20 * time.Second", "defaultMaxBytes": "2 << 20", "defaultMaxRedirects": "5",
    "maxPipedPromptBytes": "1 << 20", "DefaultMaxSessions": "50", "DefaultMaxBytes": "8 << 20",
    "maxLoadBytes": "8 * DefaultMaxBytes", "CalibrationMin": "0.4", "CalibrationMax": "2.5",
}


def read(path):
    with open(os.path.join(ROOT, path), encoding="utf-8") as f:
        return f.read()


class Tasks(unittest.TestCase):
    def test_every_fact_is_confirmed_in_the_source_it_names(self):
        for t in SPEC["tasks"]:
            for f in t["facts"]:
                self.assertTrue(f["verify"], (t["id"], f["symbol"]))
                for path, pattern in f["verify"]:
                    self.assertRegex(read(path), re.compile(pattern, re.M), (t["id"], f["symbol"], path))
                if t["kind"] != "trail":  # a trail's file is a random name, checked by the walk below
                    self.assertTrue(f["file"].endswith(".go"), (t["id"], f["symbol"]))

    def test_every_task_key_is_satisfied_by_its_own_gold_answer_and_by_nothing_wrong(self):
        for t in SPEC["tasks"]:
            lines, wrong = [], []
            for f in t["facts"]:
                val = f["value"].replace("\\", "") if t["kind"] == "trail" else GOLD[f["symbol"]]
                lines.append("%s = %s (%s)" % (f["symbol"], val, f["file"]))
                wrong.append("%s = zzz (%s)" % (f["symbol"], f["file"]))
            n, flags = b.rb.grade("\n".join(lines), t["facts"])
            self.assertEqual(n, len(t["facts"]), (t["id"], flags))
            self.assertEqual(b.rb.grade("\n".join(wrong), t["facts"])[0], 0, t["id"])

    def test_each_trail_follows_refers_to_links_from_its_start_through_every_hop(self):
        by_id = {}
        for dirpath, _, names in os.walk(os.path.join(HERE, "fixtures", "ledger")):
            for n in names:
                fields = dict(line.split(": ", 1) for line in read(os.path.relpath(
                    os.path.join(dirpath, n), ROOT)).splitlines())
                self.assertNotIn(fields["record"], by_id, "duplicate record id")
                by_id[fields["record"]] = (fields, n)
        self.assertGreaterEqual(len(by_id), 400)  # too few records would let one grep dump the whole graph
        trails = [t for t in SPEC["tasks"] if t["kind"] == "trail"]
        self.assertEqual(len(trails), 2)
        for t in trails:
            cur = t["trail"]["start"]
            self.assertEqual(len(t["facts"]), t["trail"]["hops"])
            self.assertEqual(len(t["items"]), t["trail"]["hops"])
            for f in t["facts"]:
                fields, name = by_id[cur]
                self.assertEqual((f["symbol"], f["file"]), (cur, name))
                self.assertEqual(f["value"].replace("\\", ""), fields["amount-code"])
                cur = fields["refers-to"]

    def test_the_committed_fixture_is_what_the_generator_writes_byte_for_byte(self):
        import filecmp
        import tempfile
        import gen_ledger
        with tempfile.TemporaryDirectory() as d:
            trails = gen_ledger.generate(d)
            cmp = filecmp.dircmp(d, os.path.join(HERE, "fixtures", "ledger"))
            stack, diffs = [cmp], []
            while stack:
                c = stack.pop()
                diffs += c.left_only + c.right_only + c.diff_files + c.funny_files
                stack += list(c.subdirs.values())
            self.assertEqual(diffs, [])
        for t, trail in zip((TASKS["trail-a"], TASKS["trail-b"]), trails):
            self.assertEqual([f["symbol"] for f in t["facts"]], [h["id"] for h in trail])

    def test_clear_sessions_removes_the_session_logs_and_nothing_else(self):
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            os.makedirs(os.path.join(d, ".devcouncil", "sessions"))
            with open(os.path.join(d, ".devcouncil", "sessions", "x.json"), "w") as f:
                f.write("RX-1234")
            with open(os.path.join(d, ".devcouncil", "config.yaml"), "w") as f:
                f.write("k: v")
            b.clear_sessions(d)
            self.assertFalse(os.path.exists(os.path.join(d, ".devcouncil", "sessions")))
            self.assertTrue(os.path.exists(os.path.join(d, ".devcouncil", "config.yaml")))
            b.clear_sessions(d)  # already gone is fine

    def test_the_two_trails_share_no_record(self):
        a, c = (set(f["symbol"] for f in TASKS[k]["facts"]) for k in ("trail-a", "trail-b"))
        self.assertFalse(a & c)

    def test_kinds_have_a_preamble_and_strata_cover_the_chained_kinds(self):
        for t in SPEC["tasks"]:
            self.assertIn(t["kind"], SPEC["preambles"])
        chained = {t["id"] for t in SPEC["tasks"] if t["kind"] in b.STRATA["chained"]}
        self.assertEqual(chained, {"chain-batch-switch", "chain-serial-rule", "trail-a", "trail-b"})


class Design(unittest.TestCase):
    def test_arm_environments(self):
        self.assertEqual(b.ARMS["A"], {"MANVI_HARNESS_BATCH_TOOL": "off"})
        self.assertEqual(b.ARMS["C"], b.ARMS["D"])
        self.assertEqual(b.ARMS["C"], b.ARMS["S"])  # S differs by its prompt alone
        self.assertEqual(b.ARMS["R"], {"MANVI_HARNESS_BATCH_TOOL": "on", "MANVI_HARNESS_SERIAL_LOOKUPS": "off"})
        # C keeps the default serial-lookups mode: the variable is stripped, never set.
        self.assertIn("MANVI_HARNESS_SERIAL_LOOKUPS", b.rb.STRIP_ENV)
        self.assertNotIn("MANVI_HARNESS_SERIAL_LOOKUPS", b.ARMS["C"])
        self.assertEqual(set(b.ARMS), set(b.ARM_IDS))

    def test_only_arm_s_prompt_names_the_script_tool(self):
        for t in SPEC["tasks"]:
            prompts = {arm: b.build_prompt(SPEC, t, arm) for arm in b.ARM_IDS}
            self.assertEqual({a for a, p in prompts.items() if "devcouncil_script" in p}, {"S"}, t["id"])
            self.assertEqual(len({p for a, p in prompts.items() if a != "S"}), 1)
            self.assertTrue(prompts["S"].startswith(prompts["C"].split("\n\n1. ")[0]))

    def test_orders_balance_every_arm_at_every_position(self):
        orders = b.balanced_orders()
        self.assertEqual(len(set(orders)), 10)
        for pos in range(5):
            self.assertEqual({sum(1 for o in orders if o[pos] == a) for a in b.ARM_IDS}, {2})

    def test_schedule_is_deterministic_complete_and_balanced_over_the_planned_study(self):
        tasks = SPEC["tasks"]
        s = b.schedule(tasks, PLANNED_REPS)
        self.assertEqual(s, b.schedule(tasks, PLANNED_REPS))
        self.assertEqual(len(s), len(tasks) * PLANNED_REPS)
        self.assertEqual(len({(t, r) for t, r, _ in s}), len(s))
        for pos in range(5):
            counts = {a: sum(1 for _, _, o in s if o[pos] == a) for a in b.ARM_IDS}
            self.assertEqual(len(set(counts.values())), 1, counts)
        self.assertEqual(len({o for _, _, o in s}), 10)

    def test_a_different_seed_gives_a_different_schedule(self):
        self.assertNotEqual(b.schedule(SPEC["tasks"], 4, seed=1), b.schedule(SPEC["tasks"], 4, seed=2))

    def test_primary_family_and_alpha(self):
        self.assertEqual([p[0] for p in b.PRIMARY], ["P1", "P2", "P3"])
        self.assertAlmostEqual(b.FAMILY_ALPHA, 0.05 / 3)
        self.assertEqual(b.NOISE_FLOOR, ("D", "C"))


PLANNED_REPS = 20


class Power(unittest.TestCase):
    def stats(self):
        with open(os.path.join(HERE, "..", "batching", "results", "stats_prereg_n12.json")) as f:
            return json.load(f)

    def test_planning_sds_are_recovered_from_the_first_studys_intervals(self):
        sds = b.planning_sds(self.stats())
        self.assertAlmostEqual(sds["B-A"], 33634, delta=5)
        self.assertAlmostEqual(sds["C-A"], 21703, delta=5)
        self.assertAlmostEqual(sds["C-B"], 28992, delta=5)

    def test_pairs_needed_and_detectable_effect_invert_each_other(self):
        sd, alpha = 30000.0, b.FAMILY_ALPHA
        n = b.pairs_needed(sd, 10000.0, alpha)
        self.assertLessEqual(b.detectable_effect(sd, n, alpha), 10000.0)
        self.assertGreater(b.detectable_effect(sd, n - 1, alpha), 10000.0)

    def test_the_planned_study_is_powered_for_what_the_preregistration_claims(self):
        sd = max(b.planning_sds(self.stats()).values())
        units = len(SPEC["tasks"]) * PLANNED_REPS
        self.assertGreaterEqual(units, b.pairs_needed(sd, 10000.0, b.FAMILY_ALPHA) + 0)
        chained = sum(1 for t in SPEC["tasks"] if t["kind"] in b.STRATA["chained"]) * PLANNED_REPS
        self.assertLess(b.detectable_effect(sd, chained, b.FAMILY_ALPHA), 13000.0)
        with open(os.path.join(HERE, "PREREG.md"), encoding="utf-8") as f:
            prereg = f.read()
        self.assertIn("%d units" % units, prereg)
        self.assertIn("%d pairs" % b.pairs_needed(sd, 10000.0, b.FAMILY_ALPHA), prereg)


class Verdicts(unittest.TestCase):
    def d(self, lo, hi):
        return {"ci95": [lo, hi]}

    def test_each_reading(self):
        self.assertTrue(b.verdict(self.d(-9, -1), 0.0, True).startswith("supported"))
        self.assertTrue(b.verdict(self.d(-9, 1), 0.0, True).startswith("not shown"))
        self.assertTrue(b.verdict(self.d(1, 9), 0.0, True).startswith("harm shown"))

    def test_a_correctness_drop_refutes_even_a_cheaper_arm(self):
        self.assertTrue(b.verdict(self.d(-9, -1), -0.06, True).startswith("refuted"))
        self.assertTrue(b.verdict(self.d(-9, -1), -0.05, True).startswith("supported"))

    def test_a_mechanism_that_never_ran_is_not_exercised_not_null(self):
        self.assertTrue(b.verdict(self.d(-9, 1), 0.0, True, exercised=False).startswith("not exercised"))
        self.assertTrue(b.verdict(self.d(-9, -1), 0.0, True, exercised=False).startswith("not exercised"))
        self.assertTrue(b.verdict(self.d(-9, -1), 0.0, False, exercised=False).startswith("not interpretable"))

    def test_a_failed_noise_floor_makes_every_reading_uninterpretable(self):
        for lo, hi in ((-9, -1), (-9, 1), (1, 9)):
            self.assertTrue(b.verdict(self.d(lo, hi), 0.0, False).startswith("not interpretable"))


class Analysis(unittest.TestCase):
    def rows(self, tokens, refusals=None):
        rows = []
        for t in SPEC["tasks"]:
            for rep in range(6):
                for arm in b.ARM_IDS:
                    tok = tokens(t, rep, arm)
                    rows.append({"task": t["id"], "rep": rep, "arm": arm, "input_tokens": tok, "cost_nano_usd": tok * 10,
                                 "steps": 4, "output_tokens": 1, "tool_calls": 3, "correct": len(t["facts"]),
                                 "facts": len(t["facts"]), "exit_code": 0, "turn_usage_events": 1,
                                 "batch_calls": 0 if arm == "A" else 1,
                                 "batch_inner_calls": 2, "writes": 0, "notices": [],
                                 "refusals": 1 if arm == "C" and t["kind"] == "trail" else 0,
                                 "serial_notes": 0, "script_calls": 1 if arm == "S" else 0, "unpriced_steps": 0})
        return rows

    def test_primary_verdicts_and_usage_on_a_planted_effect(self):
        # Planted: C is 5000 below A everywhere; S equals C; C is 2000 below R on chained tasks.
        def tok(t, rep, arm):
            base = 100000 + 700 * rep  # same unit effect in every arm, so deltas are exact
            chained = t["kind"] in b.STRATA["chained"]
            return base + {"A": 5000, "C": 0, "D": 0, "S": 0, "R": 2000 if chained else 0}[arm]
        rep = b.analyze_all(self.rows(tok), SPEC, secondary=True)
        self.assertEqual(len(rep["secondary"]), 24)
        self.assertTrue(rep["noise_floor"]["gate_ok"])
        p = rep["primary"]
        self.assertEqual(p["P1"]["input_tokens"]["mean"], -5000.0)
        self.assertTrue(p["P1"]["verdict"].startswith("supported"))
        self.assertEqual(p["P2"]["input_tokens"]["mean"], 0.0)
        self.assertTrue(p["P2"]["verdict"].startswith("not shown"))
        self.assertEqual((p["P3"]["n_pairs"], p["P3"]["input_tokens"]["mean"]), (24, -2000.0))
        # The fixture's C refuses once in each of the 12 trail units, over the minimum of 10.
        self.assertEqual(rep["c_chained_runs_with_refusal"], 12)
        self.assertTrue(p["P3"]["verdict"].startswith("supported"))
        rows = self.rows(tok)
        for r in rows:
            if r["arm"] == "C" and r["task"] == "trail-b":
                r["refusals"] = 0
        few = b.analyze_all(rows, SPEC, secondary=False)
        self.assertEqual(few["c_chained_runs_with_refusal"], 6)  # below the minimum of 10
        self.assertTrue(few["primary"]["P3"]["verdict"].startswith("not exercised"))
        self.assertTrue(few["primary"]["P1"]["verdict"].startswith("supported"))  # only P3 needs the refusal
        u = rep["usage"]
        self.assertEqual(u["S/all"]["runs_with_script"], 36)
        self.assertEqual(u["C/all"]["runs_with_script"], 0)
        self.assertEqual((u["C/trail"]["runs_with_refusal"], u["C/independent"]["runs_with_refusal"]), (12, 0))
        self.assertEqual(u["R/all"]["runs_with_refusal"], 0)

    def test_an_arm_that_did_not_get_its_treatment_voids_its_comparisons(self):
        rows = self.rows(lambda t, rep, arm: 100000 + (5000 if arm == "A" else 0))
        clean = b.analyze_all(rows, SPEC, secondary=False)
        self.assertEqual(clean["arm_checks"]["A"]["violations"], 0)
        self.assertEqual(clean["arm_checks"]["R"]["violations"], 0)
        for r in rows:
            if r["arm"] == "R" and r["task"] == "trail-a" and r["rep"] == 0:
                r["serial_notes"] = 1  # an R run that was noted: the refusal was not off
        noted = b.analyze_all(rows, SPEC, secondary=False)
        self.assertEqual(noted["arm_checks"]["R"]["violations"], 1)
        self.assertTrue(noted["primary"]["P3"]["verdict"].startswith("not interpretable"))
        rows = self.rows(lambda t, rep, arm: 100000 + (5000 if arm == "A" else 0))
        for r in rows:
            if r["arm"] == "A" and r["task"] == "limits-fetch" and r["rep"] == 0:
                r["script_calls"] = 1
        bad = b.analyze_all(rows, SPEC, secondary=False)
        self.assertEqual(bad["arm_checks"]["A"]["violations"], 1)
        self.assertTrue(bad["primary"]["P1"]["verdict"].startswith("not interpretable"))
        self.assertFalse(bad["primary"]["P2"]["verdict"].startswith("not interpretable"))  # P2 does not involve A

    def test_a_noisy_c2_voids_the_primary_readings(self):
        def tok(t, rep, arm):
            return 100000 + (30000 if arm == "D" else 0) + {"A": 5000}.get(arm, 0)
        rep = b.analyze_all(self.rows(tok), SPEC, secondary=False)
        self.assertFalse(rep["noise_floor"]["gate_ok"])
        self.assertTrue(all(v["verdict"].startswith("not interpretable") for v in rep["primary"].values()))

    def test_an_arm_missing_from_a_cell_drops_that_pair_not_the_study(self):
        rows = self.rows(lambda t, rep, arm: 100000)
        rows = [r for r in rows if not (r["task"] == "trail-a" and r["rep"] == 0 and r["arm"] == "S")]
        rep = b.analyze_all(rows, SPEC, secondary=False)
        self.assertEqual(rep["primary"]["P2"]["n_pairs"], len(SPEC["tasks"]) * 6 - 1)
        self.assertEqual(rep["primary"]["P1"]["n_pairs"], len(SPEC["tasks"]) * 6)

    def test_a_failed_run_is_never_zero_filled(self):
        rows = self.rows(lambda t, rep, arm: 100000)
        for r in rows:
            if r["task"] == "limits-fetch" and r["rep"] == 0 and r["arm"] == "C":
                r["exit_code"] = 1
        rep = b.analyze_all(rows, SPEC, secondary=False)
        self.assertEqual(rep["primary"]["P1"]["n_pairs"], len(SPEC["tasks"]) * 6 - 1)
        self.assertEqual(rep["primary"]["P1"]["dropped_pairs"], [["limits-fetch", 0]])


if __name__ == "__main__":
    # verify.sh reads the count from the last line, so a run that collected
    # nothing is told apart from one that passed.
    result = unittest.TextTestRunner(verbosity=1).run(
        unittest.defaultTestLoader.loadTestsFromModule(sys.modules[__name__]))
    failed = len(result.failures) + len(result.errors)
    print(f"{result.testsRun - failed} passed, {failed} failed")
    sys.exit(0 if result.wasSuccessful() and result.testsRun > 0 else 1)
