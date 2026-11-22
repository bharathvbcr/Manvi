"""Adversarial probes against the B25 child containment (branch b25-bench @ ea3edab9).

Every check is named test_adv_b25_* and is written so it PASSES if the containment
holds; a FAIL is a break. Nothing here fixes the implementation. Probes that only
observe a behaviour the brief says to report rather than judge print INFO and are
not counted.

Checks named test_open_b25_* are KNOWN-OPEN (out of this card's scope, filed as
their own cards): they still run and still FAIL when the break is present, but the
suite's exit status ignores them, and the summary line counts them separately.

Run from bench/ with Python >= 3.11:  python3.14 test_adv_b25.py
"""
import glob
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
import unicodedata

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mh.tools as toolmod
from mh.tools import Sandbox, ChildScope, scrubbed_env, env_file_guards, git_guard_roots
from mh.bench import load_tasks
from mh.harness import log_args, ARG_LOG_MAX_CHARS
from mh.checks import FAIL, PASS, check

HERE = os.path.dirname(os.path.abspath(__file__))
CONTAINED = toolmod.containment_backend() in ("sandbox-exec", "bwrap")
if not CONTAINED:
    print("no containment backend: nothing to attack")
    sys.exit(0)

# One private parent for every temp this suite makes, so "what leaked into the host
# temp" is a directory listing and not a guess. tempfile.tempdir is what
# ChildScope's mkdtemp(dir=None) resolves, exactly as TMPDIR would.
ADV = os.path.realpath(tempfile.mkdtemp(prefix="mh-adv-b25-"))
tempfile.tempdir = ADV
TASKS = {t.name: t for t in load_tasks()}
CRYPTIC, ENVBUILD = TASKS["cryptic"], TASKS["envbuild"]
REF = open(os.path.join(CRYPTIC.dir, "reference", "app", "invoice.py")).read()
PIDS = []   # explicit PIDs this suite started, killed at the end (never by name)


def info(name, text):
    print(f"  INFO {name}  {text}")


def guarded(name, fn):
    """Run one probe; an exception is a FAIL, never a silent pass."""
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        check(name + " (probe ran)", False, f"{type(e).__name__}: {e}")


def sandbox_for(task, protected=None):
    d = os.path.realpath(tempfile.mkdtemp(prefix="ws-", dir=ADV))
    s = os.path.join(d, "s")
    task.materialise(s)
    return Sandbox(s, protected_roots=task.guard_roots if protected is None else protected), s


def put_payload(s, payload):
    open(os.path.join(s, "app", "invoice.py"), "w").write(payload + "\n" + REF)


def unlock(path):
    for dp, dns, fns in os.walk(path):
        for n in dns:
            try:
                os.chmod(os.path.join(dp, n), 0o700)
            except OSError:
                pass
    shutil.rmtree(path, ignore_errors=True)


def scopes(prefix):
    # Every scope lives under the runner's private parent (mh-runner-<pid>-*).
    return set(glob.glob(os.path.join(ADV, "mh-runner-*", prefix + "*")))


# ---------------------------------------------------------------------------
print("verifier scope lifetime")


def p_locked_verifier():
    before = scopes("mh-verify-")
    sb, s = sandbox_for(CRYPTIC)
    put_payload(s, textwrap.dedent('''
        import os, tempfile
        d = os.path.join(tempfile.gettempdir(), "locked")
        os.makedirs(d)
        open(os.path.join(d, "secret.txt"), "w").write("LOCKED-SENTINEL")
        os.chmod(d, 0o500)
        print("MH_ADV_PATH", d)
    '''))
    CRYPTIC.verify(s)
    left = scopes("mh-verify-") - before
    out = sb.run_shell(cmd="cat " + " ".join(f"'{p}'/*/locked/secret.txt" for p in left) + " 2>&1") if left else ""
    check("test_adv_b25_verifier_scope_removed_when_worker_leaves_a_dir_without_write_bit",
          not left, f"left behind: {sorted(left)}")
    check("test_adv_b25_next_run_shell_cannot_read_a_locked_verifier_leftover",
          "LOCKED-SENTINEL" not in out, out[:200])
    for p in left:
        unlock(p)
    sb.close()


