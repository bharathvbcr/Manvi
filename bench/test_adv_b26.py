"""Adversarial probes against the B26 bench containment residue (branch b26-benchres @ 10782de7).

Every check is named test_adv_b26_* and is written so it PASSES if the change holds; a FAIL
is a break. Nothing here fixes the implementation. Observations the brief says to report
rather than judge print INFO and are not counted.

Probes that touch the operator's host outside the filesystem (setsid daemons, the
notification centres, the keychain, launchd, the pasteboard, a defaults domain) run only
with MH_ADV_HOST_PROBES=1; each tears down in a finally: the clipboard is restored, the
defaults/keychain/launchd/notify names removed, and every process this suite started is
killed by its recorded PID, never by name.

Run from bench/ with Python >= 3.11:  python3.14 test_adv_b26.py
"""
import ctypes
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import mh.tools as toolmod
from mh import keystore, procscan
from mh.bench import load_tasks
from mh.checks import FAIL, PASS, check
from mh.tools import Sandbox, env_file_guards

HERE = os.path.dirname(os.path.abspath(__file__))
DARWIN = sys.platform == "darwin"
CONTAINED = toolmod.containment_backend() in ("sandbox-exec", "bwrap")
if not CONTAINED:
    print("no containment backend: nothing to attack")
    sys.exit(0)

ADV = os.path.realpath(tempfile.mkdtemp(prefix="mh-adv-b26-"))
tempfile.tempdir = ADV
TASKS = {t.name: t for t in load_tasks()}
CRYPTIC = TASKS["cryptic"]
PIDS = []       # explicit PIDs this suite started; killed at the end, never by name
HOST_PROBES = os.environ.get("MH_ADV_HOST_PROBES") == "1"
SECRET_SUFFIXES = keystore.SECRET_SUFFIXES


def host_probe_skipped(what):
    if HOST_PROBES:
        return False
    print(f"  skipped (opt-in) {what}: set MH_ADV_HOST_PROBES=1 to run it")
    return True


def info(name, text):
    print(f"  INFO {name}  {text}")


ONLY = [x for x in os.environ.get("MH_ADV_ONLY", "").split(",") if x]


def guarded(name, fn):
    """Run one probe; an exception is a FAIL, never a silent pass. MH_ADV_ONLY=a,b limits which run."""
    if ONLY and not any(o in name for o in ONLY):
        return
    try:
        fn()
    except Exception as e:  # noqa: BLE001
        check(name + " (probe ran)", False, f"{type(e).__name__}: {e}")


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def kill_recorded():
    for p in PIDS:
        try:
            os.kill(p, signal.SIGKILL)
        except OSError:
            pass


def sandbox_for(task=CRYPTIC):
    d = os.path.realpath(tempfile.mkdtemp(prefix="ws-", dir=ADV))
    s = os.path.join(d, "s")
    task.materialise(s)
    return Sandbox(s, protected_roots=task.guard_roots), s


BASE_ENV = {k: v for k, v in os.environ.items() if not k.upper().endswith(SECRET_SUFFIXES)}


def ks(code, extra_env=None, pass_fds=(), timeout=60):
    """Run code in a fresh interpreter that has imported mh.keystore, with no inherited keys."""
    return subprocess.run(
        [sys.executable, "-I", "-c",
         f"import sys; sys.path.insert(0, {HERE!r})\nfrom mh import keystore\n" + code],
        capture_output=True, text=True, timeout=timeout, pass_fds=pass_fds,
        env={**BASE_ENV, **(extra_env or {})})


def pipe_with(data, hold_write_end=False):
    r, w = os.pipe()
    os.write(w, data)
    if not hold_write_end:
        os.close(w)
        w = None
    os.set_inheritable(r, True)
    return r, w


