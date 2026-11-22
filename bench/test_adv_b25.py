"""Adversarial probes against the B25 child containment (branch b25-bench @ ea3edab9).

Every check is named test_adv_b25_* and is written so it PASSES if the containment
holds; a FAIL is a break. Nothing here fixes the implementation. Probes that only
observe a behaviour the brief says to report rather than judge print INFO and are
not counted.

Checks named test_open_b25_* would be KNOWN-OPEN (a break found but not yet fixed):
they still run and still FAIL when the break is present, but the suite's exit
status ignores them, and the summary line counts them separately. None is open
now; every former one gates as test_adv_b25_*.

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
from mh.tools import Sandbox, scrubbed_env, env_file_guards, git_guard_roots
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
# Probes that touch the operator's host outside the filesystem (the pasteboard, a
# defaults domain, a setsid daemon on loopback) run only when asked for.
HOST_PROBES = os.environ.get("MH_ADV_HOST_PROBES") == "1"


def host_probe_skipped(what):
    if HOST_PROBES:
        return False
    print(f"  skipped (opt-in) {what}: set MH_ADV_HOST_PROBES=1 to run it")
    return True


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
    if sys.platform != "darwin" or host_probe_skipped("cfprefsd and pasteboard probes"):
        return
    key = f"mh.adv.b25.{os.getpid()}"
    # The operator's pasteboard is saved first (plain text only) and restored in
    # the finally; the defaults key is deleted there too.
    saved = subprocess.run(["/usr/bin/pbpaste"], capture_output=True).stdout
    sbs = []
    try:
        sb1, _ = sandbox_for(CRYPTIC)
        sbs.append(sb1)
        sb1.run_shell(cmd=f"defaults write {key} marker -string DEFAULTS-SENTINEL 2>&1; echo PASTE-SENTINEL | pbcopy 2>&1")
        sb1.close()
        sb2, _ = sandbox_for(CRYPTIC)
        sbs.append(sb2)
        r1 = sb2.run_shell(cmd=f"defaults read {key} marker 2>&1")
        r2 = sb2.run_shell(cmd="pbpaste 2>&1")
        check("test_adv_b25_cfprefsd_is_not_a_cross_episode_channel", "DEFAULTS-SENTINEL" not in r1, r1[:200])
        check("test_adv_b25_pasteboard_is_not_a_cross_episode_channel", "PASTE-SENTINEL" not in r2, r2[:200])
    finally:
        subprocess.run(["/usr/bin/defaults", "delete", key], capture_output=True)
        subprocess.run(["/usr/bin/pbcopy"], input=saved, capture_output=True)
        for s in sbs:
            s.close()


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


def p_env_guard_cap_refuses():
    """A .env* scan that hits its directory cap refuses; it never runs with a partial guard set."""
    repo = os.path.realpath(tempfile.mkdtemp(prefix="repo-cap-", dir=ADV))
    os.makedirs(os.path.join(repo, ".git"))
    bench = os.path.join(repo, "bench")
    os.makedirs(bench)
    for i in range(8):
        os.makedirs(os.path.join(repo, f"pkg{i}"))
    open(os.path.join(repo, "pkg7", ".env"), "w").write("PAST-THE-CAP-SENTINEL")
    saved_cap, saved_cache = toolmod.ENV_GUARD_MAX_DIRS, dict(toolmod._ENV_GUARD_CACHE)
    err, guards = None, None
    try:
        toolmod.ENV_GUARD_MAX_DIRS = 4
        toolmod._ENV_GUARD_CACHE.clear()
        try:
            guards = env_file_guards(bench)
        except toolmod.GitGuardError as e:
            err = e
        check("test_adv_b25_capped_env_guard_scan_refuses_the_episode",
              err is not None, f"no refusal; the scan returned {guards!r} with the cap hit")
        text = str(err)
        check("test_adv_b25_capped_env_guard_refusal_names_the_cap_and_the_directory_count",
              err is not None and "cap of 4 directories" in text and "5 directories" in text, text[:300])
        check("test_adv_b25_capped_scan_is_not_cached_as_a_complete_one",
              not toolmod._ENV_GUARD_CACHE, str(toolmod._ENV_GUARD_CACHE)[:200])
        toolmod.ENV_GUARD_MAX_DIRS = 50
        found = env_file_guards(bench)
        check("test_adv_b25_scan_under_its_cap_still_finds_every_env_file",
              found == (os.path.join(repo, "pkg7", ".env"),), repr(found))
    finally:
        toolmod.ENV_GUARD_MAX_DIRS = saved_cap
        toolmod._ENV_GUARD_CACHE.clear()
        toolmod._ENV_GUARD_CACHE.update(saved_cache)
        shutil.rmtree(repo, ignore_errors=True)


guarded("test_adv_b25_env_guard_cap", p_env_guard_cap_refuses)

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


def procargs_reader_path():
    reader = os.path.join(ADV, "procargs_reader.py")
    open(reader, "w").write(textwrap.dedent('''
        import ctypes, sys
        libc = ctypes.CDLL(None)
        for pid in sys.argv[1:]:
            mib = (ctypes.c_int * 3)(1, 49, int(pid))
            size = ctypes.c_size_t(0)
            libc.sysctl(mib, 3, None, ctypes.byref(size), None, 0)
            buf = ctypes.create_string_buffer(size.value)
            rc = libc.sysctl(mib, 3, buf, ctypes.byref(size), None, 0)
            print("SYSCTL_RC", rc, size.value, "pid", pid)
            print(buf.raw[:size.value].replace(b"\\0", b" ").decode("utf-8", "replace"))
    '''))
    return reader


def sandboxed_shell_script(shell_cmd, seal, extra=""):
    """A throwaway runner: optionally sealed first, then an episode shell running shell_cmd."""
    return textwrap.dedent(f'''
        import os, sys, tempfile
        sys.path.insert(0, {HERE!r})
        {"from mh import keystore; keystore.seal_environment()" if seal else ""}
        tempfile.tempdir = {ADV!r}
        from mh.tools import Sandbox
        from mh.bench import load_tasks
        {extra}
        t = {{x.name: x for x in load_tasks()}}["cryptic"]
        d = tempfile.mkdtemp(prefix="ws-", dir={ADV!r}); s = os.path.join(d, "s"); t.materialise(s)
        sb = Sandbox(s, protected_roots=t.guard_roots)
        print(sb.run_shell(cmd={shell_cmd}))
        sb.close()
    ''')


def run_py(script, env, timeout=90):
    return subprocess.run([sys.executable, "-I", "-c", script], capture_output=True, text=True,
                          timeout=timeout, env=env)


def p_procargs_env():
    """ps is blocked in the sandbox; sysctl(KERN_PROCARGS2) on the parent is not a binary.

    No SBPL rule closes it (a same-uid process can read its parent's exec-time
    environment), so the runner removes its keys from the environment it is
    exec'd with: mh.keystore.seal_environment() re-execs it clean and hands the
    keys over an inherited pipe.
    """
    if sys.platform != "darwin":
        return
    reader = procargs_reader_path()
    read_self = '"python3 " + ' + repr(reader) + ' + " " + str(os.getpid()) + " 2>&1 | tail -c 6000"'
    env = {**os.environ, "CEREBRAS_API_KEY": "PROCARGS-SENTINEL-KEY"}
    # Negative control: the same probe against a runner that did NOT seal reads the key,
    # so a clean result below is the seal's doing and not a probe that cannot see.
    p0 = run_py(sandboxed_shell_script(read_self, seal=False), env)
    check("test_adv_b25_procargs_probe_detects_an_unsealed_runner", "PROCARGS-SENTINEL-KEY" in p0.stdout,
          (p0.stdout + p0.stderr)[:300])
    p = run_py(sandboxed_shell_script(read_self, seal=True), env)
    check("test_adv_b25_procargs_probe_ran", "SYSCTL_RC" in p.stdout, (p.stdout + p.stderr)[:300])
    check("test_adv_b25_model_shell_cannot_read_the_runners_environment_via_sysctl_procargs",
          "PROCARGS-SENTINEL-KEY" not in p.stdout, p.stdout[:400])
    # The runner still has its key: it is in memory, which is what Client() reads.
    p2 = run_py(sandboxed_shell_script("'true'", seal=True,
                                       extra="from mh import model; print('KEY', model._credential('CEREBRAS_API_KEY'), "
                                             "'ENV', os.environ.get('CEREBRAS_API_KEY'))"), env)
    check("test_adv_b25_sealed_runner_still_reads_its_key_from_the_key_store",
          "KEY PROCARGS-SENTINEL-KEY ENV None" in p2.stdout, (p2.stdout + p2.stderr)[:300])
    # Every ancestor of the shell: grid.py (outer) -> run.py (inner) -> shell. The shell reads
    # the exec-time environment of both. The outer hands the key to the inner over keystore.handoff.
    inner = sandboxed_shell_script(
        '"python3 " + ' + repr(reader) + ' + " " + str(os.getpid()) + " " + str(os.getppid()) + " 2>&1 | tail -c 12000"',
        seal=True, extra="from mh import model; print('INNER-KEY', model._credential('CEREBRAS_API_KEY'))")
    outer = textwrap.dedent(f'''
        import os, subprocess, sys
        sys.path.insert(0, {HERE!r})
        from mh import keystore
        keystore.seal_environment()
        env, fds, done = keystore.handoff(dict(os.environ))
        try:
            p = subprocess.run([sys.executable, "-I", "-c", {inner!r}], env=env, pass_fds=fds,
                               capture_output=True, text=True, timeout=80)
        finally:
            done()
        sys.stdout.write(p.stdout); sys.stderr.write(p.stderr)
    ''')
    p3 = run_py(outer, env, timeout=120)
    both = p3.stdout.count("SYSCTL_RC")
    check("test_adv_b25_shell_read_both_ancestors_exec_environments", both == 2, (p3.stdout + p3.stderr)[:400])
    check("test_adv_b25_no_ancestor_of_the_shell_holds_a_key_in_its_exec_environment",
          "PROCARGS-SENTINEL-KEY" not in p3.stdout.replace("INNER-KEY PROCARGS-SENTINEL-KEY", ""), p3.stdout[:600])
    check("test_adv_b25_runner_launched_through_handoff_still_has_its_key",
          "INNER-KEY PROCARGS-SENTINEL-KEY" in p3.stdout, (p3.stdout + p3.stderr)[:300])


guarded("test_adv_b25_procargs_env", p_procargs_env)


def p_keystore():
    """The key hand-off fails closed, is bounded, and sits first in every entry point."""
    import ast
    base_env = {k: v for k, v in os.environ.items() if not k.upper().endswith(keystore_suffixes)}

    def ks(code, extra_env=None, pass_fds=()):
        return subprocess.run([sys.executable, "-I", "-c",
                               f"import sys; sys.path.insert(0, {HERE!r})\nfrom mh import keystore\n" + code],
                              capture_output=True, text=True, timeout=60, pass_fds=pass_fds,
                              env={**base_env, **(extra_env or {})})
    r = ks("print('SEAL', keystore.seal_environment())")
    check("test_adv_b25_keystore_seal_with_nothing_to_hide_returns_without_re_exec",
          "SEAL none" in r.stdout, (r.stdout + r.stderr)[:200])
    r = ks("keystore.seal_environment()", {"MH_KEYSTORE_FD": "abc"})
    check("test_adv_b25_keystore_refuses_a_non_numeric_descriptor", r.returncode != 0 and "KeystoreError" in r.stderr, r.stderr[-200:])
    r = ks("keystore.seal_environment()", {"MH_KEYSTORE_FD": "1"})
    check("test_adv_b25_keystore_refuses_a_standard_stream_as_the_pipe", r.returncode != 0 and "standard stream" in r.stderr, r.stderr[-200:])
    rf = os.open(os.path.join(HERE, "run.py"), os.O_RDONLY)
    os.set_inheritable(rf, True)
    try:
        r = ks("keystore.seal_environment()", {"MH_KEYSTORE_FD": str(rf)}, pass_fds=(rf,))
    finally:
        os.close(rf)
    check("test_adv_b25_keystore_refuses_a_descriptor_that_is_not_a_pipe", r.returncode != 0 and "not a pipe" in r.stderr, r.stderr[-200:])
    for label, data, want in (("not_json", b"nope", "not JSON"), ("not_a_name_value_object", b"[1,2]", "name -> value"),
                              ("oversized", b'{"A_API_KEY":"' + b"x" * 9000 + b'"}', "exceeds its bound")):
        rr, ww = os.pipe()
        os.write(ww, data)
        os.close(ww)
        os.set_inheritable(rr, True)
        try:
            r = ks("keystore.seal_environment()", {"MH_KEYSTORE_FD": str(rr)}, pass_fds=(rr,))
        finally:
            os.close(rr)
        check(f"test_adv_b25_keystore_refuses_a_payload_that_is_{label}", r.returncode != 0 and want in r.stderr, r.stderr[-200:])
    r = ks("keystore.seal_environment()", {"BIG_API_KEY": "k" * 9000})
    check("test_adv_b25_keystore_refuses_keys_over_the_hand_off_bound_rather_than_keep_them_in_the_environment",
          r.returncode != 0 and "hand-off bound" in r.stderr, r.stderr[-200:])
    r = ks("keystore.seal_environment(); import os; print('LEFT', sorted(k for k in os.environ if k.endswith('_TOKEN') or k.endswith('_API_KEY') or k == 'MH_KEYSTORE_FD'), "
           "keystore.names())", {"A_API_KEY": "1", "B_TOKEN": "2", "HOME_NOT_A_KEY": "3"})
    check("test_adv_b25_keystore_moves_every_secret_named_variable_out_of_the_environment",
          "LEFT [] ['A_API_KEY', 'B_TOKEN']" in r.stdout, (r.stdout + r.stderr)[:300])
    r = ks("keystore.seal_environment(); import os, urllib.request as u; print('PX', u.getproxies().get('https'), keystore.names())",
           {"HTTPS_PROXY": "http://user:pw@127.0.0.1:9", "HTTP_PROXY": "http://127.0.0.1:9"})
    check("test_adv_b25_sealed_runner_keeps_a_credentialed_proxy_for_urllib_in_process",
          "PX http://user:pw@127.0.0.1:9 ['HTTPS_PROXY']" in r.stdout, (r.stdout + r.stderr)[-300:])
    # Each entry point seals before it does anything else, or its exec-time environment is exposed.
    for entry in ("run.py", "grid.py"):
        tree = ast.parse(open(os.path.join(HERE, entry)).read())
        main_block = next(n for n in tree.body if isinstance(n, ast.If) and isinstance(n.test, ast.Compare)
                          and getattr(n.test.left, "id", "") == "__name__")
        first = ast.unparse(main_block.body[0])
        check(f"test_adv_b25_{entry.replace('.', '_')}_seals_its_environment_first_in_its_entry_point",
              first == "keystore.seal_environment()", first)


keystore_suffixes = ("_API_KEY", "_TOKEN", "_SECRET", "_SECRET_KEY", "_PASSWORD")
guarded("test_adv_b25_keystore", p_keystore)

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
    sb1.close()
    sb2.close()


guarded("test_adv_b25_concurrent", p_concurrent)


def p_workspace_layout():
    """Concurrent episodes cannot read each other's workspaces in the layout the runner supports.

    Outside the protected tree nothing denies a sibling's workspace (reads are
    open elsewhere), so the runner refuses that layout (see test_runtime) and
    require_protected_workspace_root is its one check. Inside BENCH_ROOT the
    protected root above both workspaces is what closes the cross-read.
    """
    outside = os.path.join(ADV, "open-layout")
    try:
        toolmod.require_protected_workspace_root(outside, CRYPTIC.guard_roots)
        refused = False
    except toolmod.WorkspaceLayoutError:
        refused = True
    check("test_adv_b25_workspace_root_outside_the_protected_tree_is_refused", refused,
          f"{outside} was accepted")
    link = os.path.join(ADV, "link-into-bench")
    os.symlink(os.path.join(HERE, ".work"), link)
    try:
        inside = toolmod.require_protected_workspace_root(os.path.join(link, "w"), CRYPTIC.guard_roots)
    finally:
        os.unlink(link)
    check("test_adv_b25_workspace_root_is_judged_by_its_realpath", inside == os.path.realpath(HERE), inside)
    work = os.path.join(HERE, ".work")
    os.makedirs(work, exist_ok=True)
    parent = os.path.realpath(tempfile.mkdtemp(prefix="adv-b26-", dir=work))
    try:
        toolmod.require_protected_workspace_root(parent, CRYPTIC.guard_roots)
        s1 = os.path.join(parent, "e1")
        s2 = os.path.join(parent, "e2")
        CRYPTIC.materialise(s1)
        CRYPTIC.materialise(s2)
        sb1 = Sandbox(s1, protected_roots=CRYPTIC.guard_roots)
        sb2 = Sandbox(s2, protected_roots=CRYPTIC.guard_roots)
        sb2.run_shell(cmd="echo OTHER-EPISODE-SENTINEL > app/note.txt")
        own = sb1.run_shell(cmd="cat app/invoice.py 2>&1 | head -c 100")
        r = sb1.run_shell(cmd=f"cat '{s2}/app/invoice.py' '{s2}/app/note.txt' 2>&1 | head -c 300")
        check("test_adv_b25_supported_layout_episode_still_reads_its_own_workspace",
              "def " in own or "import" in own, own[:160])
        check("test_adv_b25_supported_layout_episode_cannot_read_the_other_episodes_workspace",
              "def " not in r and "import" not in r and "OTHER-EPISODE-SENTINEL" not in r, r[:200])
        sb1.close()
        sb2.close()
    finally:
        unlock(parent)


guarded("test_adv_b25_workspace_layout", p_workspace_layout)


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
    if host_probe_skipped("setsid daemon probes"):
        return
    try:
        _survivor()
    finally:
        for p in PIDS:
            try:
                os.kill(p, signal.SIGKILL)
            except OSError:
                pass


def _survivor():
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
    check("test_adv_b25_no_process_survives_its_episode", not alive, f"daemon pid {alive_pid} alive={alive}; run_shell said {r[:120]!r}")
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
    check("test_adv_b25_survivor_cannot_carry_verifier_scope_contents_to_a_later_episode",
          "SURVIVOR-HIDDEN-ARGS" not in got, got[:200])
    sbV.close()


guarded("test_adv_b25_survivor", p_survivor)


DAEMON = textwrap.dedent('''
    import os, sys, time
    if os.fork(): os._exit(0)
    os.setsid()
    if os.fork(): os._exit(0)
    open(sys.argv[1], "w").write(str(os.getpid()))
    time.sleep(120)
''')


def _alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def _start_daemon(sb, name):
    """Run a double-forked, setsid'd python daemon in sb's shell; return its pid (or 0)."""
    sb.run_shell(cmd="cat > $TMPDIR/d.py <<'EOF'\n" + DAEMON + "\nEOF\n")
    pidfile = os.path.join(sb.root, name)
    sb.run_shell(cmd=f"python3 $TMPDIR/d.py '{pidfile}' </dev/null >/dev/null 2>&1; sleep 0.5")
    try:
        pid = int(open(pidfile).read())
    except (OSError, ValueError):
        return 0
    PIDS.append(pid)
    return pid


def p_reap():
    """Episode end kills what setsid'd away, only that episode's, and says so when it cannot."""
    try:
        sb1, _ = sandbox_for(CRYPTIC)
        sb2, _ = sandbox_for(CRYPTIC)
        d1, d2 = _start_daemon(sb1, "d1.pid"), _start_daemon(sb2, "d2.pid")
        check("test_adv_b25_reap_probe_started_two_setsid_daemons", bool(d1 and d2 and _alive(d1) and _alive(d2)),
              f"d1={d1} d2={d2}")
        sb1.close()
        time.sleep(0.3)
        check("test_adv_b25_closing_an_episode_kills_its_setsid_daemon", not _alive(d1), f"pid {d1} alive")
        check("test_adv_b25_closing_an_episode_leaves_a_concurrent_episodes_daemon_alone", _alive(d2),
              f"pid {d2} was killed by the other episode's close")
        check("test_adv_b25_clean_close_reports_no_survivors", sb1.survivors == [] and sb1.reap_error is None,
              f"{sb1.survivors} {sb1.reap_error}")
        sb2.close()
        time.sleep(0.3)
        check("test_adv_b25_the_second_episodes_close_kills_its_daemon_too", not _alive(d2), f"pid {d2} alive")
        # A process that cannot be killed within the deadline is REPORTED, never read as a clean close.
        sb3, _ = sandbox_for(CRYPTIC)
        d3 = _start_daemon(sb3, "d3.pid")
        from mh import procscan
        left = procscan.reap_marked(sb3.scope.marker, deadline_s=0.0)
        check("test_adv_b25_a_scan_that_runs_out_of_time_returns_the_survivors", d3 in left, f"d3={d3} left={left}")
        sb3.close()
        time.sleep(0.3)
        check("test_adv_b25_close_after_a_timed_out_scan_still_kills_it", not _alive(d3), f"pid {d3} alive")
        # What the marker cannot see: a hardened system binary hides its environment from the sysctl.
        if sys.platform == "darwin" and os.path.exists("/usr/bin/perl"):
            sb4, _ = sandbox_for(CRYPTIC)
            pf = os.path.join(sb4.root, "perl.pid")
            sb4.run_shell(cmd="/usr/bin/perl -e 'use POSIX; exit 0 if fork; POSIX::setsid(); exit 0 if fork; "
                              f"open F, \">{pf}\"; print F $$; close F; sleep 120' </dev/null >/dev/null 2>&1; sleep 0.5")
            try:
                pp = int(open(pf).read())
                PIDS.append(pp)
            except (OSError, ValueError):
                pp = 0
            sb4.close()
            time.sleep(0.3)
            info("hardened_binary_escape", f"a setsid'd /usr/bin/perl daemon (pid {pp}) is "
                 f"{'STILL ALIVE: its exec-time environment is hidden from KERN_PROCARGS2, so the marker cannot find it' if pp and _alive(pp) else 'gone'}"
                 " (reported, not judged)")
    finally:
        for p in PIDS:
            try:
                os.kill(p, signal.SIGKILL)
            except OSError:
                pass


guarded("test_adv_b25_reap", p_reap)

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
# The summary is the LAST line: verify.sh reads its first number as the count.
for n in OPEN:
    print("  known-open:", n)
print(f"\n{len(PASS)} passed, {len(REAL)} failed, {len(OPEN)} known-open")
sys.exit(1 if REAL else 0)