def p_locked_episode():
    before = scopes("mh-episode-")
    sb, s = sandbox_for(CRYPTIC)
    r = sb.run_shell(cmd='mkdir "$TMPDIR/l" && echo EPISODE-SENTINEL > "$TMPDIR/l/s" && chmod 500 "$TMPDIR/l"; echo "$TMPDIR"')
    tmp = r.strip().splitlines()[-1]
    sb.close()
    left = scopes("mh-episode-") - before
    sb2, _ = sandbox_for(CRYPTIC)
    out = sb2.run_shell(cmd=f"cat '{tmp}/l/s' 2>&1")
    check("test_adv_b25_episode_scope_removed_when_the_model_leaves_a_dir_without_write_bit",
          not left, f"left behind: {sorted(left)}")
    check("test_adv_b25_next_episode_cannot_read_a_locked_episode_leftover",
          "EPISODE-SENTINEL" not in out, out[:200])
    for p in left:
        unlock(p)
    sb2.close()


def p_timeout_verifier():
    before = scopes("mh-verify-")
    sb, s = sandbox_for(CRYPTIC)
    put_payload(s, "import time\ntime.sleep(60)\n")
    old = CRYPTIC.timeout
    CRYPTIC.timeout = 3
    try:
        t0 = time.time()
        CRYPTIC.verify(s)
        took = time.time() - t0
    finally:
        CRYPTIC.timeout = old
    left = scopes("mh-verify-") - before
    check("test_adv_b25_verifier_scope_removed_after_a_timeout", not left, f"left: {sorted(left)} ({took:.1f}s)")
    for p in left:
        unlock(p)
    sb.close()


def p_sigkill_runner():
    """The runner itself is killed mid-verification (OOM killer, ctrl-C storm)."""
    before = scopes("mh-verify-")
    script = textwrap.dedent(f'''
        import os, signal, sys, tempfile, threading
        sys.path.insert(0, {HERE!r})
        tempfile.tempdir = {ADV!r}
        from mh.bench import load_tasks
        t = {{x.name: x for x in load_tasks()}}["cryptic"]
        d = tempfile.mkdtemp(prefix="ws-", dir={ADV!r}); s = os.path.join(d, "s"); t.materialise(s)
        ref = open(os.path.join(t.dir, "reference", "app", "invoice.py")).read()
        open(os.path.join(s, "app", "invoice.py"), "w").write(
            "import os, tempfile, time\\nopen(os.path.join(tempfile.gettempdir(), 'k.txt'), 'w').write('KILLED-SENTINEL')\\ntime.sleep(6)\\n" + ref)
        threading.Timer(2.5, lambda: os.kill(os.getpid(), signal.SIGKILL)).start()
        t.verify(s)
    ''')
    p = subprocess.run([sys.executable, "-I", "-c", script], capture_output=True, text=True, timeout=60)
    left = scopes("mh-verify-") - before
    time.sleep(7)   # let any orphaned worker finish its own sleep
    # The next runner's start reaps the parent of a runner that no longer exists.
    toolmod.reap_stale_scopes([ADV])
    left = scopes("mh-verify-") - before
    check("test_adv_b25_verifier_scope_removed_when_the_runner_is_SIGKILLed",
          not left, f"rc={p.returncode} left: {sorted(left)}")
    for q in left:
        unlock(q)


for n, f in (("locked_verifier", p_locked_verifier), ("locked_episode", p_locked_episode),
             ("timeout_verifier", p_timeout_verifier), ("sigkill", p_sigkill_runner)):
    guarded("test_adv_b25_" + n, f)


# ---------------------------------------------------------------------------
print("the model rewrites TMPDIR/HOME or goes around the shim")