def procargs_reader():
    path = os.path.join(ADV, "procargs_reader.py")
    if not os.path.exists(path):
        open(path, "w").write(textwrap.dedent('''
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
    return path


def runner(extra="", shell_cmd="'true'", env=None, seal=True, argv_tail=(), timeout=120):
    """A throwaway runner: sealed first, optionally some code, then one episode shell command."""
    script = textwrap.dedent(f'''
        import os, sys, tempfile
        sys.path.insert(0, {HERE!r})
        from mh import keystore
        {"keystore.seal_environment()" if seal else ""}
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
    return subprocess.run([sys.executable, "-I", "-c", script, *argv_tail], capture_output=True, text=True,
                          timeout=timeout, env={**BASE_ENV, **(env or {})})


READ_RUNNER_ENV = '"python3 " + ' + repr(procargs_reader() if DARWIN else "") + ' + " " + str(os.getpid()) + " 2>&1 | tail -c 8000"'

# ---------------------------------------------------------------------------
print("1. key coverage")


def p_coverage():
    import ast
    tree = ast.parse(open(os.path.join(HERE, "mh", "model.py")).read())
    read = set()
    for n in ast.walk(tree):
        if isinstance(n, ast.Call) and getattr(n.func, "id", "") == "_credential":
            read.update(a.value for a in n.args if isinstance(a, ast.Constant))
    check("test_adv_b26_probe_found_the_credential_names_model_reads",
          {"GEMINI_API_KEY", "GOOGLE_API_KEY", "CEREBRAS_API_KEY"} <= read, str(sorted(read)))
    missing = sorted(n for n in read if not keystore.is_secret_name(n))
    check("test_adv_b26_every_credential_model_py_reads_is_a_secret_name", not missing, f"not caught: {missing}")
    r = ks("keystore.seal_environment()\nimport os\nprint('LEFT', sorted(k for k in os.environ if k.endswith('_KEY')), keystore.names())",
           {n: "v-" + n for n in sorted(read)})
    check("test_adv_b26_each_credential_the_bench_reads_leaves_the_environment_and_enters_the_store",
          "LEFT [] " + str(sorted(read)) in r.stdout, (r.stdout + r.stderr)[-300:])
    if not DARWIN:
        return
    # What a shell can actually see: the exec-time environment of the sealed runner.
    envv = {n: "EXEC-SENTINEL-" + n for n in sorted(read)}
    p = runner(shell_cmd=READ_RUNNER_ENV, env=envv)
    check("test_adv_b26_credential_probe_ran", "SYSCTL_RC" in p.stdout, (p.stdout + p.stderr)[:300])
    leaked = [n for n in read if "EXEC-SENTINEL-" + n in p.stdout]
    check("test_adv_b26_no_bench_credential_is_in_the_sealed_runners_exec_environment", not leaked, str(leaked))
    # Proxy credentials: urllib.request.urlopen (every provider call) reads these from the environment.
    proxy = "http://adv-user:PROXY-SENTINEL-PASS@127.0.0.1:9"
    seen = ks("import urllib.request as u; print('PROXIES', u.getproxies())",
              {"HTTPS_PROXY": proxy, "https_proxy": proxy, "ALL_PROXY": proxy})
    check("test_adv_b26_probe_confirms_urllib_reads_the_proxy_the_bench_would_use",
          "PROXY-SENTINEL-PASS" in seen.stdout, (seen.stdout + seen.stderr)[:200])
    p = runner(shell_cmd=READ_RUNNER_ENV, env={"CEREBRAS_API_KEY": "x-key", "HTTPS_PROXY": proxy,
                                                "https_proxy": proxy, "ALL_PROXY": proxy})
    check("test_adv_b26_proxy_url_credentials_do_not_survive_in_the_sealed_runners_exec_environment",
          "PROXY-SENTINEL-PASS" not in p.stdout, "the shell read user:pass from the runner's exec-time HTTPS_PROXY")
    # Names outside the suffix list that still hold secret values.
    for name in ("AWS_SECRET_ACCESS_KEY", "PGPASSWORD", "GOOGLE_APPLICATION_CREDENTIALS_JSON", "OPENAI_ORG_SECRET_ID"):
        info("name_pattern", f"{name}: is_secret_name={keystore.is_secret_name(name)} (the bench does not read it)")
    check("test_adv_b26_aws_secret_access_key_is_a_secret_name", keystore.is_secret_name("AWS_SECRET_ACCESS_KEY"),
          "ends _ACCESS_KEY, not in (_API_KEY _TOKEN _SECRET _SECRET_KEY _PASSWORD); the bench does not read it")
    # argv: a value in the command line survives the re-exec by construction.
    p = runner(shell_cmd=READ_RUNNER_ENV, env={"CEREBRAS_API_KEY": "x-key"}, argv_tail=("ARGV-SENTINEL-VALUE",))
    info("argv_survives_seal", f"a value placed in argv is visible to the shell: {'yes' if 'ARGV-SENTINEL-VALUE' in p.stdout else 'no'}")
    import ast as _ast
    flags = []
    for entry in ("run.py", "grid.py", "probe.py"):
        for n in _ast.walk(_ast.parse(open(os.path.join(HERE, entry)).read())):
            if isinstance(n, _ast.Call) and getattr(n.func, "attr", "") == "add_argument":
                flags += [a.value for a in n.args if isinstance(a, _ast.Constant) and isinstance(a.value, str)]
    check("test_adv_b26_no_entry_point_takes_a_credential_as_a_command_line_flag",
          not [f for f in flags if any(w in f.lower() for w in ("key", "token", "secret", "pass", "cred"))], str(flags))


guarded("test_adv_b26_coverage", p_coverage)

# ---------------------------------------------------------------------------
print("2. the pipe")


def p_pipe_fds():
    if not DARWIN:
        return
    lister = ("import os, stat\nfor fd in range(3, 256):\n    try:\n        m = os.fstat(fd).st_mode\n"
              "    except OSError:\n        continue\n    print('OPEN-FD', fd, 'FIFO' if stat.S_ISFIFO(m) else oct(m))\nprint('END')\n")
    probe = repr("cat > $TMPDIR/fds.py <<'EOF'\n" + lister + "EOF\npython3 $TMPDIR/fds.py 2>&1")
    p = runner(extra="print('RUNNER-ENV-FD', os.environ.get('MH_KEYSTORE_FD'))", shell_cmd=probe, env={"CEREBRAS_API_KEY": "fd-key"})
    check("test_adv_b26_fd_probe_ran", "END" in p.stdout, (p.stdout + p.stderr)[:300])
    check("test_adv_b26_runner_dropped_the_pipe_variable_after_reading", "RUNNER-ENV-FD None" in p.stdout, p.stdout[:200])
    check("test_adv_b26_model_shell_inherits_no_descriptor_beyond_stdio",
          "OPEN-FD" not in p.stdout, p.stdout[:400])


guarded("test_adv_b26_pipe_fds", p_pipe_fds)


def p_malformed():
    code = "try:\n    keystore.seal_environment()\n    print('CONTINUED')\nexcept BaseException as e:\n    print('EXC', type(e).__name__)\n"
    key = {"CEREBRAS_API_KEY": "must-not-fall-back"}
    for label, value in (("unicode_superscript_digit", "\u00b2"), ("empty", ""), ("negative", "-1"), ("padded", " 3"),
                         ("hex", "0x3"), ("exponent", "1e3"), ("plus", "+3"), ("arabic_indic_digit", "\u0663"),
                         ("huge_decimal", "99999999999999999999"), ("two_pow_31", "2147483648"),
                         ("two_pow_32", "4294967296"), ("closed_fd_99", "99"), ("fd_0", "0"), ("fd_2", "2"),
                         ("nul", "3\x00")):
        try:
            r = ks(code, {**key, "MH_KEYSTORE_FD": value})
        except ValueError as e:        # an env value the OS refuses (NUL): not a probe of the code
            info(f"malformed_fd_{label}", f"cannot be set in an environment: {e}")
            continue
        check(f"test_adv_b26_keystore_fd_{label}_fails_closed_with_a_KeystoreError",
              "EXC KeystoreError" in r.stdout, f"{r.stdout.strip()[:80]!r} {r.stderr.strip()[-120:]!r}")
    # Payload edges: invalid UTF-8, JSON null, a non-string value, nesting, exactly at the bound, one over.
    fixed = b'{"A_API_KEY":"' + b"x" * (keystore.MAX_PAYLOAD_BYTES - len(b'{"A_API_KEY":""}')) + b'"}'
    cases = [("invalid_utf8", b'{"A_API_KEY":"\xff\xfe"}', "EXC KeystoreError"),
             ("json_null", b"null", "EXC KeystoreError"),
             ("integer_value", b'{"A_API_KEY":5}', "EXC KeystoreError"),
             ("nested_value", b'{"A_API_KEY":{"x":"y"}}', "EXC KeystoreError"),
             ("empty_payload", b"", "EXC KeystoreError"),
             ("exactly_at_the_bound", fixed, "RET inherited"),
             ("one_byte_over_the_bound", fixed[:-2] + b'x"}', "EXC KeystoreError")]
    assert len(fixed) == keystore.MAX_PAYLOAD_BYTES
    for label, data, want in cases:
        rr, _ = pipe_with(data)
        try:
            r = ks("try:\n    print('RET', keystore.seal_environment())\nexcept BaseException as e:\n    print('EXC', type(e).__name__)\n",
                   {"MH_KEYSTORE_FD": str(rr)}, pass_fds=(rr,))
        finally:
            os.close(rr)
        check(f"test_adv_b26_keystore_payload_{label}", want in r.stdout, f"{r.stdout.strip()[:80]!r} {r.stderr.strip()[-120:]!r}")
    # A hand-off pipe whose writer never closes must not hang the runner forever.
    rr, ww = pipe_with(b'{"A_API_KEY":"k"}', hold_write_end=True)
    try:
        try:
            r = ks("keystore.seal_environment()", {"MH_KEYSTORE_FD": str(rr)}, pass_fds=(rr,), timeout=8)
            hung, detail = False, (r.stdout + r.stderr)[-120:]
        except subprocess.TimeoutExpired:
            hung, detail = True, "seal_environment blocked on os.read for more than 8s"
    finally:
        os.close(rr)
        os.close(ww)
    check("test_adv_b26_keystore_does_not_block_forever_on_a_pipe_whose_writer_stays_open", not hung, detail)
    # Pipe AND a key left in the environment: both end up in the store, nothing stays in the environment.
    rr, _ = pipe_with(b'{"PIPE_API_KEY":"from-pipe"}')
    try:
        r = ks("keystore.seal_environment()\nimport os\nprint('S', keystore.names(), sorted(k for k in os.environ if k.endswith('_KEY')))",
               {"MH_KEYSTORE_FD": str(rr), "ENV_API_KEY": "from-env"}, pass_fds=(rr,))
    finally:
        os.close(rr)
    check("test_adv_b26_keystore_merges_the_pipe_with_a_key_the_parent_left_in_the_environment",
          "S ['ENV_API_KEY', 'PIPE_API_KEY'] []" in r.stdout, (r.stdout + r.stderr)[-300:])
    # Re-exec loop: count process images. A key in the environment costs exactly one re-exec.
    marker = os.path.join(ADV, "images.txt")
    prog = (f"open({marker!r}, 'a').write('image\\n')\n"
            "keystore.seal_environment()\n")
    for label, env, want in (("one_key", {"A_API_KEY": "1"}, 2), ("no_key", {}, 1),
                             ("empty_valued_key", {"A_API_KEY": ""}, 2),
                             ("lowercase_key", {"a_api_key": "1"}, 2),
                             ("many_keys", {f"K{i}_TOKEN": "v" for i in range(40)}, 2)):
        if os.path.exists(marker):
            os.remove(marker)
        try:
            ks(prog, env, timeout=20)
        except subprocess.TimeoutExpired:
            pass
        n = open(marker).read().count("image") if os.path.exists(marker) else 0
        check(f"test_adv_b26_keystore_re_exec_{label}_terminates_after_{want}_image(s)", n == want, f"{n} images")


guarded("test_adv_b26_malformed_pipe", p_malformed)

# ---------------------------------------------------------------------------
print("3. grid -> runner -> shell")


def p_grid_structure():
    import ast
    tree = ast.parse(open(os.path.join(HERE, "grid.py")).read())
    bad = []
    for n in ast.walk(tree):
        if isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute) and getattr(n.func.value, "id", "") == "subprocess" \
                and n.func.attr in ("Popen", "call", "run", "check_call", "check_output"):
            kw = {k.arg for k in n.keywords}
            if "pass_fds" not in kw or "env" not in kw:
                bad.append((n.lineno, n.func.attr, sorted(kw)))
    check("test_adv_b26_every_runner_launch_in_grid_py_hands_off_keys_through_the_pipe", not bad, str(bad))
    left = []
    for n in ast.walk(tree):
        if isinstance(n, ast.Call) and getattr(n.func, "attr", "") in ("system", "popen", "spawnv", "execv", "execve"):
            left.append((n.lineno, n.func.attr))
    check("test_adv_b26_grid_py_starts_no_process_by_a_route_that_bypasses_the_hand_off", not left, str(left))
    env, fds, done = keystore.handoff({"X_API_KEY": "k", "KEEP": "1", "MH_KEYSTORE_FD": "77", "y_token": "t"})
    check("test_adv_b26_handoff_environment_is_clean", "X_API_KEY" not in env and "y_token" not in env and env.get("KEEP") == "1"
          and env.get("MH_KEYSTORE_FD") != "77", str(env))
    keystore._store.clear()
    env, fds, done = keystore.handoff({"X_API_KEY": "k"})
    check("test_adv_b26_handoff_with_no_keys_passes_no_descriptor", fds == () and "MH_KEYSTORE_FD" not in env, str((env, fds)))
    keystore._store["Z_API_KEY"] = "z"
    try:
        env, fds, done = keystore.handoff({})
        a = os.fstat(fds[0])
        done()
        done()          # idempotent close
        try:
            os.fstat(fds[0])
            closed = False
        except OSError:
            closed = True
        check("test_adv_b26_handoff_done_closes_the_parent_copy_and_is_idempotent", closed, "descriptor still open")
        old = keystore._store
        keystore._store = {"BIG_API_KEY": "k" * 9000}
        try:
            keystore.handoff({})
            refused = False
        except keystore.KeystoreError:
            refused = True
        finally:
            keystore._store = old
        check("test_adv_b26_handoff_refuses_an_oversized_store_instead_of_blocking_or_truncating", refused, "no refusal")
    finally:
        keystore._store.clear()


guarded("test_adv_b26_grid_structure", p_grid_structure)

# ---------------------------------------------------------------------------
print("4. procscan")


def own_sleeper(env):
    p = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(90)"], env=env, stdin=subprocess.DEVNULL,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    PIDS.append(p.pid)
    return p


def p_pid_reuse():
    if not DARWIN:
        return
    M = "ab" * 16
    victim = own_sleeper({"PATH": "/usr/bin:/bin"})
    real_marked, real_env = procscan.marked_pids, procscan._exec_environment
    try:
        # (a) the scan hands the killer a pid that never carried the marker (recycled before the kill)
        procscan.marked_pids = lambda marker, exclude=(): [victim.pid]
        left = procscan.reap_marked(M, deadline_s=0.4)
        check("test_adv_b26_reap_does_not_kill_a_pid_that_no_longer_carries_the_marker", victim.poll() is None,
              "an unrelated process was SIGKILLed")
        check("test_adv_b26_reap_reports_the_stale_pid_as_a_survivor_rather_than_a_clean_close", left == [victim.pid], str(left))
        procscan.marked_pids = real_marked
        # (b) forced race inside the real scan: the first read of the victim shows the marker, every later read does not
        calls = {"n": 0}
        needle = f"{procscan.MARKER_VAR}={M}".encode()

        def flip(pid):
            if pid != victim.pid:
                return real_env(pid)
            calls["n"] += 1
            return b"\0" + needle + b"\0" if calls["n"] == 1 else b"\0PATH=/usr/bin\0"
        procscan._exec_environment = flip
        procscan.reap_marked(M, deadline_s=8.0)
        check("test_adv_b26_forced_pid_recycle_between_scan_and_kill_spares_the_new_owner",
              victim.poll() is None and calls["n"] >= 2, f"reads={calls['n']} alive={victim.poll() is None}")
        info("pid_reuse_guard", "reap re-reads the marker before SIGKILL; there is no start-time or pidfd check, so the "
             "window between that read and os.kill is not closed (microseconds)")
    finally:
        procscan.marked_pids, procscan._exec_environment = real_marked, real_env
        victim.kill()


guarded("test_adv_b26_pid_reuse", p_pid_reuse)


def p_bystanders():
    if not DARWIN:
        return
    M, OTHER = "cd" * 16, "ef" * 16
    mine = own_sleeper({"PATH": "/usr/bin:/bin", "MH_EPISODE": M})
    other = own_sleeper({"PATH": "/usr/bin:/bin", "MH_EPISODE": OTHER})
    plain = own_sleeper({"PATH": "/usr/bin:/bin"})
    prefix = own_sleeper({"PATH": "/usr/bin:/bin", "MH_EPISODE": M + "00"})
    suffix = own_sleeper({"PATH": "/usr/bin:/bin", "XMH_EPISODE": M})
    argv_only = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(90)", f"MH_EPISODE={M}"],
                                 env={"PATH": "/usr/bin:/bin"}, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL)
    PIDS.append(argv_only.pid)
    try:
        time.sleep(1.0)
        left = procscan.reap_marked(M, deadline_s=1.0)
        check("test_adv_b26_reap_kills_the_process_that_carries_the_marker", mine.poll() is not None and left == [], f"left={left}")
        check("test_adv_b26_reap_spares_another_episodes_process", other.poll() is None, "killed")
        check("test_adv_b26_reap_spares_a_process_with_no_marker", plain.poll() is None, "killed")
        check("test_adv_b26_reap_spares_a_marker_that_merely_starts_with_ours", prefix.poll() is None, "killed")
        check("test_adv_b26_reap_spares_a_variable_whose_name_merely_ends_with_ours", suffix.poll() is None, "killed")
        check("test_adv_b26_reap_does_not_kill_a_process_that_only_mentions_the_marker_in_argv", argv_only.poll() is None,
              "the needle is matched anywhere in the procargs block, argv included")
        for bad in ("", None):
            try:
                procscan.reap_marked(bad)
                refused = False
            except ValueError:
                refused = True
            check(f"test_adv_b26_reap_refuses_the_marker_{bad!r}", refused, "an empty marker would match every process")
    finally:
        for p in (mine, other, plain, prefix, suffix, argv_only):
            p.kill()


