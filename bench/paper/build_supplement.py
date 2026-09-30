#!/usr/bin/env python3
"""Build the anonymised supplementary-material ZIP for double-blind review.

    python3 bench/paper/build_supplement.py [--ref HEAD] [--verify]

The source of truth is a committed tree (`--ref`, default HEAD), read through
git; the working tree is never read, so uncommitted edits cannot ship and
cannot hide anything. Every file under bench/ must be covered by exactly one
POLICY rule (include or exclude, each with its reason); a file no rule covers
fails the build rather than being shipped or dropped silently.

Fails closed, exit 1, listing every finding as file:line, when a shipped
byte or path carries an identity marker, a commit hash of this repository, a
host name or IP address, a binary or non-UTF-8 file, a symlink, or when a
rewrite rule is stale, or the archive is over the size limit. Neutral
substitutions are explicit REWRITES, counted and listed in the archive's
README; the result is re-scanned after rewriting and again after it is written.

The archive is deterministic: sorted entries, 1980-01-01 timestamps, fixed
permissions, no extra fields. Two builds of one ref on one machine are
byte-identical (deflate output depends on the zlib version).

`--verify` unpacks the archive into a fresh directory outside the repository
and runs every command its README tells a reviewer to run, from there.
"""
import argparse
import fnmatch
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import zipfile
from dataclasses import dataclass, field

HERE = os.path.dirname(os.path.abspath(__file__))
REPO_DEFAULT = os.path.dirname(os.path.dirname(HERE))
OUT_DEFAULT = os.path.join(HERE, "supplement")
ZIP_NAME = "supplement.zip"
ZIP_ROOT = "supplement"
FIXED_DATE = (1980, 1, 1, 0, 0, 0)
MAX_BYTES_DEFAULT = 100 * 1000 * 1000  # TMLR: supplementary material <= 100 MB
GIT_TIMEOUT_S = 120
VERIFY_TIMEOUT_S = 1800


class BuildError(Exception):
    def __init__(self, message, findings=()):
        super().__init__(message)
        self.findings = list(findings)


# --- what ships -----------------------------------------------------------------
# First matching rule wins. `**` spans directories; other segments are globs.
POLICY = (
    ("bench/mh/*.py", "include", "the harness package: agent loop, tools, sandbox, verification, statistics"),
    ("bench/run.py", "include", "the runner"),
    ("bench/grid.py", "include", "the grid driver"),
    ("bench/compare.py", "include", "tables and bootstrap statistics from run logs"),
    ("bench/figures.py", "include", "figures, rendered from the stats JSON"),
    ("bench/report.py", "include", "head-to-head report from run logs"),
    ("bench/probe.py", "include", "provider probe used before the API-served extension arm"),
    ("bench/selftest.py", "include", "task-suite self-test"),
    ("bench/stress_test.py", "include", "adversarial harness tests"),
    ("bench/test_*.py", "include", "unit and regression tests"),
    ("bench/expand_hard.sh", "include", "driver of the superseded v1 grid, cited as a record"),
    ("bench/wait_expand.sh", "include", "companion of expand_hard.sh"),
    ("bench/README.md", "include", "harness usage"),
    ("bench/DESIGN.md", "include", "harness design"),
    ("bench/tasks/**", "include", "the benchmark tasks"),
    ("bench/paper/stats-*.json", "include", "compare summaries behind the reported tables and figures"),
    ("bench/paper/sensitivity.py", "include", "derives the preregistered section 7 sensitivity bounds from the stats summaries"),
    ("bench/paper/preregistration.md", "include", "the registered design"),
    ("bench/paper/DEVIATIONS.md", "include", "every departure from the registration"),
    ("bench/paper/extension-cerebras.md", "include", "registration of the extension arm"),
    ("bench/paper/run_v2.sh", "include", "the six-phase driver that ran the reported grid"),
    ("bench/paper/archives/*.md", "include", "provenance notes for archived, unreported data"),
    ("bench/.gitignore", "exclude", "repository housekeeping"),
    ("bench/live/**", "exclude", "drives the authors' separate production agent (Go), not the Python harness"),
    ("bench/results/**", "exclude", "raw run logs carry host paths; released after acceptance"),
    ("bench/paper/harness_architecture.*", "exclude", "the manuscript itself"),
    ("bench/paper/header.tex", "exclude", "manuscript typesetting"),
    ("bench/paper/references.bib", "exclude", "the manuscript's bibliography; the harness does not use it"),
    ("bench/paper/litreview.md", "exclude", "the author's literature-review notes for the manuscript"),
    ("bench/paper/AUDIT.md", "exclude", "the author's internal audit notes (findings are fixed in the shipped code)"),
    ("bench/paper/build_pdf.sh", "exclude", "manuscript build; carries the author block"),
    ("bench/paper/prepare_body.py", "exclude", "manuscript build; carries the withheld commit hash"),
    ("bench/paper/svg2png.py", "exclude", "manuscript figure rasteriser"),
    ("bench/paper/tmlr/**", "exclude", "venue style files"),
    ("bench/paper/workshop/**", "exclude", "a different manuscript"),
    ("bench/paper/figures/**", "exclude", "manuscript figures; figures.py regenerates them from the stats JSON"),
    ("bench/paper/README.md", "exclude", "the author's revision notes"),
    ("bench/paper/SUBMISSION.md", "exclude", "the author's submission plan"),
    ("bench/paper/build_supplement.py", "exclude", "this builder"),
    ("bench/paper/test_supplement.py", "exclude", "this builder's tests"),
    ("bench/paper/supplement/**", "exclude", "this builder's output"),
)