def p_writes():
    sb, s = sandbox_for(CRYPTIC)
    real_home = os.path.expanduser("~" + os.environ.get("USER", ""))
    try:
        real_home = os.path.realpath(__import__("pwd").getpwuid(os.getuid()).pw_dir)
    except Exception:  # noqa: BLE001
        pass
    dtd = subprocess.run(["/usr/bin/getconf", "DARWIN_USER_TEMP_DIR"], capture_output=True, text=True).stdout.strip() \
        if sys.platform == "darwin" else "/tmp"
    targets = {
        "tmp": "/tmp", "private_tmp": "/private/tmp", "var_tmp": "/var/tmp",
        "darwin_user_temp_dir": dtd or "/tmp", "users_shared": "/Users/Shared",
        "real_home_library_caches": os.path.join(real_home, "Library", "Caches"),
        "real_home": real_home, "checkout_root": os.path.dirname(os.path.dirname(HERE)) if False else os.path.dirname(HERE),
        "bench_root": HERE,
    }
    # The host temp the runner itself would use (TMPDIR as the runner sees it).
    targets["runner_tmpdir"] = ADV
    for name, d in targets.items():
        if not os.path.isdir(d):
            continue
        tag = f"mh-adv-write-{name}-{os.getpid()}"
        marker = os.path.join(d, tag)
        cmds = [
            f"export TMPDIR='{d}'; f=$(mktemp -d -t {tag}); touch \"$f/x\"; echo $f",
            f"export TMPDIR='{d}'; f=$(mktemp -t {tag}); echo $f",
            f"/usr/bin/mktemp -d -t {tag}",
            f"TMPDIR='{d}' /usr/bin/mktemp -d -t {tag}",
            f"d=$(/usr/bin/mktemp -d '{d}/{tag}.XXXXXX'); echo $d",
            f"python3 -c \"import tempfile; tempfile.mkdtemp(prefix='{tag}', dir='{d}')\"",
            f"echo x > '{marker}'; ln -s /etc/hosts '{marker}.lnk'; mkdir '{marker}.d'",
        ]
        for c in cmds:
            sb.run_shell(cmd=c)
        landed = glob.glob(os.path.join(d, tag + "*"))
        if name == "darwin_user_temp_dir":
            landed += glob.glob(os.path.join(os.path.realpath(d), tag + "*"))
        check(f"test_adv_b25_write_refused_{name}", not landed, f"landed: {landed}")
        for p in landed:
            unlock(p)
    # Python's tempfile with TMPDIR forced to the host temp.
    r = sb.run_shell(cmd=f"TMPDIR='{ADV}' python3 -c \"import tempfile; print(tempfile.mkdtemp(dir='{ADV}'))\" 2>&1; ls '{ADV}' | rg -c '^tmp' ")
    check("test_adv_b25_python_tempfile_cannot_create_in_runner_tmpdir",
          not [p for p in os.listdir(ADV) if p.startswith("tmp")], r[:200])
    sb.close()


guarded("test_adv_b25_writes", p_writes)


def p_defaults_pasteboard():
    """Non-filesystem channels that outlive an episode: cfprefsd and the pasteboard."""
    if sys.platform != "darwin":
        return
    key = f"mh.adv.b25.{os.getpid()}"
    sb1, _ = sandbox_for(CRYPTIC)
    sb1.run_shell(cmd=f"defaults write {key} marker -string DEFAULTS-SENTINEL 2>&1; echo PASTE-SENTINEL | pbcopy 2>&1")
    sb1.close()
    sb2, _ = sandbox_for(CRYPTIC)
    r1 = sb2.run_shell(cmd=f"defaults read {key} marker 2>&1")
    r2 = sb2.run_shell(cmd="pbpaste 2>&1")
    subprocess.run(["/usr/bin/defaults", "delete", key], capture_output=True)
    check("test_open_b25_cfprefsd_is_not_a_cross_episode_channel", "DEFAULTS-SENTINEL" not in r1, r1[:200])
    check("test_open_b25_pasteboard_is_not_a_cross_episode_channel", "PASTE-SENTINEL" not in r2, r2[:200])
    sb2.close()


guarded("test_adv_b25_defaults_pasteboard", p_defaults_pasteboard)

# ---------------------------------------------------------------------------
print("symlinks and hard links")