guarded("test_adv_b26_bystanders", p_bystanders)


def p_marker_readable():
    """The marker is not a secret from a concurrent episode: its shell can read it from any same-uid process."""
    if not DARWIN:
        return
    sbA, _ = sandbox_for()
    sbB, _ = sandbox_for()
    reader = textwrap.dedent('''
        import ctypes, re
        libproc = ctypes.CDLL("/usr/lib/libproc.dylib"); libc = ctypes.CDLL(None)
        n = libproc.proc_listallpids(None, 0); buf = (ctypes.c_int * (n + 512))()
        got = libproc.proc_listallpids(buf, ctypes.sizeof(buf))
        seen = set()
        for pid in buf[:got]:
            mib = (ctypes.c_int * 3)(1, 49, pid); size = ctypes.c_size_t(0)
            if libc.sysctl(mib, 3, None, ctypes.byref(size), None, 0) != 0 or not size.value: continue
            b = ctypes.create_string_buffer(size.value)
            if libc.sysctl(mib, 3, b, ctypes.byref(size), None, 0) != 0: continue
            seen.update(re.findall(rb"MH_EPISODE=([0-9a-f]{32})", b.raw[:size.value]))
        for m in sorted(seen): print("MARK", m.decode())
    ''')
    out = {}
    try:
        sbA.run_shell(cmd="cat > $TMPDIR/r.py <<'EOF'\n" + reader + "\nEOF\n")
        sbB.run_shell(cmd="true")
        th = threading.Thread(target=lambda: out.setdefault("B", sbB.run_shell(
            cmd="python3 -c 'import time; time.sleep(8)'")))
        th.start()
        time.sleep(2)
        seen = sbA.run_shell(cmd="python3 $TMPDIR/r.py 2>&1")
        th.join()
        check("test_adv_b26_marker_probe_saw_its_own_marker", sbA.scope.marker in seen, seen[:300])
        check("test_adv_b26_a_concurrent_episodes_shell_cannot_read_this_episodes_marker",
              sbB.scope.marker not in seen, "episode A's shell read episode B's MH_EPISODE from B's running child")
    finally:
        sbA.close()
        sbB.close()


