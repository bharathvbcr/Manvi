"""Adversarial tests for build_supplement.py. Plain python3: no network, no TeX.

Every scenario that can leak an identity is planted in a throwaway git
repository and the build is required to refuse it. The last block builds the
real repository at HEAD (committed content only) and checks the archive
independently of the builder's own scanner.
"""
import contextlib
import hashlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
sys.path.insert(0, HERE)

import build_supplement as B  # noqa: E402

PASS, FAIL = [], []


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(("  ok   " if cond else "  FAIL ") + name + ("" if cond else f"  -- {detail}"))


def raises(fn, exc=B.BuildError):
    try:
        fn()
    except exc as e:
        return e
    return None


# --- fixture repositories ---------------------------------------------------
# Isolated from the user's git config: no global hooks, no signing, fixed dates.
AUTHOR = "Quillon Farthingale"
EMAIL = "quill.farthing@example.org"
REMOTE = "https://github.com/fixture-owner/fixture-repo.git"
GIT_ENV = dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM="1",
               GIT_AUTHOR_NAME=AUTHOR, GIT_AUTHOR_EMAIL=EMAIL,
               GIT_COMMITTER_NAME=AUTHOR, GIT_COMMITTER_EMAIL=EMAIL,
               GIT_AUTHOR_DATE="2001-01-01T00:00:00Z",
               GIT_COMMITTER_DATE="2001-01-01T00:00:00Z")

CLEAN_STATS = json.dumps({"cells": {}, "provenance": {
    "m|full": {"env_node": "192-0-2-10", "env_client_node": "Mac-1.lan"},
    "m|base": {"env_node": "192-0-2-10", "env_client_node": "Mac-2.lan"}}},
    indent=1) + "\n"

BASE = {
    "bench/README.md": "# harness\n",
    "bench/DESIGN.md": "design\n",
    "bench/mh/__init__.py": "",
    "bench/mh/harness.py": "X = 1\n",
    "bench/run.py": "print('run')\n",
    "bench/compare.py": "print('compare')\n",
    "bench/selftest.py": "print('suite valid: 1 tasks')\n",
    "bench/stress_test.py": "print('1 passed, 0 failed')\n",
    "bench/test_stats.py": "print('ok 1 stats tests')\n",
    "bench/tasks/t1/TASK.md": "Fix it.\n",
    "bench/tasks/t1/hidden_test.py": "import sys\nsys.exit(0)\n",
    "bench/paper/stats-v2.json": CLEAN_STATS,
    "bench/paper/stats-hard.json": json.dumps({"provenance": {
        "x": {"env_client_node": "Mac-2.lan"}}}) + "\n",
    "bench/paper/preregistration.md": "# prereg\n",
    # Excluded, and deliberately dirty: exclusion must mean never read.
    "bench/paper/harness_architecture.md": f"by {AUTHOR}, manvi, bharath\n",
    "bench/paper/build_pdf.sh": f"-V author=\"{AUTHOR}\" email={EMAIL}\n",
    "bench/paper/prepare_body.py": "HASH = 'feedfacecafe'\n",
    "bench/paper/SUBMISSION.md": "gmail plans\n",
    "bench/paper/README.md": "ScholarLM\n",
    "bench/paper/tmlr/tmlr.sty": "% style\n",
    "bench/paper/figures/a.png": b"\x89PNG\r\n\x00\x00binary",
    "bench/live/run.sh": "go build ./cmd/manvi\n",
    "bench/.gitignore": ".work/\n",
    "manvi/go.mod": "module github.com/fixture-owner/manvi\n",
}

_TMP = tempfile.mkdtemp(prefix="supp-test-")


def git(repo, *args):
    return subprocess.run(["git", *args], cwd=repo, env=GIT_ENV, check=True,
                          capture_output=True).stdout.decode().strip()


def write(repo, files):
    for rel, body in files.items():
        p = os.path.join(repo, rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "wb") as f:
            f.write(body if isinstance(body, bytes) else body.encode())


def make_repo(extra=None, drop=()):
    repo = tempfile.mkdtemp(prefix="repo-", dir=_TMP)
    git(repo, "init", "-q", "-b", "main")
    git(repo, "remote", "add", "origin", REMOTE)
    files = {k: v for k, v in BASE.items() if k not in drop}
    files.update(extra or {})
    write(repo, files)
    git(repo, "add", "-A")
    git(repo, "commit", "-q", "-m", "fixture")
    return repo