def p_links():
    base = os.path.realpath(tempfile.mkdtemp(prefix="guard-", dir=ADV))
    gdir = os.path.join(base, "guarded")
    os.makedirs(gdir)
    gfile = os.path.join(gdir, "hidden_test.py")
    open(gfile, "w").write("HIDDEN-SENTINEL")
    envf = os.path.join(base, ".env.local")
    open(envf, "w").write("KEY-SENTINEL")
    ws = os.path.join(base, "ws")
    os.makedirs(ws)
    sb = Sandbox(ws, protected_roots=(gdir, envf))
    r = sb.run_shell(cmd=f"ln -s '{gfile}' ws_sym; cat ws_sym 2>&1; ln -s '{envf}' env_sym; cat env_sym 2>&1")
    check("test_adv_b25_symlink_to_guarded_file_is_not_readable", "SENTINEL" not in r, r[:200])
    r = sb.run_shell(cmd=f"python3 -c \"import os; os.link('{gfile}', os.path.join(os.environ['TMPDIR'], 'hl')); "
                         f"os.link('{envf}', 'env_hl')\" 2>&1; cat \"$TMPDIR/hl\" env_hl 2>&1")
    check("test_adv_b25_hard_link_to_guarded_file_is_not_readable", "SENTINEL" not in r, r[:300])
    r = sb.run_shell(cmd=f"echo POISON > ws_w; ln -sf '{gfile}' ws_w 2>&1; echo POISON2 > ws_w 2>&1; cat '{gfile}' 2>&1 | head -c 0")
    check("test_adv_b25_symlink_write_through_to_guarded_file_is_refused",
          open(gfile).read() == "HIDDEN-SENTINEL", open(gfile).read())
    # hard link made BEFORE the guard existed (a tracked file hard-linked into the workspace)
    pre = os.path.join(ws, "pre_link")
    os.link(gfile, pre)
    r = sb.run_shell(cmd="cat pre_link 2>&1")
    info("pre_link", "a hard link to a guarded file that already sits in the workspace is readable "
         f"({'READABLE' if 'HIDDEN-SENTINEL' in r else 'refused'}); the harness materialises copies, so this needs a prior link")
    sb.close()
    shutil.rmtree(base, ignore_errors=True)


guarded("test_adv_b25_links", p_links)


def p_env_guard_shapes():
    repo = os.path.realpath(tempfile.mkdtemp(prefix="repo-", dir=ADV))
    os.makedirs(os.path.join(repo, ".git"))
    bench = os.path.join(repo, "bench")
    pkg = os.path.join(repo, "pkg")
    os.makedirs(bench)
    os.makedirs(pkg)
    open(os.path.join(pkg, ".env"), "w").write("SIBLING-DIR-SENTINEL")
    store = os.path.join(repo, "store_secret")
    open(store, "w").write("SYMLINK-ENV-SENTINEL")
    os.symlink(store, os.path.join(repo, ".env.local"))
    open(os.path.join(repo, ".npmrc"), "w").write("//registry:_authToken=NPMRC-SENTINEL")
    open(os.path.join(repo, "secrets.env"), "w").write("SECRETS-ENV-SENTINEL")
    guards = (bench,) + git_guard_roots(bench) + env_file_guards(bench)
    ws = os.path.join(bench, "ws")
    os.makedirs(ws)
    sb = Sandbox(ws, protected_roots=guards)
    r1 = sb.run_shell(cmd=f"cat '{pkg}/.env' 2>&1")
    r2 = sb.run_shell(cmd=f"cat '{repo}/.env.local' 2>&1")
    check("test_adv_b25_env_file_in_a_sibling_subdirectory_of_the_checkout_is_guarded",
          "SENTINEL" not in r1, r1[:200] + f" guards={env_file_guards(bench)}")
    check("test_adv_b25_env_file_that_is_a_symlink_is_guarded",
          "SENTINEL" not in r2, r2[:200] + f" guards={env_file_guards(bench)}")
    r3 = sb.run_shell(cmd=f"cat '{repo}/.npmrc' '{repo}/secrets.env' 2>&1")
    info("secret_names", "only names starting .env are guarded; .npmrc/secrets.env are "
         f"{'READABLE' if 'SENTINEL' in r3 else 'refused'}")
    sb.close()
    shutil.rmtree(repo, ignore_errors=True)


guarded("test_adv_b25_env_guard_shapes", p_env_guard_shapes)

# ---------------------------------------------------------------------------
print("environment leakage")