def _seg_match(pat, parts):
    if not pat:
        return not parts
    if pat[0] == "**":
        return any(_seg_match(pat[1:], parts[i:]) for i in range(len(parts) + 1))
    return bool(parts) and fnmatch.fnmatchcase(parts[0], pat[0]) and _seg_match(pat[1:], parts[1:])


def classify(path):
    """(action, reason) of the first POLICY rule covering `path`, else None."""
    parts = path.split("/")
    for pattern, action, reason in POLICY:
        if _seg_match(pattern.split("/"), parts):
            return action, reason
    return None


# --- neutral substitutions ------------------------------------------------------
@dataclass(frozen=True)
class Rewrite:
    path: str       # exact repository path the rule applies to
    old: str
    new: str
    reason: str     # printed in the archive README; must not quote `old`
    required: bool = True  # a required rule that matches nothing is stale: fail


DEFAULT_REWRITES = (
    Rewrite("bench/README.md",
            "It is deliberately *not* MANVI. MANVI is the production harness — policy ladder,\n"
            "grants, leases, Rust verifier. This is",
            "It is deliberately *not* a production agent harness. This is",
            "named the authors' production agent and its features"),
    Rewrite("bench/DESIGN.md", "(inherited from this repo's MANVI rules)",
            "(inherited from the parent project's rules)",
            "named the authors' production agent"),
    Rewrite("bench/mh/model.py", "working MANVI run", "working production-agent run",
            "product name in a code comment"),
    Rewrite("bench/test_gemini_wire.py", "working MANVI\nrun", "working production-agent\nrun",
            "product name in a docstring"),
    Rewrite("bench/tasks/globmatch/hidden_test.py", ".devcouncil/", ".toolstate/",
            "a tool-state directory name in glob test data (fnmatch is the oracle, "
            "so every expected answer is unchanged)"),
    Rewrite("bench/test_pool.py", '"Mac-12582.lan"', '"laptop-01.example"',
            "a real workstation host name used as test data (any distinct host serves)"),
    Rewrite("bench/paper/run_v2.sh", "cd /home/ubuntu/manvi-bench",
            'cd "$(dirname "$0")/.."  # run-host checkout path withheld for review',
            "the run host's absolute checkout path, replaced by the script's own location"),
    Rewrite("bench/paper/DEVIATIONS.md", "82e453a", "<instrument commit, withheld for review>",
            "repository commit hash of the instrument revision"),
    Rewrite("bench/paper/DEVIATIONS.md", "855acc1", "<fix commit, withheld for review>",
            "repository commit hash of a harness fix"),
    Rewrite("bench/test_stats.py", "82e453a", "<withheld-commit>",
            "repository commit hash in probes of the (excluded) manuscript build",
            required=False),
)


@dataclass
class Applied:
    path: str
    rule: str
    count: int


# JSON provenance keys whose values are machine names. Replaced by stable
# pseudonyms, one mapping across every shipped file, so equal hosts stay equal.
HOST_KEYS = ("env_node", "env_client_node", "hostname")
_HOST_VALUE = re.compile(r'("(?:%s)"\s*:\s*)"((?:[^"\\]|\\.)*)"' % "|".join(HOST_KEYS))