def commit(repo, files, msg="more"):
    write(repo, files)
    git(repo, "add", "-A")
    git(repo, "commit", "-q", "-m", msg)
    return git(repo, "rev-parse", "HEAD")


def out_dir():
    return tempfile.mkdtemp(prefix="out-", dir=_TMP)


def build(repo, **kw):
    kw.setdefault("rewrites", ())
    kw.setdefault("out_dir", out_dir())
    return B.build(repo, **kw)


def entries(zpath):
    with zipfile.ZipFile(zpath) as z:
        return {i.filename: z.read(i) for i in z.infolist()}


def findings_of(e):
    return "\n".join(getattr(e, "findings", []) or []) + "\n" + str(e)


# --- 1. a clean fixture builds, and only the policy's files ship ------------
print("clean build ships exactly the allowed files")
repo = make_repo()
res = build(repo)
names = set(entries(res.path))
expected = {"supplement/README.md"} | {
    "supplement/" + p for p in BASE
    if p.startswith("bench/") and p not in (
        "bench/paper/harness_architecture.md", "bench/paper/build_pdf.sh",
        "bench/paper/prepare_body.py", "bench/paper/SUBMISSION.md",
        "bench/paper/README.md", "bench/paper/tmlr/tmlr.sty",
        "bench/paper/figures/a.png", "bench/live/run.sh", "bench/.gitignore")}
check("entry set is exactly the included files plus README", names == expected,
      sorted(names ^ expected))
for gone in ("harness_architecture", "build_pdf.sh", "prepare_body.py", "SUBMISSION.md",
             "paper/README.md", "/tmlr/", "/figures/", "/live/", "go.mod", ".gitignore"):
    check(f"excluded path absent: {gone}", not any(gone in n for n in names))
check("README.md is present at the archive root", "supplement/README.md" in names)
readme = entries(res.path)["supplement/README.md"].decode()
check("README tells a reviewer to run the self-tests",
      all(s in readme for s in ("python3 selftest.py", "python3 stress_test.py",
                                "python3 test_stats.py")), readme[:400])
check("README says run logs come after acceptance", "after acceptance" in readme)
check("every excluded file is reported with a reason",
      all(p in res.excluded and res.excluded[p] for p in (
          "bench/paper/build_pdf.sh", "bench/paper/prepare_body.py", "bench/live/run.sh")),
      res.excluded)
check("files outside bench/ are never candidates", "manvi/go.mod" not in res.excluded
      and not any("go.mod" in n for n in names))

# --- 2. determinism ----------------------------------------------------------
print("two builds of one ref are byte-identical")
res2 = build(repo)
h1 = hashlib.sha256(open(res.path, "rb").read()).hexdigest()
h2 = hashlib.sha256(open(res2.path, "rb").read()).hexdigest()
check("same sha256 across builds", h1 == h2 == res.sha256 == res2.sha256, (h1, h2))
os.utime(os.path.join(repo, "bench/mh/harness.py"), (1_900_000_000, 1_900_000_000))
check("working-tree mtimes do not reach the archive",
      hashlib.sha256(open(build(repo).path, "rb").read()).hexdigest() == h1)
with zipfile.ZipFile(res.path) as z:
    infos = z.infolist()
    check("entries are sorted", [i.filename for i in infos] == sorted(i.filename for i in infos))
    check("fixed 1980-01-01 timestamps", all(i.date_time == (1980, 1, 1, 0, 0, 0) for i in infos))
    check("fixed permissions (0644 files)",
          all((i.external_attr >> 16) == 0o100644 for i in infos),
          {i.filename: oct(i.external_attr >> 16) for i in infos})
    check("no extra fields (no uid/gid)", all(i.extra == b"" for i in infos))
    check("no archive comment", z.comment == b"")
    check("unix create_system", all(i.create_system == 3 for i in infos))
repo_x = make_repo({"bench/run.py": "print('run')\n"})
git(repo_x, "update-index", "--chmod=+x", "bench/run.py")
git(repo_x, "commit", "-q", "-m", "exec")
with zipfile.ZipFile(build(repo_x).path) as z:
    check("an executable blob keeps a fixed 0755",
          (z.getinfo("supplement/bench/run.py").external_attr >> 16) == 0o100755)