def p_env():
    leak = {"LC_SECRET": "LCSECRET-SENTINEL", "LC_ALL_KEY": "LCALLKEY-SENTINEL",
            "GOOGLE_APPLICATION_CREDENTIALS": "GAC-SENTINEL", "AWS_SECRET_ACCESS_KEY": "AWS-SENTINEL",
            "AWS_ACCESS_KEY_ID": "AWSID-SENTINEL", "GH_TOKEN": "GH-SENTINEL", "GITHUB_TOKEN": "GHT-SENTINEL",
            "NPM_TOKEN": "NPM-SENTINEL", "HF_TOKEN": "HF-SENTINEL", "SSH_AUTH_SOCK": "/tmp/ssh-sentinel"}
    env = {"PATH": "/usr/bin:/bin", **leak}
    out = scrubbed_env("/x", environ=env)
    for k, v in leak.items():
        check(f"test_adv_b25_scrubbed_env_drops_{k}", v not in out.values(), str({k: out.get(k)}))
    saved = {k: os.environ.get(k) for k in leak}
    os.environ.update(leak)
    try:
        sb, _ = sandbox_for(CRYPTIC)
        r = sb.run_shell(cmd="env")
        check("test_adv_b25_run_shell_env_carries_no_sentinel_through_an_allowed_prefix",
              "SENTINEL" not in r and "ssh-sentinel" not in r, "\n".join(l for l in r.splitlines() if "SENTINEL" in l))
        sb.close()
    finally:
        for k, v in saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
    info("smuggled_values", "TERM/LANG/TZ/USER/SHELL/PATH and LC_* values are inherited verbatim: "
         f"TERM={'x'*3}... any value the runner holds there reaches the child")


guarded("test_adv_b25_env", p_env)


def p_ps_env():
    """The runner's own environment is the source the allow-list protects; ps -E reads it back."""
    script = textwrap.dedent(f'''
        import os, sys, tempfile
        sys.path.insert(0, {HERE!r})
        tempfile.tempdir = {ADV!r}
        from mh.tools import Sandbox
        from mh.bench import load_tasks
        t = {{x.name: x for x in load_tasks()}}["cryptic"]
        d = tempfile.mkdtemp(prefix="ws-", dir={ADV!r}); s = os.path.join(d, "s"); t.materialise(s)
        sb = Sandbox(s, protected_roots=t.guard_roots)
        print(sb.run_shell(cmd="ps -Eww -p " + str(os.getpid()) + " 2>&1; ps -Eww -o command -p $$ 2>&1"))
        sb.close()
    ''')
    env = {**os.environ, "CEREBRAS_API_KEY": "PS-SENTINEL-KEY"}
    p = subprocess.run([sys.executable, "-I", "-c", script], capture_output=True, text=True, timeout=60, env=env)
    check("test_adv_b25_model_shell_cannot_read_the_runners_environment_via_ps",
          "PS-SENTINEL-KEY" not in p.stdout, (p.stdout + p.stderr)[:300])


guarded("test_adv_b25_ps_env", p_ps_env)


def p_procargs_env():
    """ps is blocked in the sandbox; sysctl(KERN_PROCARGS2) on the parent is not a binary."""
    if sys.platform != "darwin":
        return
    reader = os.path.join(ADV, "procargs_reader.py")
    open(reader, "w").write(textwrap.dedent('''
        import ctypes, sys
        libc = ctypes.CDLL(None)
        mib = (ctypes.c_int * 3)(1, 49, int(sys.argv[1]))
        size = ctypes.c_size_t(0)
        libc.sysctl(mib, 3, None, ctypes.byref(size), None, 0)
        buf = ctypes.create_string_buffer(size.value)
        rc = libc.sysctl(mib, 3, buf, ctypes.byref(size), None, 0)
        print("SYSCTL_RC", rc, size.value)
        print(buf.raw[:size.value].replace(b"\\0", b" ").decode("utf-8", "replace"))
    '''))
    script = textwrap.dedent(f'''
        import os, sys, tempfile
        sys.path.insert(0, {HERE!r})
        tempfile.tempdir = {ADV!r}
        from mh.tools import Sandbox
        from mh.bench import load_tasks
        t = {{x.name: x for x in load_tasks()}}["cryptic"]
        d = tempfile.mkdtemp(prefix="ws-", dir={ADV!r}); s = os.path.join(d, "s"); t.materialise(s)
        sb = Sandbox(s, protected_roots=t.guard_roots)
        print(sb.run_shell(cmd="python3 " + {reader!r} + " " + str(os.getpid()) + " 2>&1 | tail -c 6000"))
        sb.close()
    ''')
    env = {**os.environ, "CEREBRAS_API_KEY": "PROCARGS-SENTINEL-KEY"}
    p = subprocess.run([sys.executable, "-I", "-c", script], capture_output=True, text=True, timeout=60, env=env)
    check("test_adv_b25_procargs_probe_ran", "SYSCTL_RC" in p.stdout, (p.stdout + p.stderr)[:300])
    check("test_open_b25_model_shell_cannot_read_the_runners_environment_via_sysctl_procargs",
          "PROCARGS-SENTINEL-KEY" not in p.stdout, p.stdout[:400])