# --- identity markers -----------------------------------------------------------
STATIC_MARKERS = (
    "manvi", "bharath", "vaddaram", "vbcr", "78684269", "gmail", "/users/", "/home/",
    "devcouncil", "gitpulse", "devmap", "scholarlm", "wisdev",
    "82e453a",  # the instrument commit the review manuscript withholds
)
# Identities in the history that are not the author and must not be markers.
_NOT_IDENTIFYING_EMAILS = {"noreply@anthropic.com", "noreply@github.com"}
_STOP_TOKENS = {"user", "users", "test", "admin", "root", "github", "noreply", "claude",
                "none", "unknown", "localhost"}
_HASH_TOKEN = re.compile(r"(?<![0-9A-Za-z])[0-9A-Fa-f]{7,40}(?![0-9A-Za-z])")
_IPV4 = re.compile(r"(?<![\w.-])(\d{1,3})([.-])(\d{1,3})\2(\d{1,3})\2(\d{1,3})(?![\w-]|\.\d)")
_LAN_HOST = re.compile(r"\b[A-Za-z0-9-]+\.lan\b", re.I)


def _git(repo, *args, check=True, input_bytes=None):
    r = subprocess.run(["git", *args], cwd=repo, capture_output=True, input=input_bytes,
                       timeout=GIT_TIMEOUT_S)
    if check and r.returncode != 0:
        raise BuildError(f"git {' '.join(args)} failed: {r.stderr.decode(errors='replace').strip()}")
    return r


def identity_markers(repo, commit):
    """Author/committer names and emails in the history, the configured user and
    the remotes' owner and URL -- on top of STATIC_MARKERS."""
    found = set(STATIC_MARKERS)
    log = _git(repo, "log", "--format=%an%x1f%ae%x1f%cn%x1f%ce", commit).stdout.decode()
    people = set()
    for line in log.splitlines():
        f = line.split("\x1f")
        if len(f) == 4:
            people.update({(f[0], f[1]), (f[2], f[3])})
    for key in ("user.name", "user.email"):
        v = _git(repo, "config", "--get", key, check=False).stdout.decode().strip()
        if v:
            people.add((v, "") if key == "user.name" else ("", v))
    for name, email in people:
        email = email.lower()
        if email in _NOT_IDENTIFYING_EMAILS or name.endswith("[bot]") or email.endswith("[bot]@users.noreply.github.com"):
            continue
        for s in (name.strip(), email.strip()):
            if len(s) >= 4:
                found.add(s.lower())
        tokens = name.split() + re.split(r"[.+_\-]", email.split("@")[0])
        found.update(t.lower() for t in tokens if len(t) >= 4 and t.lower() not in _STOP_TOKENS)
    remotes = _git(repo, "remote", "-v", check=False).stdout.decode()
    for url in {l.split()[1] for l in remotes.splitlines() if len(l.split()) >= 2}:
        found.add(url.lower().removesuffix(".git"))
        m = re.match(r"(?:\w+://)?(?:[^@/]+@)?([^/:]+)[/:]([^/]+)/([^/]+?)(?:\.git)?/?$", url)
        if m:
            host, owner, name = m.groups()
            found.update({f"{host}/{owner}".lower(), owner.lower()})
            if len(name) >= 4:
                found.add(name.lower())
    return sorted(found)


def _line_of(text, pos):
    return text.count("\n", 0, pos) + 1