guarded("test_adv_b26_marker_readable", p_marker_readable)


def p_spoof_stalls_reaper():
    """A holder of another episode's marker (readable, above) can keep that episode's reaper from finishing."""
    if not DARWIN or host_probe_skipped("marker-carrying spawner against reap_marked"):
        return
    M = "9a" * 16
    procs, stop = [], threading.Event()

    def spawner():
        end = time.monotonic() + 6
        while time.monotonic() < end and not stop.is_set() and len(procs) < 200:
            p = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"],
                                 env={"PATH": "/usr/bin:/bin", "MH_EPISODE": M}, stdin=subprocess.DEVNULL,
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            procs.append(p)
            PIDS.append(p.pid)
            time.sleep(0.05)
    th = threading.Thread(target=spawner)
    th.start()
    try:
        time.sleep(1.0)
        t0 = time.monotonic()
        left = procscan.reap_marked(M)
        took = time.monotonic() - t0
        check("test_adv_b26_a_stream_of_marker_carrying_processes_cannot_stall_the_reaper_into_reporting_survivors",
              left == [], f"reap_marked returned {len(left)} survivors after {took:.1f}s")
    finally:
        stop.set()
        th.join()
        for p in procs:
            p.kill()
        for p in procs:
            p.wait()


guarded("test_adv_b26_spoof", p_spoof_stalls_reaper)

DAEMON_PY = textwrap.dedent('''
    import os, sys, time
    if os.fork(): os._exit(0)
    os.setsid()
    if os.fork(): os._exit(0)
    open(sys.argv[1], "w").write(str(os.getpid()))
    time.sleep(120)
''')
# setsid'd double fork, then exec of the named program with the marker environment untouched.
EXEC_PY = textwrap.dedent('''
    import os, sys
    if os.fork(): os._exit(0)
    os.setsid()
    if os.fork(): os._exit(0)
    open(sys.argv[1], "w").write(str(os.getpid()))
    os.execv(sys.argv[2], sys.argv[2:])
''')
ENVI_PY = textwrap.dedent('''
    import os, sys
    if os.fork(): os._exit(0)
    os.setsid()
    if os.fork(): os._exit(0)
    os.execve("/bin/sh", ["sh", "-c", "echo $$ > " + sys.argv[1] + "; exec /bin/sleep 120"], {})
''')


