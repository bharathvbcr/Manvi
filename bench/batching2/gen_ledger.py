#!/usr/bin/env python3
"""Regenerates bench/batching2/fixtures/ledger, the pointer-chase fixture of the
trail-a and trail-b tasks (see PREREG.md). Deterministic from SEED; a unit test
regenerates it and compares every byte, so the committed fixture cannot drift from
this file. Prints the two trails as JSON (the source of their tasks.json keys).

  gen_ledger.py OUTDIR
"""
import json
import os
import random
import sys

SEED = 20261121
RECORDS = 480
SHARDS = 12
HOPS = 8
WORDS = ["tide", "ember", "lark", "quill", "moss", "rivet", "cinder", "fable", "pylon", "wren", "alder", "basalt"]
NOTES = ["Filed after the quarterly reconciliation.", "Held pending a second signature.",
         "Carried forward from the prior ledger.", "Amount confirmed by the clerk on duty.",
         "Superseded in part; see the linked record.", "No dispute recorded.",
         "Posted late because the office was closed.", "Cross-checked against the paper copy."]


def generate(out):
    rnd = random.Random(SEED)
    ids = ["RX-%04d" % i for i in rnd.sample(range(1000, 9999), RECORDS)]
    codes = set()

    def code():
        while True:
            c = "%s-%d" % (rnd.choice(WORDS), rnd.randint(10, 99))
            if c not in codes:
                codes.add(c)
                return c

    trail_a = rnd.sample(range(RECORDS), HOPS)
    trail_b = rnd.sample([i for i in range(RECORDS) if i not in trail_a], HOPS)
    refs = {}
    for trail in (trail_a, trail_b):
        for a, b in zip(trail, trail[1:]):
            refs[a] = b
    # Every other record, including each trail's last, refers to a random record.
    for i in range(RECORDS):
        if i not in refs:
            refs[i] = rnd.choice([j for j in range(RECORDS) if j != i])
    meta, used = {}, set()
    for i in range(RECORDS):
        while True:
            name = "r%05x.rec" % rnd.randrange(16 ** 5)
            if name not in used:
                used.add(name)
                break
        meta[i] = ("s%d" % rnd.randrange(SHARDS), name, code())
    for i in range(RECORDS):
        shard, name, c = meta[i]
        os.makedirs(os.path.join(out, shard), exist_ok=True)
        body = "record: %s\nfiled: 2025-%02d-%02d\nnote: %s\namount-code: %s\nrefers-to: %s\n" % (
            ids[i], rnd.randint(1, 12), rnd.randint(1, 28), rnd.choice(NOTES), c, ids[refs[i]])
        with open(os.path.join(out, shard, name), "w") as f:
            f.write(body)
    return [[{"id": ids[i], "code": meta[i][2], "path": "bench/batching2/fixtures/ledger/%s/%s" % (meta[i][0], meta[i][1])}
             for i in trail] for trail in (trail_a, trail_b)]


if __name__ == "__main__":
    print(json.dumps(generate(sys.argv[1])))