class Scanner:
    def __init__(self, repo, markers):
        self.repo = repo
        self.markers = markers
        self._marker_re = re.compile("|".join(re.escape(m) for m in sorted(markers, key=len, reverse=True)), re.I)
        self._hash_cache = {}

    def _resolving(self, tokens):
        """The subset of hex tokens that name a commit in this repository.
        `ambiguous` counts as resolving: it means more than one object matches."""
        todo = sorted({t.lower() for t in tokens} - set(self._hash_cache))
        if todo:
            out = _git(self.repo, "cat-file", "--batch-check",
                       input_bytes="".join(f"{t}^{{commit}}\n" for t in todo).encode()).stdout.decode()
            lines = out.splitlines()
            if len(lines) != len(todo):
                raise BuildError("git cat-file --batch-check answered %d of %d hash probes"
                                 % (len(lines), len(todo)))
            for t, line in zip(todo, lines):
                self._hash_cache[t] = not line.endswith(" missing")
        return {t for t in tokens if self._hash_cache[t.lower()]}

    def _text_findings(self, where, text):
        out = []
        for m in self._marker_re.finditer(text):
            out.append(f"{where(m.start())}: identity marker {m.group()!r}")
        for m in _LAN_HOST.finditer(text):
            out.append(f"{where(m.start())}: host name {m.group()!r}")
        for m in _IPV4.finditer(text):
            octets = [int(m.group(i)) for i in (1, 3, 4, 5)]
            if max(octets) > 255 or octets[0] == 127 or not any(octets):
                continue
            out.append(f"{where(m.start())}: IP address {m.group()!r}")
        hexes = [(m.start(), m.group()) for m in _HASH_TOKEN.finditer(text)]
        hit = self._resolving([h for _, h in hexes])
        for pos, h in hexes:
            if h in hit:
                out.append(f"{where(pos)}: commit hash of this repository {h!r}")
        return out

    def scan(self, arcname, data):
        findings = self._text_findings(lambda _p: f"{arcname}: path", arcname)
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            return findings + [f"{arcname}: not UTF-8 text; refusing to ship an unscanned binary"]
        if "\x00" in text:
            return findings + [f"{arcname}: binary (NUL bytes); refusing to ship an unscanned binary"]
        findings += self._text_findings(lambda p: f"{arcname}:{_line_of(text, p)}", text)
        if arcname.endswith((".json", ".ipynb")):
            try:
                doc = json.loads(text)
            except ValueError as e:
                return findings + [f"{arcname}: JSON does not parse ({e}); refusing to ship it unscanned"]
            strings = []

            def walk(x):
                if isinstance(x, dict):
                    for k, v in x.items():
                        strings.append(k)
                        walk(v)
                elif isinstance(x, list):
                    for v in x:
                        walk(v)
                elif isinstance(x, str):
                    strings.append(x)
            walk(doc)
            # Catches what \u escapes hide from the raw-text pass; a hit the raw
            # pass already reported is not reported twice.
            raw = {f.split(": ", 1)[1] for f in findings}
            where = f"{arcname}: decoded JSON string"
            findings += [f for f in self._text_findings(lambda _p: where, "\n".join(strings))
                         if f.split(": ", 2)[2] not in raw]
        return findings


# --- the build -------------------------------------------------------------------
@dataclass
class BuildResult:
    path: str
    entries: int
    size: int
    sha256: str
    included: list
    excluded: dict
    rewrites: list
    pseudonyms: dict
    by_dir: dict = field(default_factory=dict)


VERIFY_COMMANDS = (
    ("selftest.py", ["selftest.py"], "every task starts broken, accepts its reference solution, rejects tampering"),
    ("stress_test.py", ["stress_test.py"], "adversarial tests of the harness itself, no GPU"),
    ("test_stats.py", ["test_stats.py"], "bootstrap, paired-delta, coverage and figure tests, no GPU"),
    ("test_pool.py", ["test_pool.py"], "cell assembly and pooling refusals, compare.py end to end on synthetic cells"),
    ("test_runtime.py", ["test_runtime.py"], "resume, extension and starvation semantics"),
    ("test_compute.py", ["test_compute.py"], "GPU telemetry parsing, decode tok/s"),
    ("test_tools.py", ["test_tools.py"], "the five agent tools"),
    ("test_gemini_wire.py", ["test_gemini_wire.py"], "Gemini request shape, no network or credential"),
    ("test_cerebras_wire.py", ["test_cerebras_wire.py"], "Cerebras request/response shape, no network or credential"),
    ("figures stats-all3", ["figures.py", "paper/stats-all3.json", "figures-out/all3"], "Figures 3-6"),
    ("figures stats-v2", ["figures.py", "paper/stats-v2.json", "figures-out/v2"], "the graphical abstract"),
)