guarded("test_adv_b25_procargs_env", p_procargs_env)

# ---------------------------------------------------------------------------
print("odd and hostile paths")


def odd_case(label, name, parent=None):
    parent = parent or ADV
    root = os.path.join(parent, name)
    os.makedirs(root)
    canary = os.path.join(ADV, f"canary-{label}")
    sb = Sandbox(root, protected_roots=())
    r = sb.run_shell(cmd=f"echo inside > ok.txt; echo outside > '{canary}' 2>&1; ls")
    check(f"test_adv_b25_odd_path_{label}_fails_closed", not os.path.exists(canary),
          f"outside write landed; {r[:200]}")
    check(f"test_adv_b25_odd_path_{label}_still_usable", os.path.exists(os.path.join(root, "ok.txt")), r[:200])
    sb.close()
    if os.path.exists(canary):
        os.remove(canary)


def p_odd():
    inj = 'inj") (allow file-write* (subpath "/")) ("'
    for label, name in (("space", "sp ace"), ("quote", 'q"uote'), ("backslash", "back\\\\slash"),
                        ("trailing_backslash", "tb\\\\"), ("unicode", "uni é 日本 🙂"),
                        ("injection", inj), ("newline", "new\nline"), ("parens", "a)(b"),
                        ("semicolon", "a;b#c"), ("injection_nl", 'x")\n(allow file-write* (subpath "/"))\n("')):
        guarded(f"test_adv_b25_odd_{label}", lambda l=label, n=name: odd_case(l, n))
    nfd = unicodedata.normalize("NFD", "é-nfd")
    guarded("test_adv_b25_odd_nfd", lambda: odd_case("nfd", nfd))
    # an odd TEMP PARENT: the scope root, the shim dir and .bash_profile are all built from it
    for label, name in (("tmpparent_quote", 'tp"q'), ("tmpparent_space", "tp s"),
                        ("tmpparent_colon", "tp:colon"), ("tmpparent_inj", 'tp") (allow file-write* (subpath "/")) ("')):
        def run(l=label, n=name):
            par = os.path.join(ADV, n)
            os.makedirs(par)
            old = tempfile.tempdir
            tempfile.tempdir = par
            try:
                canary = os.path.join(ADV, f"canary-{l}")
                d = os.path.join(par, "w")
                os.makedirs(d)
                sb = Sandbox(d, protected_roots=())
                r = sb.run_shell(cmd=f"echo outside > '{canary}' 2>&1; d=$(mktemp -d) && echo ok-$(dirname $d | xargs basename)")
                check(f"test_adv_b25_{l}_fails_closed", not os.path.exists(canary), r[:200])
                check(f"test_adv_b25_{l}_mktemp_shim_works", "ok-" in r, r[:200])
                sb.close()
            finally:
                tempfile.tempdir = old
        guarded(f"test_adv_b25_{label}", run)

    def long_path():
        par = ADV
        for _ in range(23):
            par = os.path.join(par, "d" * 40)
        os.makedirs(par)
        canary = os.path.join(ADV, "canary-long")
        sb = Sandbox(par, protected_roots=())
        r = sb.run_shell(cmd=f"echo inside > ok.txt; echo outside > '{canary}' 2>&1")
        check("test_adv_b25_long_path_fails_closed", not os.path.exists(canary), r[:200])
        sb.close()
        shutil.rmtree(os.path.join(ADV, "d" * 40), ignore_errors=True)
    guarded("test_adv_b25_long", long_path)


p_odd()

# ---------------------------------------------------------------------------
print("concurrency")