# --- 3. committed content only, ref configurable ----------------------------
print("the source of truth is the committed tree, not the working tree")
repo_w = make_repo()
write(repo_w, {"bench/mh/harness.py": "X = 'manvi bharath'\n",
               "bench/mh/untracked.py": "Y = 'devmap'\n"})
res_w = build(repo_w)
ent = entries(res_w.path)
check("uncommitted edit does not ship", ent["supplement/bench/mh/harness.py"] == b"X = 1\n")
check("untracked file does not ship", "supplement/bench/mh/untracked.py" not in ent)
first = git(repo_w, "rev-parse", "HEAD")
git(repo_w, "checkout", "-q", "--", ".")
commit(repo_w, {"bench/tasks/t1/TASK.md": "Fix it. See wisdev.\n"})
check("older ref still builds", raises(lambda: build(repo_w, ref=first)) is None)
e = raises(lambda: build(repo_w, ref="HEAD"))
check("HEAD with a planted marker fails", e is not None)
check("the finding names file:line",
      e is not None and "supplement/bench/tasks/t1/TASK.md:1" in findings_of(e),
      e and findings_of(e))
check("a ref that does not resolve fails", raises(lambda: build(repo_w, ref="no-such-ref")) is not None)

# --- 4. identity markers, content ------------------------------------------
print("planted identity markers fail the build")
MARKERS = ["Manvi", "BHARATH", "vaddaram", "Bharathvbcr", "someone@gmail.com",
           "/Users/someone/x", "/home/someone/x", ".devcouncil/", "GitPulse",
           "DevMap", "ScholarLM", "wisdev", "82e453a",
           "https://github.com/fixture-owner/fixture-repo",
           "github.com/fixture-owner", "fixture-owner",
           "Farthingale", "quillon", EMAIL, "quill.farthing"]
for m in MARKERS:
    r = make_repo({"bench/tasks/t1/TASK.md": f"Fix it.\nsee {m} here\n"})
    od = out_dir()
    e = raises(lambda: build(r, out_dir=od))
    check(f"marker {m!r} refused", e is not None and "TASK.md:2" in findings_of(e),
          e and findings_of(e))
    check(f"no archive left behind for {m!r}", os.listdir(od) == [], os.listdir(od))

r = make_repo({"bench/paper/stats-v2.json": '{"note": "\\u006danvi"}\n'})
e = raises(lambda: build(r))
check("a \\u-escaped marker inside JSON is refused", e is not None, "built")
r = make_repo({"bench/paper/stats-v2.json": '{"note": \n'})
check("unparseable JSON is refused", raises(lambda: build(r)) is not None)
r = make_repo({"bench/tasks/t1/TASK.md": "hosted on 203.0.113.7 and 198-51-100-4\n"})
check("a raw IP address is refused", raises(lambda: build(r)) is not None)
r = make_repo({"bench/tasks/t1/TASK.md": "ran on studio-7.lan\n"})
check("a .lan hostname is refused", raises(lambda: build(r)) is not None)
r = make_repo({"bench/tasks/t1/TASK.md": "ollama on http://127.0.0.1:11434\n"})
check("loopback is not an identity", raises(lambda: build(r)) is None)

# --- 5. identity markers, paths ---------------------------------------------
print("a marker in a path fails the build")
for bad in ("bench/tasks/devmap_task/TASK.md", "bench/tasks/t1/Bharath.py"):
    r = make_repo({bad: "clean\n"})
    e = raises(lambda: build(r))
    check(f"path {bad} refused", e is not None and bad.split("/")[-2] in findings_of(e),
          e and findings_of(e))

# --- 6. commit hashes ---------------------------------------------------------
print("hashes that resolve in the repository fail the build")
r = make_repo()
sha = git(r, "rev-parse", "HEAD")
for label, token in (("short", sha[:7]), ("medium", sha[:12]), ("full", sha),
                     ("upper-case", sha[:9].upper())):
    r2 = make_repo()
    sha2 = git(r2, "rev-parse", "HEAD")
    tok = {"short": sha2[:7], "medium": sha2[:12], "full": sha2,
           "upper-case": sha2[:9].upper()}[label]
    commit(r2, {"bench/tasks/t1/TASK.md": f"instrument commit `{tok}`\n"})
    e = raises(lambda: build(r2))
    check(f"{label} hash refused", e is not None and "TASK.md:1" in findings_of(e),
          e and findings_of(e))