def _render_readme(counts, rewrites, n_pseudonyms):
    cmds = "\n".join(f"python3 {' '.join(argv):<44} # {what}" for _, argv, what in VERIFY_COMMANDS)
    dirs = "\n".join(f"| `{d}` | {n} |" for d, n in sorted(counts.items()))
    if rewrites:
        rw = "\n".join(f"| `{a.path}` | {a.rule} | {a.count} |" for a in rewrites)
        rw = "| File | What was substituted | Occurrences |\n|---|---|---|\n" + rw
    else:
        rw = "None."
    hosts = (f"\nMachine names in the provenance blocks of `bench/paper/stats-*.json` "
             f"(keys {', '.join('`%s`' % k for k in HOST_KEYS)}) are replaced by stable pseudonyms "
             f"(`host-01` ... `host-{n_pseudonyms:02d}`); equal hosts map to equal pseudonyms "
             f"across files, so every protocol-drift finding is preserved.\n") if n_pseudonyms else ""
    return f"""# Supplementary material: harness, tasks and statistics

Anonymised for double-blind review. This archive holds the measurement harness,
the benchmark tasks, the runner, the statistics code, the self-tests, and the
compare summaries behind the reported tables. Python 3.10 or later, standard
library only; no GPU, network or credential is needed for anything below.

## Layout

```
bench/mh/            the harness package: model clients, tools and sandbox, agent loop,
                     task loading and verification, statistics, pooling, runtime guards
bench/tasks/<name>/  TASK.md, setup/, reference/, hidden_test.*, task.json
bench/run.py         the runner (one resident model, seed pinned per repeat)
bench/grid.py        the resumable grid driver
bench/compare.py     tables and bootstrap CIs from run logs
bench/figures.py     figures, pure functions of a stats JSON
bench/selftest.py, bench/stress_test.py, bench/test_*.py   self-tests
bench/paper/stats-v2.json          compare summary behind the main tables
bench/paper/stats-all3.json        three-arm family; source of the interaction figures
bench/paper/stats-ext-cerebras.json   the API-served extension arm
bench/paper/stats-hard.json        the superseded v1 grid (instrument-failure record)
bench/paper/preregistration.md, DEVIATIONS.md, extension-cerebras.md, run_v2.sh, archives/
bench/README.md, bench/DESIGN.md   harness usage and design
```

| Directory | Files |
|---|---|
{dirs}

## Reproduction

```bash
cd bench
{cmds}
```

Each self-test ends with a summary line carrying its count and exits non-zero on
any failure. `test_stats.py` also probes the manuscript build tooling, which is
not part of this archive; those probes are reported as skipped on a line that
begins `SKIP <n> paper probes:`.

The shell tool is contained with `sandbox-exec` on macOS and `bwrap` on Linux.

`compare.py` and `report.py` read the per-episode run logs under
`bench/results/`. The run logs are not included here (they carry host-specific
paths); they are available after acceptance. Their outputs for the reported
grids are included as `bench/paper/stats-*.json`, from which `figures.py`
regenerates the figures as shown above. With the run logs in place:

```bash
python3 compare.py --tag v2 --json-out paper/stats-v2.json
python3 compare.py --tag v2,ext-cerebras
python3 compare.py --tag hard --exclude gemini --json-out results/stats-hard.json
```

Running the grid itself needs a local model server (ollama) or an API key for
the extension arm; see `bench/README.md`.

## This code versus the instrument that produced the reported grid

The reported episodes were run on an earlier revision of this harness (the
paper calls it the instrument, and its identifier is withheld for review). The
files shipped here are the submission revision. Between the two, the harness
files changed in three ways, none of which alters what the model sees or how an
episode is scored:

- `mh/tools.py`, `mh/harness.py`: the containment provenance record is built by
  one function (`containment_event`), which fixes episodes on Linux being
  stamped "not OS-contained" although `bwrap` contained them; and tool dispatch
  goes through an explicit table of the same five tools instead of `getattr`.
- `mh/pool.py`, `compare.py`: the cross-arm protocol check compares every pair
  of arms (the paper's §5.7 describes the defect this fixed).
- `bench/tasks/`: identical to the instrument's task files.

## Anonymisation

Files are the committed versions of the submission revision, byte-for-byte,
except for these neutral substitutions:

{rw}
{hosts}
Not included: the manuscript and its build scripts, venue style files, the
manuscript's figure files (regenerable as above), submission notes, and a
separate driver for the authors' production agent that the Python harness does
not use.
"""


def _write_zip(path, files, modes):
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for name in sorted(files):
            zi = zipfile.ZipInfo(name, date_time=FIXED_DATE)
            zi.compress_type = zipfile.ZIP_DEFLATED
            zi.create_system = 3
            zi.external_attr = (0o100000 | modes.get(name, 0o644)) << 16
            zi.extra = b""
            z.writestr(zi, files[name], compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)