def rs(sb, cmd):
    """run_shell that reports a hang instead of aborting the probe (a hung command is data here)."""
    try:
        return sb.run_shell(cmd=cmd)
    except toolmod.ToolError as e:
        return f"TOOLERROR {str(e)[:100]}"


def start_in_shell(sb, name, shell_line):
    pf = os.path.join(sb.root, name)
    rs(sb, shell_line.replace("PIDFILE", pf) + "; sleep 0.7")
    try:
        pid = int(open(pf).read())
    except (OSError, ValueError):
        return 0
    PIDS.append(pid)
    return pid


def p_escapes():
    if not DARWIN or host_probe_skipped("setsid daemon escapes"):
        return
    toolmod.SHELL_TIMEOUT_S = 30
    try:
        sb, _ = sandbox_for()
        for fname, body in (("d.py", DAEMON_PY), ("x.py", EXEC_PY), ("e.py", ENVI_PY)):
            sb.run_shell(cmd=f"cat > $TMPDIR/{fname} <<'EOF'\n{body}\nEOF\n")
        cases = {
            "control_python_setsid_daemon": "python3 $TMPDIR/d.py PIDFILE </dev/null >/dev/null 2>&1",
            "setsid_python_then_exec_bin_sleep": "python3 $TMPDIR/x.py PIDFILE /bin/sleep 120 </dev/null >/dev/null 2>&1",
            "setsid_python_then_exec_usr_bin_nc_listener": "python3 $TMPDIR/x.py PIDFILE /usr/bin/nc -l 127.0.0.1 0 </dev/null >/dev/null 2>&1",
            "setsid_python_then_exec_bin_sh": "python3 $TMPDIR/x.py PIDFILE /bin/sh -c 'sleep 120' </dev/null >/dev/null 2>&1",
            "perl_setsid_daemon": "/usr/bin/perl -e 'use POSIX; exit 0 if fork; POSIX::setsid(); exit 0 if fork; "
                                  "open F, \">PIDFILE\"; print F $$; close F; sleep 120' </dev/null >/dev/null 2>&1",
            "env_i_python_setsid_daemon": "python3 $TMPDIR/e.py PIDFILE </dev/null >/dev/null 2>&1",
            "nohup_background": "nohup /bin/sleep 120 </dev/null >/dev/null 2>&1 & echo $! > PIDFILE",
            "disown_background": "( /bin/sleep 120 </dev/null >/dev/null 2>&1 & echo $! > PIDFILE; disown )",
        }
        started = {}
        for name, line in cases.items():
            started[name] = start_in_shell(sb, name + ".pid", line)
        check("test_adv_b26_escape_probes_started_every_daemon", all(started.values()), str(started))
        sb.close()
        time.sleep(0.5)
        check("test_adv_b26_probe_control_a_marked_python_daemon_is_reaped", not alive(started["control_python_setsid_daemon"]),
              "the control survived: the probes below cannot be read")
        for name in cases:
            if name == "control_python_setsid_daemon":
                continue
            pid = started[name]
            check(f"test_adv_b26_escape_{name}_does_not_survive_the_episode", not (pid and alive(pid)),
                  f"pid {pid} still alive after close(); survivors={sb.survivors}")
    finally:
        kill_recorded()


guarded("test_adv_b26_escapes", p_escapes)


def p_bounds_and_deadline():
    if not DARWIN or host_probe_skipped("process-bound probes"):
        return
    try:
        sb, _ = sandbox_for()
        tree = textwrap.dedent('''
            import os, sys, time, signal
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            def node(d):
                if d:
                    for _ in range(2):
                        if os.fork() == 0:
                            os.setsid(); node(d - 1); os._exit(0)
                time.sleep(100); os._exit(0)
            node(int(sys.argv[1]))
        ''')
        sb.run_shell(cmd="cat > $TMPDIR/tree.py <<'EOF'\n" + tree + "\nEOF\n")
        sb.run_shell(cmd="python3 $TMPDIR/tree.py 8 </dev/null >/dev/null 2>&1 & sleep 3; echo ok")
        time.sleep(1)
        n = len(procscan.marked_pids(sb.scope.marker))
        t0 = time.monotonic()
        sb.close()
        took = time.monotonic() - t0
        check("test_adv_b26_511_sigterm_ignoring_setsid_processes_are_reaped_clean",
              n >= 400 and sb.survivors == [] and not procscan.marked_pids(sb.scope.marker), f"carriers={n} survivors={len(sb.survivors)}")
        check("test_adv_b26_reaping_hundreds_of_processes_stays_inside_the_deadline", took < procscan.REAP_DEADLINE_S + 1, f"{took:.2f}s")
        # SIGSTOPped carrier.
        sb2, _ = sandbox_for()
        sb2.run_shell(cmd="cat > $TMPDIR/d.py <<'EOF'\n" + DAEMON_PY + "\nEOF\n")
        pid = start_in_shell(sb2, "stopped.pid", "python3 $TMPDIR/d.py PIDFILE")
        os.kill(pid, signal.SIGSTOP)
        sb2.close()
        time.sleep(0.4)
        check("test_adv_b26_a_stopped_process_is_reaped", pid and not alive(pid), f"pid {pid}")
        # A scan slower than the deadline (a loaded host: syspolicyd at 450%) must still get one kill pass.
        sb3, _ = sandbox_for()
        sb3.run_shell(cmd="cat > $TMPDIR/d.py <<'EOF'\n" + DAEMON_PY + "\nEOF\n")
        pid3 = start_in_shell(sb3, "slow.pid", "python3 $TMPDIR/d.py PIDFILE")
        real = procscan.marked_pids

        def slow(marker, exclude=()):
            time.sleep(0.6)
            return real(marker, exclude)
        procscan.marked_pids = slow
        try:
            left = procscan.reap_marked(sb3.scope.marker, deadline_s=0.3)
        finally:
            procscan.marked_pids = real
        time.sleep(0.3)
        check("test_adv_b26_a_scan_slower_than_the_deadline_still_kills_what_it_found", pid3 and not alive(pid3),
              f"daemon {pid3} alive; reap_marked returned {left} without sending a single SIGKILL")
        sb3.close()
    finally:
        kill_recorded()


guarded("test_adv_b26_bounds", p_bounds_and_deadline)

# ---------------------------------------------------------------------------
print("5. cross-episode channels the mach-lookup deny leaves open")


def host(cmd, timeout=20):
    r = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=timeout)
    return (r.stdout + r.stderr).strip()