r2 = make_repo()
sha2 = git(r2, "rev-parse", "HEAD")
commit(r2, {"bench/tasks/t1/hash_" + sha2[:8] + ".md": "x\n"})
check("a hash in a path is refused", raises(lambda: build(r2)) is not None)
r3 = make_repo({"bench/tasks/t1/TASK.md": "deadbee cafe1234 0123456789 abcdef0\n"})
check("hex that does not resolve is not a finding", raises(lambda: build(r3)) is None)
r4 = make_repo({"bench/tasks/t1/TASK.md": "sha256 " + "ab" * 32 + "\n"})
check("a 64-hex digest is not mistaken for a commit", raises(lambda: build(r4)) is None)

# --- 7. binary and unclassified files ----------------------------------------
print("unknown binaries and unclassified files are refused, not shipped")
r = make_repo({"bench/tasks/t1/data.bin": b"\x00\x01\x02manvi"})
e = raises(lambda: build(r))
check("a NUL-bearing file is refused as binary", e is not None and "binary" in findings_of(e).lower(),
      e and findings_of(e))
r = make_repo({"bench/tasks/t1/latin.txt": b"caf\xe9\n"})
check("non-UTF-8 text is refused", raises(lambda: build(r)) is not None)
r = make_repo({"bench/newdir/tool.py": "x = 1\n"})
e = raises(lambda: build(r))
check("a file no policy rule covers is refused",
      e is not None and "bench/newdir/tool.py" in findings_of(e), e and findings_of(e))
r = make_repo()
os.symlink("TASK.md", os.path.join(r, "bench/tasks/t1/link.md"))
commit(r, {})
e = raises(lambda: build(r))
check("a symlink is refused", e is not None and "link.md" in findings_of(e), e and findings_of(e))
r = make_repo({"bench/tasks/t1/nb.ipynb": json.dumps(
    {"cells": [{"source": ["print('/Users/someone')"]}]})})
check("a notebook's cell source is scanned", raises(lambda: build(r)) is not None)

# --- 8. rewrite rules ---------------------------------------------------------
print("rewrite rules are explicit, counted and re-scanned")
r = make_repo({"bench/mh/harness.py": "# ported from MANVI\nX = 1\n"})
check("without a rule, the product name fails", raises(lambda: build(r)) is not None)
rule = B.Rewrite("bench/mh/harness.py", "MANVI", "a production agent", "product name in a comment")
res_r = build(r, rewrites=(rule,))
body = entries(res_r.path)["supplement/bench/mh/harness.py"]
check("the rule is applied", body == b"# ported from a production agent\nX = 1\n", body)
check("the application is reported with its count",
      any(a.path == "bench/mh/harness.py" and a.count == 1 for a in res_r.rewrites), res_r.rewrites)
readme = entries(res_r.path)["supplement/README.md"].decode()
check("README lists the substitution without the original text",
      "bench/mh/harness.py" in readme and "product name in a comment" in readme
      and "MANVI" not in readme)
stale = B.Rewrite("bench/mh/harness.py", "NOT PRESENT", "x", "stale")
e = raises(lambda: build(r, rewrites=(rule, stale)))
check("a required rule that matches nothing fails", e is not None and "stale" in findings_of(e).lower(),
      e and findings_of(e))
opt = B.Rewrite("bench/mh/harness.py", "NOT PRESENT", "x", "optional", required=False)
check("an optional rule may match nothing", raises(lambda: build(r, rewrites=(rule, opt))) is None)
dirty = B.Rewrite("bench/mh/harness.py", "MANVI", "gitpulse", "bad replacement")
check("a replacement that introduces a marker fails the re-scan",
      raises(lambda: build(r, rewrites=(dirty,))) is not None)
excl = B.Rewrite("bench/paper/build_pdf.sh", "x", "y", "targets an excluded file")
check("a rule aimed at a file that does not ship fails",
      raises(lambda: build(r, rewrites=(rule, excl))) is not None)