def build(repo=REPO_DEFAULT, ref="HEAD", out_dir=OUT_DEFAULT, max_bytes=MAX_BYTES_DEFAULT,
          rewrites=DEFAULT_REWRITES):
    repo = os.path.realpath(repo)
    r = _git(repo, "rev-parse", "--verify", "--quiet", f"{ref}^{{commit}}", check=False)
    if r.returncode != 0:
        raise BuildError(f"ref {ref!r} does not name a commit")
    commit = r.stdout.decode().strip()

    findings = []
    listing = _git(repo, "ls-tree", "-r", "-z", "--full-tree", commit, "--", "bench").stdout
    included, excluded, blobs = [], {}, {}
    for rec in filter(None, listing.split(b"\x00")):
        meta, path = rec.split(b"\t", 1)
        mode, kind, oid = meta.decode().split()
        path = path.decode("utf-8", errors="surrogateescape")
        c = classify(path)
        if c is None:
            findings.append(f"{path}: no POLICY rule covers this file; classify it as include or exclude")
            continue
        action, reason = c
        if action == "exclude":
            excluded[path] = reason
            continue
        if mode == "120000":
            findings.append(f"{path}: symlink; refusing to ship a link")
            continue
        if kind != "blob" or mode not in ("100644", "100755"):
            findings.append(f"{path}: {kind} with mode {mode}; only regular files ship")
            continue
        included.append(path)
        blobs[path] = (oid, 0o755 if mode == "100755" else 0o644)
    if not included:
        findings.append("nothing to ship: no committed file under bench/ matched an include rule")

    contents = {}
    if blobs:
        order = sorted(blobs)
        out = _git(repo, "cat-file", "--batch",
                   input_bytes="".join(blobs[p][0] + "\n" for p in order).encode()).stdout
        pos = 0
        for p in order:
            nl = out.index(b"\n", pos)
            oid, _kind, size = out[pos:nl].decode().split()
            size = int(size)
            if oid != blobs[p][0]:
                raise BuildError(f"git cat-file answered {oid} for {p}")
            contents[p] = out[nl + 1:nl + 1 + size]
            pos = nl + 1 + size + 1

    # Explicit substitutions, counted. Only text that decodes is rewritten; the
    # scan below refuses anything that does not.
    applied = []
    for rw in rewrites:
        if rw.path not in contents:
            findings.append(f"{rw.path}: rewrite rule ({rw.reason}) targets a file that does not ship")
            continue
        try:
            text = contents[rw.path].decode("utf-8")
        except UnicodeDecodeError:
            continue
        n = text.count(rw.old)
        if n == 0:
            if rw.required:
                findings.append(f"{rw.path}: stale rewrite rule ({rw.reason}) matched nothing")
            continue
        contents[rw.path] = text.replace(rw.old, rw.new).encode("utf-8")
        applied.append(Applied(rw.path, rw.reason, n))

    # Host pseudonyms: one mapping, sorted, across every shipped JSON file.
    hosts = set()
    for p, b in contents.items():
        if p.endswith(".json"):
            try:
                hosts.update(m.group(2) for m in _HOST_VALUE.finditer(b.decode("utf-8")))
            except UnicodeDecodeError:
                pass
    pseudonyms = {h: f"host-{i:02d}" for i, h in enumerate(sorted(hosts), 1)}
    for p in list(contents):
        if p.endswith(".json") and pseudonyms:
            try:
                text = contents[p].decode("utf-8")
            except UnicodeDecodeError:
                continue
            new = _HOST_VALUE.sub(lambda m: f'{m.group(1)}"{pseudonyms[m.group(2)]}"', text)
            if new != text:
                contents[p] = new.encode("utf-8")

    by_dir = {}
    for p in included:
        parts = p.split("/")
        d = "bench/tasks/" if parts[1] == "tasks" else "/".join(parts[:-1]) + "/"
        by_dir[d] = by_dir.get(d, 0) + 1

    files = {f"{ZIP_ROOT}/{p}": b for p, b in contents.items()}
    modes = {f"{ZIP_ROOT}/{p}": blobs[p][1] for p in contents}
    files[f"{ZIP_ROOT}/README.md"] = _render_readme(by_dir, applied, len(pseudonyms)).encode("utf-8")

    scanner = Scanner(repo, identity_markers(repo, commit))
    for name in sorted(files):
        findings += scanner.scan(name, files[name])
    if findings:
        raise BuildError(f"refusing to build: {len(findings)} finding(s)", findings)

    os.makedirs(out_dir, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".supplement-", suffix=".zip.tmp", dir=out_dir)
    os.close(fd)
    try:
        _write_zip(tmp, files, modes)
        # Read back what was written: what ships is the file, not the dict.
        post = []
        with zipfile.ZipFile(tmp) as z:
            if z.comment:
                post.append("archive comment present")
            names = [i.filename for i in z.infolist()]
            if names != sorted(files):
                post.append("archive entries differ from the scanned set")
            for i in z.infolist():
                if i.date_time != FIXED_DATE or i.extra or i.create_system != 3:
                    post.append(f"{i.filename}: non-deterministic metadata")
                data = z.read(i)
                if data != files.get(i.filename):
                    post.append(f"{i.filename}: content differs from what was scanned")
                post += scanner.scan(i.filename, data)
        if post:
            raise BuildError(f"written archive failed its re-scan: {len(post)} finding(s)", post)
        size = os.path.getsize(tmp)
        if size > max_bytes:
            raise BuildError(f"archive is {size:,} bytes, over the {max_bytes:,}-byte limit",
                             [f"size {size:,} > limit {max_bytes:,}"])
        with open(tmp, "rb") as f:
            sha = hashlib.sha256(f.read()).hexdigest()
        final = os.path.join(out_dir, ZIP_NAME)
        os.chmod(tmp, 0o644)  # mkstemp's 0600 would make the upload unreadable to others
        os.replace(tmp, final)
    except BaseException:
        if os.path.exists(tmp):
            os.unlink(tmp)
        raise
    return BuildResult(final, len(files), size, sha, sorted(included), excluded, applied,
                       pseudonyms, by_dir)