def p_channels():
    toolmod.SHELL_TIMEOUT_S = 25
    if not DARWIN or host_probe_skipped("notifyd / distributed notifications / keychain / launchd / defaults / pasteboard"):
        return
    tag = "advb26" + uuid.uuid4().hex[:8]
    sbA, _ = sandbox_for()
    sbB, _ = sandbox_for()
    saved_clip = subprocess.run(["pbpaste"], capture_output=True).stdout
    listener = None
    try:
        # notifyd: state held by a live process of episode A is readable by a concurrent episode B.
        holder = ("import ctypes, time, sys\nl = ctypes.CDLL(None)\ntok = ctypes.c_int(0)\n"
                  "l.notify_register_check(sys.argv[1].encode(), ctypes.byref(tok))\n"
                  "l.notify_set_state(tok, ctypes.c_uint64(777))\ntime.sleep(8)\n")
        reader = ("import ctypes, sys\nl = ctypes.CDLL(None)\ntok = ctypes.c_int(0)\n"
                  "l.notify_register_check(sys.argv[1].encode(), ctypes.byref(tok))\n"
                  "st = ctypes.c_uint64(0)\nl.notify_get_state(tok, ctypes.byref(st))\nprint('STATE', st.value)\n")
        rs(sbA, "cat > $TMPDIR/h.py <<'EOF'\n" + holder + "\nEOF\n")
        rs(sbB, "cat > $TMPDIR/r.py <<'EOF'\n" + reader + "\nEOF\n")
        th = threading.Thread(target=lambda: rs(sbA, f"python3 $TMPDIR/h.py {tag}.n"))
        th.start()
        time.sleep(3)
        got = rs(sbB, f"python3 $TMPDIR/r.py {tag}.n 2>&1")
        th.join()
        check("test_adv_b26_notifyd_state_does_not_cross_between_concurrent_episodes", "STATE 777" not in got, got[:200])
        # Distributed notifications: a sandboxed poster reaches a listener outside the episode.
        lis = (f'ObjC.import("Foundation"); var got = "none";\n'
               f'$.NSDistributedNotificationCenter.defaultCenter.addObserverForNameObjectQueueUsingBlock("{tag}.d", $(), '
               f'$.NSOperationQueue.mainQueue, function(n) {{ got = ObjC.unwrap(n.userInfo.objectForKey("k")); }});\n'
               f'$.NSRunLoop.currentRunLoop.runUntilDate($.NSDate.dateWithTimeIntervalSinceNow(8)); got;\n')
        post = (f'ObjC.import("Foundation");\n$.NSDistributedNotificationCenter.defaultCenter.'
                f'postNotificationNameObjectUserInfoDeliverImmediately("{tag}.d", $(), $({{k: "DN-SENTINEL"}}), true); "posted";\n')
        lf = os.path.join(ADV, "listen.js")
        open(lf, "w").write(lis)
        listener = subprocess.Popen(["osascript", "-l", "JavaScript", lf], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        PIDS.append(listener.pid)
        time.sleep(2)
        rs(sbA, "cat > $TMPDIR/p.js <<'EOF'\n" + post + "\nEOF\n")
        rs(sbA, "osascript -l JavaScript $TMPDIR/p.js 2>&1")
        heard = listener.communicate(timeout=25)[0]
        check("test_adv_b26_distributed_notification_posted_from_an_episode_does_not_reach_a_listener_outside_it",
              "DN-SENTINEL" not in heard, f"listener heard {heard.strip()!r}")
        # launchctl setenv, with a control: if the unsandboxed call also fails the probe says nothing.
        ctl = host(f"launchctl setenv ADV_{tag}_C 1; launchctl getenv ADV_{tag}_C; launchctl unsetenv ADV_{tag}_C")
        rs(sbA, f"launchctl setenv ADV_{tag} SENT 2>&1")
        seen = host(f"launchctl getenv ADV_{tag}")
        if ctl.strip() != "1":
            info("launchctl_setenv", f"inconclusive: the unsandboxed control cannot set it either ({ctl!r})")
        else:
            check("test_adv_b26_launchctl_setenv_from_an_episode_does_not_reach_the_user_domain", seen != "SENT", seen)
        # NSUserDefaults suites in /Users/Shared and a plain domain.
        rs(sbA, f"defaults write /Users/Shared/{tag} k SENT 2>&1; defaults write {tag}.dom k SENT 2>&1")
        check("test_adv_b26_defaults_written_from_an_episode_are_not_visible_to_the_host",
              "SENT" not in host(f"defaults read /Users/Shared/{tag} k 2>&1; defaults read {tag}.dom k 2>&1")
              and not os.path.exists(f"/Users/Shared/{tag}.plist"), "defaults value or /Users/Shared plist landed")
        # pasteboard through pbcopy and through AppleScript (cfprefsd/pasteboard deny should hold both ways).
        subprocess.run(["pbcopy"], input=b"CLIP-HOST-ORIGINAL", check=False)
        rs(sbA, f"printf CLIP-EPISODE-SENTINEL | pbcopy 2>&1; osascript -e 'set the clipboard to \"CLIP-EPISODE-SENTINEL\"' 2>&1")
        check("test_adv_b26_an_episode_cannot_write_the_clipboard", b"CLIP-EPISODE-SENTINEL" not in subprocess.run(
            ["pbpaste"], capture_output=True).stdout, "clipboard holds the episode's text")
        subprocess.run(["pbcopy"], input=b"CLIP-HOST-ORIGINAL", check=False)
        got = rs(sbA, "pbpaste 2>&1; osascript -e 'the clipboard as text' 2>&1")
        check("test_adv_b26_an_episode_cannot_read_the_clipboard", "CLIP-HOST-ORIGINAL" not in got, got[:200])
        # keychain (default keychain, unique service name), with a control.
        host(f"security add-generic-password -s {tag}c -a adv -w CTL 2>&1", timeout=40)
        ctlk = host(f"security find-generic-password -s {tag}c -w 2>&1", timeout=40)
        host(f"security delete-generic-password -s {tag}c 2>&1", timeout=40)
        kcout = rs(sbA, f"security add-generic-password -s {tag} -a adv -w KC-SENTINEL 2>&1")
        info("keychain_sandboxed_add", kcout[:160].replace("\n", " | "))
        gotk = host(f"security find-generic-password -s {tag} -w 2>&1", timeout=40)
        if ctlk.strip() != "CTL":
            info("keychain", f"inconclusive: the unsandboxed control cannot write the keychain either ({ctlk!r})")
        else:
            check("test_adv_b26_an_episode_cannot_leave_an_item_in_the_login_keychain", "KC-SENTINEL" not in gotk, gotk)
    finally:
        subprocess.run(["pbcopy"], input=saved_clip, check=False)
        host(f"launchctl unsetenv ADV_{tag}; launchctl unsetenv ADV_{tag}_C; defaults delete {tag}.dom 2>&1; "
             f"rm -f /Users/Shared/{tag}.plist /Users/Shared/{tag}; ", timeout=30)
        host(f"security delete-generic-password -s {tag} 2>&1; security delete-generic-password -s {tag}c 2>&1", timeout=40)
        if listener is not None and listener.poll() is None:
            listener.kill()
        sbA.close()
        sbB.close()
        kill_recorded()


guarded("test_adv_b26_channels", p_channels)


def p_launchd_escape():
    if not DARWIN or host_probe_skipped("launchctl submit / LaunchAgents write"):
        return
    label = "adv.b26." + uuid.uuid4().hex[:8]
    agent = os.path.expanduser(f"~/Library/LaunchAgents/{label}.plist")
    sb, _ = sandbox_for()
    pf = os.path.join(sb.root, "lc.pid")
    try:
        sb.run_shell(cmd=f"launchctl submit -l {label} -- /bin/sh -c 'echo $$ > {pf}; exec /bin/sleep 120' 2>&1; sleep 1.5")
        pid = int(open(pf).read()) if os.path.exists(pf) else 0
        if pid:
            PIDS.append(pid)
        sb.run_shell(cmd=f"printf '<plist/>' > '{agent}' 2>&1")
        sb.close()
        time.sleep(0.5)
        check("test_adv_b26_launchctl_submit_from_an_episode_does_not_start_a_job_that_outlives_it", not (pid and alive(pid)),
              f"launchd started pid {pid} outside the sandbox and outside the episode's process tree")
        check("test_adv_b26_an_episode_cannot_write_a_launchagent_plist", not os.path.exists(agent), agent)
        info("at_and_login_items", "at(1) is refused by the sandbox ('Operation not permitted'); a login item needs System Events "
             "automation, not run here because it raises a TCC prompt on the host")
    finally:
        host(f"launchctl remove {label} 2>&1")
        kill_recorded()
        if os.path.exists(agent):
            os.remove(agent)


guarded("test_adv_b26_launchd", p_launchd_escape)

# ---------------------------------------------------------------------------
print("6. workspace refusal")


def p_workspace():
    # BENCH_ROOT alone: a linked worktree's default roots also name the main checkout above it,
    # which would make every path in it 'protected' and say nothing about the layout rule.
    roots = (toolmod.BENCH_ROOT,)
    work = os.path.join(HERE, ".work")
    os.makedirs(work, exist_ok=True)

    def refused(path, label, expect_refused=True):
        try:
            got = toolmod.require_protected_workspace_root(path, roots)
            ok = not expect_refused
            check(f"test_adv_b26_workspace_{label}", ok, f"accepted {path!r} -> {got}")
        except toolmod.WorkspaceLayoutError:
            check(f"test_adv_b26_workspace_{label}", expect_refused, f"refused {path!r}")
    outside = os.path.join(ADV, "outside")
    os.makedirs(outside)
    refused(os.path.join(HERE, ".work", "..", ".."), "dotdot_that_climbs_out_of_the_bench_tree_is_refused")
    refused(os.path.join(HERE, ".work", "x", "..", "..", ".."), "dotdot_chain_above_the_bench_tree_is_refused")
    refused(os.path.join(os.path.dirname(HERE)), "the_repository_root_above_bench_is_refused")
    refused(HERE + "-sibling", "a_sibling_directory_sharing_the_bench_prefix_is_refused")
    refused(HERE + os.sep + ".work" + os.sep + ".." + os.sep + ".work", "dotdot_that_stays_inside_is_accepted", expect_refused=False)
    link = os.path.join(work, "adv-b26-link-out")
    try:
        os.symlink(outside, link)
        refused(os.path.join(link, "w"), "symlink_inside_work_pointing_outside_is_refused")
    finally:
        if os.path.islink(link):
            os.unlink(link)
    chain = os.path.join(ADV, "chain")
    try:
        os.symlink(os.path.join(work), os.path.join(ADV, "hop1"))
        os.symlink(os.path.join(ADV, "hop1"), chain)
        refused(os.path.join(chain, "w"), "symlink_chain_into_work_is_judged_by_its_final_target_and_accepted", expect_refused=False)
    finally:
        for p in (chain, os.path.join(ADV, "hop1")):
            if os.path.islink(p):
                os.unlink(p)
    refused(os.path.join(HERE, ".work") + os.sep + os.sep + "w", "doubled_separators_resolve_normally", expect_refused=False)
    # /var vs /private/var: the same directory under both spellings.
    tmp_real = os.path.realpath(ADV)
    if tmp_real.startswith("/private/var/"):
        spelled = tmp_real[len("/private"):]
        prot = [os.path.join(tmp_real, "prot")]
        a = toolmod.require_protected_workspace_root(os.path.join(spelled, "prot", "w"), prot)
        b = toolmod.require_protected_workspace_root(os.path.join(tmp_real, "prot", "w"), [os.path.join(spelled, "prot")])
        check("test_adv_b26_workspace_var_and_private_var_spellings_are_one_directory", a == b == prot[0], f"{a} {b}")
        os.makedirs(os.path.join(tmp_real, "prot"), exist_ok=True)
        open(os.path.join(tmp_real, "prot", "s.txt"), "w").write("VAR-SECRET")
        for label, rt in (("var_spelled_root", [os.path.join(spelled, "prot")]), ("private_spelled_root", prot)):
            sbx = Sandbox(tempfile.mkdtemp(prefix="ws-", dir=ADV), protected_roots=rt)
            out = sbx.run_shell(cmd=f"cat '{spelled}/prot/s.txt' '{tmp_real}/prot/s.txt' 2>&1")
            sbx.close()
            check(f"test_adv_b26_seatbelt_deny_holds_for_{label}_through_either_spelling", "VAR-SECRET" not in out, out[:200])
    else:
        info("var_vs_private", f"temp dir {tmp_real} is not under /private/var: spelling probe not applicable")
    # Case variants on case-insensitive APFS.
    swapped = HERE.swapcase()
    if os.path.exists(swapped) and os.path.samefile(swapped, HERE):
        refused(os.path.join(swapped, ".work"), "case_variant_spelling_of_the_bench_tree_fails_closed_against_the_real_case_root")
        secret = os.path.join(HERE, "adv_b26_case_secret.txt")
        open(secret, "w").write("CASE-SECRET")
        try:
            for label, rt in (("exact_case", [HERE]), ("wrong_case", [swapped])):
                sbx = Sandbox(tempfile.mkdtemp(prefix="ws-", dir=ADV), protected_roots=rt)
                out = sbx.run_shell(cmd=f"cat '{secret}' 2>&1")
                sbx.close()
                check(f"test_adv_b26_seatbelt_deny_holds_with_a_{label}_protected_root", "CASE-SECRET" not in out, out[:200])
        finally:
            os.remove(secret)
    else:
        info("case_variants", "this volume is case-sensitive: case probes not applicable")
    old = os.getcwd()
    try:
        os.chdir(HERE)
        try:
            toolmod.require_protected_workspace_root("", roots)
            info("empty_workspace_root", "'' resolves to the cwd and is accepted when the cwd is inside bench (callers pass absolute paths)")
        except toolmod.WorkspaceLayoutError:
            info("empty_workspace_root", "'' is refused")
    finally:
        os.chdir(old)


guarded("test_adv_b26_workspace", p_workspace)

# ---------------------------------------------------------------------------
print("7. the capped .env scan")


def scan_repo(prefix, dirs):
    repo = os.path.realpath(tempfile.mkdtemp(prefix=prefix, dir=ADV))
    os.makedirs(os.path.join(repo, ".git"))
    os.makedirs(os.path.join(repo, "bench"))
    for i in range(dirs):
        os.makedirs(os.path.join(repo, f"pkg{i}"))
    return repo


def with_cap(cap, fn):
    saved_cap, saved_cache = toolmod.ENV_GUARD_MAX_DIRS, dict(toolmod._ENV_GUARD_CACHE)
    toolmod.ENV_GUARD_MAX_DIRS = cap
    toolmod._ENV_GUARD_CACHE.clear()
    try:
        return fn()
    finally:
        toolmod.ENV_GUARD_MAX_DIRS = saved_cap
        toolmod._ENV_GUARD_CACHE.clear()
        toolmod._ENV_GUARD_CACHE.update(saved_cache)


def p_cap():
    cap = 6
    # repo + bench + N pkg dirs are visited; .git is pruned. exactly cap -> fine, cap + 1 -> refuse.
    exact = scan_repo("repo-exact-", cap - 2)
    open(os.path.join(exact, f"pkg{cap - 3}", ".env"), "w").write("S")
    over = scan_repo("repo-over-", cap - 1)
    open(os.path.join(over, f"pkg{cap - 2}", ".env"), "w").write("S")
    got = with_cap(cap, lambda: env_file_guards(os.path.join(exact, "bench")))
    check("test_adv_b26_scan_with_exactly_cap_directories_completes_and_finds_the_last_env_file",
          got == (os.path.join(exact, f"pkg{cap - 3}", ".env"),), repr(got))
    try:
        with_cap(cap, lambda: env_file_guards(os.path.join(over, "bench")))
        err = None
    except toolmod.GitGuardError as e:
        err = e
    check("test_adv_b26_scan_with_cap_plus_one_directories_refuses", err is not None, "no refusal at cap+1")
    check("test_adv_b26_cap_refusal_is_a_ContainmentUnavailable_so_run_py_turns_it_into_a_refusal",
          isinstance(err, toolmod.ContainmentUnavailable), type(err).__name__)
    # Pruned trees are not counted and not entered.
    pr = scan_repo("repo-prune-", 0)
    for i in range(30):
        os.makedirs(os.path.join(pr, "node_modules", f"m{i}"))
        os.makedirs(os.path.join(pr, "target", f"t{i}"))
    got = with_cap(cap, lambda: env_file_guards(os.path.join(pr, "bench")))
    check("test_adv_b26_pruned_directories_do_not_count_toward_the_cap", got == (), repr(got))
    # Directory loops through symlinks terminate, and a .env* symlink is guarded without being followed.
    lp = scan_repo("repo-loop-", 2)
    os.symlink(lp, os.path.join(lp, "pkg0", "loop-up"))
    os.symlink(os.path.join(lp, "pkg1"), os.path.join(lp, "pkg0", "loop-abs"))
    os.symlink(lp, os.path.join(lp, "pkg1", ".envloop"))
    os.symlink("/nonexistent/target", os.path.join(lp, "pkg1", ".env.dangling"))
    t0 = time.monotonic()
    got = with_cap(cap, lambda: env_file_guards(os.path.join(lp, "bench")))
    check("test_adv_b26_symlink_directory_loops_terminate_without_hitting_the_cap", time.monotonic() - t0 < 5, f"{time.monotonic() - t0:.1f}s")
    check("test_adv_b26_env_named_symlinks_are_guarded_dangling_and_directory_loops_alike",
          set(got) == {os.path.join(lp, "pkg1", ".envloop"), os.path.join(lp, "pkg1", ".env.dangling")}, repr(got))
    # A loop that hard links cannot make (a bind mount) is not constructible here; the cap is the bound.
    # Nested independent checkouts are not entered: an .env inside one is left readable.
    nest = scan_repo("repo-nest-", 0)
    os.makedirs(os.path.join(nest, "vendor", "tool", ".git"))
    open(os.path.join(nest, "vendor", "tool", ".env"), "w").write("NESTED-SECRET")
    got = with_cap(50, lambda: env_file_guards(os.path.join(nest, "bench")))
    check("test_adv_b26_env_file_inside_a_nested_checkout_is_guarded",
          os.path.join(nest, "vendor", "tool", ".env") in got, f"guards={got!r}; the walk prunes nested checkouts and "
          "git_guard_roots only names registered worktrees")
    # Real directory named .env*: its contents are not .env files by name.
    dn = scan_repo("repo-envdir-", 0)
    os.makedirs(os.path.join(dn, ".envs"))
    open(os.path.join(dn, ".envs", "prod.key"), "w").write("DIR-SECRET")
    got = with_cap(50, lambda: env_file_guards(os.path.join(dn, "bench")))
    check("test_adv_b26_files_inside_a_directory_named_dot_env_something_are_guarded",
          any(p.startswith(os.path.join(dn, ".envs")) for p in got), f"guards={got!r}")
    # Unreadable subdirectory raises, not skips.
    ur = scan_repo("repo-unreadable-", 1)
    os.chmod(os.path.join(ur, "pkg0"), 0)
    try:
        try:
            with_cap(50, lambda: env_file_guards(os.path.join(ur, "bench")))
            raised = False
        except toolmod.GitGuardError:
            raised = True
    finally:
        os.chmod(os.path.join(ur, "pkg0"), 0o700)
    check("test_adv_b26_unreadable_directory_during_the_scan_raises", raised, "scan skipped an unreadable directory")


guarded("test_adv_b26_cap", p_cap)

# ---------------------------------------------------------------------------
kill_recorded()
for root, dirs, files in os.walk(ADV):
    for d in dirs:
        try:
            os.chmod(os.path.join(root, d), 0o700)
        except OSError:
            pass
shutil.rmtree(ADV, ignore_errors=True)
print(f"\n{len(PASS)} passed, {len(FAIL)} failed")
for n in FAIL:
    print("  failed:", n)
sys.exit(1 if FAIL else 0)