def p_concurrent():
    sb1, s1 = sandbox_for(CRYPTIC)
    sb2, s2 = sandbox_for(CRYPTIC)
    sh2 = sb2.scope.private_dir("shell")
    sb2.run_shell(cmd='echo A-SOLUTION > "$TMPDIR/note"')
    sb1.run_shell(cmd="true")
    jobs = {"victim_workspace": os.path.join(s2, "app", "pwn"), "victim_shell_tmp": os.path.join(sh2, "pwn"),
            "victim_scope_root": os.path.join(sb2.scope.root, "pwn"),
            "victim_bin_shim_dir": os.path.join(sb2.scope.root, "bin", "mktemp")}
    res = {}

    def attack(k, p):
        res[k] = sb1.run_shell(cmd=f"echo pwned > '{p}' 2>&1; echo pwned >> '{p}' 2>&1")
    th = [threading.Thread(target=attack, args=a) for a in jobs.items()]
    [t.start() for t in th]
    [t.join() for t in th]
    for k, p in jobs.items():
        check(f"test_adv_b25_parallel_episode_cannot_write_{k}",
              not (os.path.exists(p) and "pwned" in open(p, errors="replace").read()), res.get(k, "")[:120])
    r = sb1.run_shell(cmd=f"cat '{sh2}/note' 2>&1")
    check("test_adv_b25_parallel_episode_cannot_read_the_other_episodes_shell_temp",
          "A-SOLUTION" not in r, r[:160])
    r = sb1.run_shell(cmd=f"cat '{s2}/app/invoice.py' 2>&1 | head -c 200")
    check("test_open_b25_parallel_episode_cannot_read_the_other_episodes_workspace",
          "def " not in r and "import" not in r, r[:160])
    sb1.close()
    sb2.close()


guarded("test_adv_b25_concurrent", p_concurrent)


def p_concurrent_verify_window():
    """Episode B polls the host temp while episode A's verifier worker is alive."""
    sbA, sA = sandbox_for(CRYPTIC)
    sbB, _ = sandbox_for(CRYPTIC)
    put_payload(sA, textwrap.dedent('''
        import os, tempfile, time
        open(os.path.join(tempfile.gettempdir(), "args.txt"), "w").write("INFLIGHT-HIDDEN-ARGS")
        time.sleep(4)
    '''))
    out = {}

    def verify():
        out["v"] = CRYPTIC.verify(sA)
    t = threading.Thread(target=verify)
    t.start()
    time.sleep(2)
    r = sbB.run_shell(cmd=f"cat '{ADV}'/mh-runner-*/mh-verify-*/*/args.txt 2>&1")
    t.join()
    check("test_adv_b25_parallel_episode_cannot_read_an_in_flight_verifier_scope",
          "INFLIGHT-HIDDEN-ARGS" not in r, r[:200])
    sbA.close()
    sbB.close()


guarded("test_adv_b25_concurrent_verify_window", p_concurrent_verify_window)