# --- verification from the unpacked copy ------------------------------------------
_SUMMARY_INT = re.compile(r"\d+")
_PAPER_SKIP = re.compile(r"^SKIP (\d+) paper probes: \S")
# test_stats.py probes the manuscript build (prepare_body.py and the manuscript),
# neither of which ships, so in the unpacked copy it must skip exactly this many
# probes on exactly one line. A different count means the probe set changed
# under the supplement, and is a failure until this number is updated.
PAPER_SKIPS_EXPECTED = 30


def judge(name, rc, output, err="", paper_skips=PAPER_SKIPS_EXPECTED):
    """(ok, summary). Success is exit 0, no traceback, a last stdout line whose
    first number is positive, and no SKIP line. test_stats.py is the exception
    and must print exactly one `SKIP <paper_skips> paper probes: ...` line.
    stdout and stderr are separate: a suite that logs to stderr would otherwise
    displace its own summary line (it is block-buffered, stderr is not)."""
    lines = [l for l in output.splitlines() if l.strip()]
    last = lines[-1].strip() if lines else ""
    if rc != 0:
        tail = [l for l in (output + "\n" + err).splitlines() if l.strip()]
        return False, f"exit {rc}: {tail[-1].strip() if tail else '(no output)'}"
    if "Traceback (most recent call last)" in output + err:
        return False, f"traceback under exit 0: {last}"
    skips = [l for l in lines + err.splitlines() if l.lstrip().upper().startswith("SKIP")]
    if name == "test_stats.py":
        m = _PAPER_SKIP.match(skips[0].strip()) if len(skips) == 1 else None
        if not m or int(m.group(1)) != paper_skips:
            return False, (f"expected exactly one 'SKIP {paper_skips} paper probes:' line, "
                           f"got {[s.strip() for s in skips] or 'none'}")
    elif skips:
        return False, f"unexpected skip: {skips[0].strip()}"
    m = _SUMMARY_INT.search(last)
    if not m or int(m.group()) <= 0:
        return False, f"no positive count on the summary line: {last or '(no output)'}"
    return True, last + (f"  [{skips[0].strip()}]" if skips else "")


def outside(path, repo):
    p, r = os.path.realpath(path), os.path.realpath(repo)
    return os.path.commonpath([p, r]) != r


def safe_extract(zip_path, dest):
    dest = os.path.realpath(dest)
    with zipfile.ZipFile(zip_path) as z:
        for i in z.infolist():
            n = i.filename
            target = os.path.realpath(os.path.join(dest, n))
            if (n.startswith("/") or "\\" in n or ".." in n.split("/")
                    or os.path.commonpath([target, dest]) != dest):
                raise BuildError(f"archive entry {n!r} escapes the extraction root")
            if n.endswith("/"):
                os.makedirs(target, exist_ok=True)
                continue
            os.makedirs(os.path.dirname(target), exist_ok=True)
            with open(target, "wb") as f:
                f.write(z.read(i))
            os.chmod(target, (i.external_attr >> 16) & 0o777 or 0o644)