print("host names in stats JSON become stable pseudonyms")
ent = entries(res.path)
stats = ent["supplement/bench/paper/stats-v2.json"].decode()
hard = ent["supplement/bench/paper/stats-hard.json"].decode()
check("raw host names are gone", not any(h in stats + hard for h in
                                         ("Mac-1.lan", "Mac-2.lan", "192-0-2-10")), stats)
d = json.loads(stats)["provenance"]
check("distinct hosts stay distinct and equal hosts stay equal",
      d["m|full"]["env_node"] == d["m|base"]["env_node"]
      and d["m|full"]["env_client_node"] != d["m|base"]["env_client_node"], d)
check("one mapping across files",
      json.loads(hard)["provenance"]["x"]["env_client_node"] == d["m|base"]["env_client_node"])
check("the pseudonymisation is reported", res.pseudonyms and all(
    not any(h in v for h in ("Mac-", "192-0")) for v in res.pseudonyms.values()), res.pseudonyms)

# --- 9. size limit --------------------------------------------------------------
print("the size limit is enforced")
od = out_dir()
e = raises(lambda: build(repo, out_dir=od, max_bytes=200))
check("an archive over the limit fails", e is not None and "limit" in findings_of(e).lower(),
      e and findings_of(e))
check("and leaves nothing behind", os.listdir(od) == [], os.listdir(od))
# Letters g-z only: no hex run for the hash detector, and deflate cannot shrink
# ~4.3 bits/char much, so the archive really is larger than the tight limit.
import random  # noqa: E402
_rng = random.Random(7)
big = "".join(_rng.choice("ghijklmnopqrstuvwxyz") for _ in range(300_000))
r = make_repo({"bench/tasks/t1/big.txt": big})
check("a large payload passes under the default limit", raises(lambda: build(r)) is None)
e = raises(lambda: build(r, max_bytes=100_000))
check("and fails under a tight one", e is not None and "limit" in findings_of(e).lower(),
      e and findings_of(e))

# --- 10. CLI exit codes -----------------------------------------------------------
print("the CLI fails closed")
r = make_repo({"bench/tasks/t1/TASK.md": "by quillon\n"})
buf_o, buf_e = io.StringIO(), io.StringIO()
with contextlib.redirect_stdout(buf_o), contextlib.redirect_stderr(buf_e):
    rc = B.main(["--repo", r, "--out-dir", out_dir()], rewrites=())
check("non-zero exit on a finding", rc != 0, rc)
check("the offending file:line is printed", "TASK.md:1" in buf_e.getvalue(), buf_e.getvalue())
buf_o, buf_e = io.StringIO(), io.StringIO()
with contextlib.redirect_stdout(buf_o), contextlib.redirect_stderr(buf_e):
    rc = B.main(["--repo", repo, "--out-dir", out_dir()], rewrites=())
out = buf_o.getvalue()
check("zero exit on a clean build, printing path, entries, size and sha256",
      rc == 0 and all(k in out for k in ("path:", "entries:", "bytes:", "sha256:")), out + buf_e.getvalue())

# --- 11. verification verdicts ----------------------------------------------------
print("verification requires test_stats' exact paper-probe skip, and nothing else")
J = B.judge
N = B.PAPER_SKIPS_EXPECTED
check("pass with exactly the expected paper-probe skip",
      J("test_stats.py", 0, f"SKIP {N} paper probes: prepare_body.py is not shipped\nok 185 stats tests\n")[0])
check("test_stats without the skip line fails (the probes cannot have run)",
      not J("test_stats.py", 0, "a\nok 214 stats tests\n")[0])
check("a different skip count fails",
      not J("test_stats.py", 0, f"SKIP {N - 1} paper probes: x\nok 186 stats tests\n")[0])
check("two skip lines fail",
      not J("test_stats.py", 0, f"SKIP {N} paper probes: x\nSKIP {N} paper probes: x\nok 1 stats tests\n")[0])
check("any other SKIP fails", not J("test_stats.py", 0, "SKIP figures: no\nok 157 stats tests\n")[0])
check("a skip with no count fails", not J("test_stats.py", 0, "SKIP paper probes: x\nok 1 stats tests\n")[0])
check("non-zero exit fails", not J("test_stats.py", 1, "ModuleNotFoundError: prepare_body\n")[0])
check("a zero count fails", not J("stress_test.py", 0, "0 passed, 0 failed\n")[0])
check("no summary line fails", not J("stress_test.py", 0, "")[0])
check("a traceback under exit 0 still fails",
      not J("test_pool.py", 0, "Traceback (most recent call last):\n5 passed, 0 failed\n")[0])