def p_survivor():
    """A process that setsid()s out of the group outlives its episode; reads are open."""
    sb1, s1 = sandbox_for(CRYPTIC)
    daemon = textwrap.dedent(f'''
        import os, socket, sys, time, glob
        if os.fork(): os._exit(0)
        os.setsid()
        if os.fork(): os._exit(0)
        seen = {{}}
        srv = socket.socket(); srv.bind(("127.0.0.1", 0)); srv.listen(5); srv.settimeout(0.05)
        print("PORT", srv.getsockname()[1], "PID", os.getpid(), flush=True)
        sys.stdout.flush()
        end = time.time() + 40
        while time.time() < end:
            for p in glob.glob({ADV!r} + "/mh-runner-*/mh-verify-*/*/*"):
                try: seen[p] = open(p, errors="replace").read()
                except OSError: pass
            try:
                c, _ = srv.accept()
            except OSError:
                continue
            c.sendall(repr(seen).encode()); c.close()
    ''')
    sb1.run_shell(cmd="cat > $TMPDIR/d.py <<'EOF'\n" + daemon + "\nEOF\n")
    r = sb1.run_shell(cmd="python3 $TMPDIR/d.py < /dev/null 2>&1 | head -1")
    import re
    mm = re.search(r"PORT (\d+) PID (\d+)", r)
    check("test_adv_b25_survivor_probe_started_a_daemon", bool(mm), r[:200])
    m = ["", mm.group(1)] if mm else ["", "0"]
    alive_pid = int(mm.group(2)) if mm else 0
    if alive_pid:
        PIDS.append(alive_pid)
    sb1.close()
    alive = False
    if alive_pid:
        try:
            os.kill(alive_pid, 0)
            alive = True
        except OSError:
            alive = False
    check("test_open_b25_no_process_survives_its_episode", not alive, f"daemon pid {alive_pid} alive={alive}; run_shell said {r[:120]!r}")
    # End to end: the survivor copies the next verification's scope and episode two reads it back.
    sbV, sV = sandbox_for(CRYPTIC)
    put_payload(sV, 'import os, tempfile, time\nopen(os.path.join(tempfile.gettempdir(), "args.txt"), "w").write("SURVIVOR-HIDDEN-ARGS")\ntime.sleep(1)\n')
    CRYPTIC.verify(sV)
    got = ""
    if alive_pid and alive:
        port = int(m[1])
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=3) as c:
                got = c.recv(1 << 20).decode()
        except OSError as e:
            got = f"connect failed: {e}"
    check("test_open_b25_survivor_cannot_carry_verifier_scope_contents_to_a_later_episode",
          "SURVIVOR-HIDDEN-ARGS" not in got, got[:200])
    sbV.close()
    for p in PIDS:
        try:
            os.kill(p, signal.SIGKILL)
        except OSError:
            pass


guarded("test_adv_b25_survivor", p_survivor)

# ---------------------------------------------------------------------------
print("logging")


def p_logging():
    big = "A" * (10 * 1024 * 1024)
    t0 = time.time()
    logged, meta = log_args({"content": big, "path": "x"})
    check("test_adv_b25_10MB_argument_is_capped_with_true_length_recorded",
          len(logged["content"]) <= ARG_LOG_MAX_CHARS and meta["content"]["chars"] == len(big), str(meta)[:100])
    check("test_adv_b25_10MB_argument_logs_quickly", time.time() - t0 < 2, f"{time.time() - t0:.1f}s")
    many = {f"k{i}": "B" * ARG_LOG_MAX_CHARS for i in range(100)}
    lg, _ = log_args(many)
    total = sum(len(v) for v in lg.values())
    check("test_adv_b25_total_logged_per_call_is_bounded_whatever_the_number_of_arguments",
          total <= 4 * ARG_LOG_MAX_CHARS, f"{total:,} chars logged for one tool call")
    emoji = {"content": "\U0001F642" * (ARG_LOG_MAX_CHARS * 2)}
    lg, _ = log_args(emoji)
    check("test_adv_b25_cap_is_in_bytes_not_characters",
          len(lg["content"].encode("utf-8")) <= ARG_LOG_MAX_CHARS, f"{len(lg['content'].encode('utf-8')):,} bytes kept")
    bad = {"content": "ok\udcff\udcfe" + "\x00" * 5}
    lg, meta = log_args(bad)
    # runtime.py writes results with json.dump (ensure_ascii default), so that is the writer to mimic.
    try:
        json.loads(json.dumps({"args": lg, "meta": meta}))
        ok, why = True, ""
    except (UnicodeEncodeError, ValueError) as e:
        ok, why = False, str(e)
    check("test_adv_b25_invalid_unicode_argument_logs_as_json", ok, why)
    sec = "ANTHROPIC_API_KEY=sk-ant-api03-ADVSENTINEL"
    lg, _ = log_args({"cmd": f"export {sec}", "content": sec})
    info("secret_in_args", f"a secret-looking argument is logged verbatim: {'yes' if sec in lg['cmd'] else 'no'} (reported, not judged)")


guarded("test_adv_b25_logging", p_logging)

shutil.rmtree(ADV, ignore_errors=True)
OPEN = [n for n in FAIL if n.startswith("test_open_b25_")]
REAL = [n for n in FAIL if n not in OPEN]
print(f"\n{len(PASS)} passed, {len(REAL)} failed, {len(OPEN)} known-open")
for n in OPEN:
    print("  known-open:", n)
sys.exit(1 if REAL else 0)