def verify(zip_path, repo=REPO_DEFAULT, timeout=VERIFY_TIMEOUT_S, keep=False):
    root = tempfile.mkdtemp(prefix="supplement-verify-")
    if not outside(root, repo):
        raise BuildError(f"extraction directory {root} is inside the repository")
    results = []
    try:
        safe_extract(zip_path, root)
        bench = os.path.join(root, ZIP_ROOT, "bench")
        env = {k: v for k, v in os.environ.items()
               if k not in ("PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP") and "API_KEY" not in k}
        env["PYTHONDONTWRITEBYTECODE"] = "1"
        for name, argv, _ in VERIFY_COMMANDS:
            try:
                r = subprocess.run([sys.executable, *argv], cwd=bench, env=env, capture_output=True,
                                   text=True, timeout=timeout)
                rc, out, err = r.returncode, r.stdout, r.stderr
            except subprocess.TimeoutExpired as e:
                rc, out, err = -1, f"timed out after {timeout}s", str(e.stderr or "")
            if argv[0] == "figures.py":
                outdir = os.path.join(bench, argv[2])
                svgs = sorted(f for f in os.listdir(outdir) if f.endswith(".svg")) if os.path.isdir(outdir) else []
                ok = rc == 0 and bool(svgs) and "Traceback" not in out + err
                summary = f"exit {rc}: {len(svgs)} SVGs ({', '.join(svgs)})"
            else:
                ok, summary = judge(name, rc, out, err)
            results.append((name, ok, rc, summary, out + ("\n[stderr]\n" + err if err.strip() else "")))
    finally:
        if keep:
            print(f"kept extraction at {root}")
        else:
            shutil.rmtree(root, ignore_errors=True)
    return results


def main(argv=None, rewrites=DEFAULT_REWRITES):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--repo", default=REPO_DEFAULT)
    ap.add_argument("--ref", default="HEAD", help="committed tree to build from (default HEAD)")
    ap.add_argument("--out-dir", default=OUT_DEFAULT)
    ap.add_argument("--max-bytes", type=int, default=MAX_BYTES_DEFAULT)
    ap.add_argument("--verify", action="store_true",
                    help="unpack outside the repository and run the README's commands")
    ap.add_argument("--keep", action="store_true", help="keep the verification directory")
    ap.add_argument("--timeout", type=int, default=VERIFY_TIMEOUT_S)
    a = ap.parse_args(argv)
    try:
        res = build(a.repo, a.ref, a.out_dir, a.max_bytes, rewrites)
    except BuildError as e:
        print(f"FAIL: {e}", file=sys.stderr)
        for f in e.findings:
            print(f"  {f}", file=sys.stderr)
        return 1
    ignored = _git(a.repo, "check-ignore", "-q", res.path, check=False).returncode == 0
    print(f"built from ref {a.ref}")
    print(f"  path:    {res.path}")
    print(f"  entries: {res.entries}")
    print(f"  bytes:   {res.size:,}")
    print(f"  sha256:  {res.sha256}")
    print("  shipped by directory: " + ", ".join(f"{d} {n}" for d, n in sorted(res.by_dir.items())))
    print(f"  excluded: {len(res.excluded)} file(s)")
    for p, why in sorted(res.excluded.items()):
        print(f"    {p}: {why}")
    print(f"  rewrites applied: {len(res.rewrites)}")
    for x in res.rewrites:
        print(f"    {x.path}: {x.rule} (x{x.count})")
    print(f"  host pseudonyms: {len(res.pseudonyms)}")
    print("  scan: clean (paths and contents, before and after writing)")
    if not ignored:
        print(f"WARNING: {os.path.relpath(res.path, a.repo)} is not gitignored; add "
              f"`bench/paper/supplement/` to .gitignore", file=sys.stderr)
    if not a.verify:
        return 0
    results = verify(res.path, a.repo, a.timeout, a.keep)
    print("verification from the unpacked copy:")
    for name, ok, rc, summary, out in results:
        print(f"  {'PASS' if ok else 'FAIL'} {name}: {summary}")
        if not ok:
            print("    | " + "\n    | ".join(out.strip().splitlines()[-15:]))
    if all(ok for _, ok, *_ in results):
        return 0
    bad = res.path[:-len(".zip")] + ".UNVERIFIED.zip"
    os.replace(res.path, bad)
    print(f"FAIL: verification failed; archive moved to {bad} -- do not upload it", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