check("a SKIP line in another suite fails",
      not J("test_pool.py", 0, f"SKIP {N} paper probes: x\n5 passed, 0 failed\n")[0])
check("stress summary", J("stress_test.py", 0, "189 passed, 0 failed\n") == (True, "189 passed, 0 failed"))
check("stderr logging does not displace the stdout summary",
      J("test_runtime.py", 0, "197 passed, 0 failed\n", "[grid] cannot read cell x: 3 bad\n")
      == (True, "197 passed, 0 failed"))
check("a traceback on stderr under exit 0 fails",
      not J("test_runtime.py", 0, "197 passed, 0 failed\n", "Traceback (most recent call last):\n")[0])
check("a failing exit reports stderr's last line",
      "ModuleNotFoundError" in J("test_stats.py", 1, "ok x\n", "ModuleNotFoundError: prepare_body\n")[1])
check("extraction outside the repository is required",
      not B.outside(os.path.join(REPO, "bench", "x"), REPO)
      and B.outside(tempfile.gettempdir(), REPO))
zbad = os.path.join(out_dir(), "evil.zip")
with zipfile.ZipFile(zbad, "w") as z:
    z.writestr("../escape.txt", "x")
check("an archive entry escaping the extraction root is refused",
      raises(lambda: B.safe_extract(zbad, out_dir())) is not None)

# --- 12. the real repository at HEAD ---------------------------------------------
print("the real repository at HEAD builds clean (committed content only)")
try:
    real = B.build(REPO, ref="HEAD", out_dir=out_dir())
    real2 = B.build(REPO, ref="HEAD", out_dir=out_dir())
except B.BuildError as e:
    # A newly committed file under bench/ with no POLICY rule lands here too:
    # classify it in build_supplement.py rather than weakening this check.
    check("the real repository at HEAD builds", False, findings_of(e))
    print(f"\n{len(PASS)} passed, {len(FAIL)} failed")
    sys.exit(1)
check("byte-identical rebuild of HEAD", real.sha256 == real2.sha256)
ent = entries(real.path)
names = sorted(ent)
for want in ("supplement/README.md", "supplement/bench/selftest.py",
             "supplement/bench/stress_test.py", "supplement/bench/test_stats.py",
             "supplement/bench/compare.py", "supplement/bench/mh/harness.py",
             "supplement/bench/paper/stats-v2.json", "supplement/bench/paper/preregistration.md"):
    check(f"present: {want}", want in ent)
tasks = {n.split("/")[3] for n in names if n.startswith("supplement/bench/tasks/")}
check("all 19 tasks ship", len(tasks) == 19, sorted(tasks))
for gone in ("harness_architecture", "build_pdf", "prepare_body", "/tmlr/", "/workshop/",
             "/figures/", "SUBMISSION", "paper/README.md", "/live/", "build_supplement",
             "test_supplement", "svg2png", "header.tex", "/results/", ".gitignore",
             "references.bib"):
    check(f"absent: {gone}", not any(gone in n for n in names),
          [n for n in names if gone in n])
# Independent of the builder's scanner: a plain substring sweep of every byte.
blob = b"\n".join([n.encode() for n in names] + list(ent.values())).lower()
for m in (b"manvi", b"bharath", b"vaddaram", b"gmail", b"/users/", b"/home/",
          b"devcouncil", b"gitpulse", b"devmap", b"scholarlm", b"wisdev",
          b"82e453a", b"855acc1", b".lan\"", b"192-222"):
    check(f"independent sweep: no {m.decode()!r}", m not in blob)
check("every default rewrite rule matched something",
      {(a.path, a.rule) for a in real.rewrites} >= {
          (rw.path, rw.reason) for rw in B.DEFAULT_REWRITES if rw.required},
      real.rewrites)

shutil.rmtree(_TMP, ignore_errors=True)
print()
print(f"{len(PASS)} passed, {len(FAIL)} failed")
if FAIL:
    sys.exit(1)
