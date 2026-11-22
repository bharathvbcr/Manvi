"""Find and kill every process that carries an episode's environment marker.

A shell command that double-forks and setsid()s leaves the process group, the
session and the parent chain of the episode, so killing the group (which
run_bounded_capture does) cannot reach it, and on macOS there is no PID
namespace to take it down with the episode. What such a process cannot shed is
the environment it was exec'd with: every child of the episode's shell inherits
the marker the ChildScope put there, and the runner (same uid) can read any
such process's exec-time environment: sysctl KERN_PROCARGS2 on macOS,
/proc/<pid>/environ on Linux. reap_marked() finds the carriers and kills them.

Limits, stated rather than hidden:

* A process that cannot be attributed by marker is REPORTED, not killed.
  macOS hides the exec-time environment of hardened system binaries
  (/bin/sh, /bin/sleep, /usr/bin/perl, ...) from KERN_PROCARGS2, and a process
  that clears its own environment (`env -i`) carries no marker. Killing "every
  new process whose environment is unreadable" could hit the operator's own
  processes, so unverified_since() lists them (pid, exec path, reason) and the
  episode records them; nothing but a namespace (bwrap's --unshare-pid on
  Linux) closes this.
* The marker is not secret from a concurrent episode: any same-uid process can
  read any other's environment, so a concurrent episode's shell can read this
  episode's marker from a running child. It can then run processes that carry
  it (they die at this episode's close, and can keep the reaper busy until its
  deadline, which is then reported as survivors, never as a hang or a clean
  close). The two checks that pin this, test_open_b26_*, stay open.
* The marker is matched as an exact environment entry, never in argv.
"""
import ctypes
import os
import signal
import struct
import sys
import time

MARKER_VAR = "MH_EPISODE"
# Bounds: one scan never walks more than MAX_PIDS processes or works longer than
# SCAN_BUDGET_S, one kill pass never signals more than MAX_KILLS_PER_PASS pids,
# and reap_marked() never works longer than its deadline (plus one kill pass).
MAX_PIDS = 1 << 18
MAX_KILLS_PER_PASS = 4096
SCAN_BUDGET_S = 3.0
REAP_DEADLINE_S = 2.0
_POLL_S = 0.05

_KERN_PROCARGS2 = (1, 49)       # CTL_KERN, KERN_PROCARGS2 (sys/sysctl.h)
_PROC_PIDTBSDINFO = 3           # proc_pidinfo flavor; struct proc_bsdinfo is 136 bytes
_BSDINFO_SIZE = 136
_SZOMB = 5


class ScanIncomplete(OSError):
    """The process scan was cut short, so "nothing found" would not be a clean result."""


def _darwin_libs():
    libc = ctypes.CDLL(None, use_errno=True)
    libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    return libc, libproc


def _all_pids():
    """Every pid visible to this user; ScanIncomplete when the list hit MAX_PIDS."""
    if sys.platform == "darwin":
        _, libproc = _darwin_libs()
        slack = 256
        while True:
            want = libproc.proc_listallpids(None, 0)
            if want <= 0:
                raise OSError("proc_listallpids could not size the process list")
            size = min(want + slack, MAX_PIDS)
            buf = (ctypes.c_int * size)()
            got = libproc.proc_listallpids(buf, ctypes.sizeof(buf))
            if got < 0:
                raise OSError("proc_listallpids failed")
            if got < size:
                return [p for p in buf[:got] if p > 0]
            if size == MAX_PIDS:
                raise ScanIncomplete(f"more than {MAX_PIDS} processes; the scan is capped")
            slack *= 4
    pids = []
    with os.scandir("/proc") as it:
        for e in it:
            if e.name.isdigit():
                pids.append(int(e.name))
                if len(pids) >= MAX_PIDS:
                    raise ScanIncomplete(f"more than {MAX_PIDS} processes; the scan is capped")
    return pids


def _parse_procargs(raw):
    """The environment block of a KERN_PROCARGS2 buffer, or None when it is malformed.

    Layout: int32 argc, the exec path, NUL padding, argc argv strings, then the
    environment strings. Only the part after argv is returned (as
    \0 entry \0 entry \0), so a marker in argv is never mistaken for one in the
    environment. A process whose environment the kernel withholds (a hardened
    binary) parses to b"": readable, with no entries.
    """
    if len(raw) < 4:
        return None
    argc = struct.unpack("=i", raw[:4])[0]
    i = raw.find(b"\0", 4)
    if argc < 0 or i < 0:
        return None
    while i < len(raw) and raw[i] == 0:
        i += 1
    for _ in range(argc):
        j = raw.find(b"\0", i)
        if j < 0:
            return None
        i = j + 1
    entries = [e for e in raw[i:].split(b"\0") if e]
    return b"\0" + b"\0".join(entries) + b"\0" if entries else b""


def _exec_environment(pid):
    """The environment block pid was exec'd with, or None when unreadable.

    None means gone, a zombie or another user's. b"" means readable but empty:
    a hardened binary on macOS, or a process exec'd with no environment.
    """
    if sys.platform == "darwin":
        libc, _ = _darwin_libs()
        mib = (ctypes.c_int * 3)(*_KERN_PROCARGS2, pid)
        size = ctypes.c_size_t(0)
        if libc.sysctl(mib, 3, None, ctypes.byref(size), None, 0) != 0 or not size.value:
            return None
        buf = ctypes.create_string_buffer(size.value)
        if libc.sysctl(mib, 3, buf, ctypes.byref(size), None, 0) != 0:
            return None
        return _parse_procargs(buf.raw[:size.value])
    try:
        with open(f"/proc/{pid}/environ", "rb") as f:
            raw = f.read()
    except OSError:
        return None
    entries = [e for e in raw.split(b"\0") if e]
    return b"\0" + b"\0".join(entries) + b"\0" if entries else b""


def _carries(pid, needle):
    raw = _exec_environment(pid)
    return raw is not None and (b"\0" + needle + b"\0") in raw


def marked_pids(marker, exclude=()):
    """Pids whose exec-time environment holds MH_EPISODE=<marker>, ours excluded.

    Raises ScanIncomplete when the process list was capped or the scan ran past
    SCAN_BUDGET_S: an incomplete scan is never an answer.
    """
    needle = f"{MARKER_VAR}={marker}".encode()
    skip = set(exclude) | {os.getpid()}
    end = time.monotonic() + SCAN_BUDGET_S
    found = []
    for p in _all_pids():
        if p in skip:
            continue
        if time.monotonic() >= end:
            raise ScanIncomplete(f"the process scan ran past its {SCAN_BUDGET_S}s budget")
        if _carries(p, needle):
            found.append(p)
    return found


def reap_marked(marker, deadline_s=REAP_DEADLINE_S):
    """Kill every process carrying `marker`; return the pids still alive afterwards.

    Kill, rescan, repeat until a scan finds none or the deadline passes. Every
    scan that finds something is followed by a full kill pass BEFORE the deadline
    is looked at, so a scan slower than the deadline still kills what it found. A
    pid's marker is read again immediately before its SIGKILL, so a pid recycled
    since the scan is not shot. The returned list is empty only when a scan that
    ran after the last kill found nothing; callers report anything else. Raises
    ScanIncomplete when a scan was capped or ran out of budget.
    """
    if not marker:
        raise ValueError("reap_marked needs a non-empty marker")
    needle = f"{MARKER_VAR}={marker}".encode()
    end = time.monotonic() + deadline_s
    while True:
        found = marked_pids(marker)
        if not found:
            return []
        for pid in found[:MAX_KILLS_PER_PASS]:
            if _carries(pid, needle):
                try:
                    os.kill(pid, signal.SIGKILL)
                except (ProcessLookupError, PermissionError):
                    pass
        if time.monotonic() >= end:
            # Deadline passed after a full kill pass: what is still there is reported.
            return [p for p in found if _carries(p, needle)] or marked_pids(marker)
        time.sleep(_POLL_S)


# --- processes that cannot be attributed ---------------------------------------


def _bsdinfo(pid):
    """(ppid, uid, status, start epoch seconds) of pid, or None (macOS only)."""
    _, libproc = _darwin_libs()
    buf = ctypes.create_string_buffer(_BSDINFO_SIZE)
    if libproc.proc_pidinfo(pid, _PROC_PIDTBSDINFO, 0, buf, _BSDINFO_SIZE) != _BSDINFO_SIZE:
        return None
    raw = buf.raw
    (status,) = struct.unpack_from("=I", raw, 4)
    (ppid,) = struct.unpack_from("=I", raw, 16)
    (uid,) = struct.unpack_from("=I", raw, 20)
    sec, usec = struct.unpack_from("=QQ", raw, 120)
    return ppid, uid, status, sec + usec / 1e6


def _exec_path(pid):
    _, libproc = _darwin_libs()
    buf = ctypes.create_string_buffer(4096)
    n = libproc.proc_pidpath(pid, buf, 4096)
    return buf.raw[:n].decode("utf-8", "replace") if n > 0 else ""


def _is_sandboxed(pid):
    """True when pid runs under a sandbox profile (a sandbox-exec child and its descendants)."""
    lib = ctypes.CDLL("/usr/lib/system/libsystem_sandbox.dylib", use_errno=True)
    lib.sandbox_check.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int]
    lib.sandbox_check.restype = ctypes.c_int
    return lib.sandbox_check(pid, None, 0) == 1


UNVERIFIED_REASON = "environment unreadable (hardened binary); cannot attribute"


def unverified_since(started, sandboxed_only=True):
    """Processes that started after `started` and cannot be tied to or cleared of an episode.

    Same-uid, not zombies, not ancestors of this process, started at or after
    `started` (epoch seconds), and whose exec-time environment could NOT be read
    (so the marker could not be looked for). With sandboxed_only (the default on
    macOS) a process that is not under any sandbox profile is left out: every
    child of an episode's shell is sandboxed and the sandbox is inherited
    across fork and exec, so an unsandboxed process is the operator's. Returns
    [{"pid", "path", "reason"}]. Not killed: nobody can say whose they are.
    Only implemented on macOS, where the environment is withheld; elsewhere an
    unreadable environment is a process that already exited.
    """
    if sys.platform != "darwin":
        return []
    ancestors, p, hops = {os.getpid()}, os.getpid(), 0
    while p > 1 and hops < 64:
        info = _bsdinfo(p)
        if info is None:
            break
        p = info[0]
        ancestors.add(p)
        hops += 1
    uid, end, out = os.getuid(), time.monotonic() + SCAN_BUDGET_S, []
    for pid in _all_pids():
        if pid in ancestors:
            continue
        if time.monotonic() >= end:
            raise ScanIncomplete(f"the unverified-process scan ran past its {SCAN_BUDGET_S}s budget")
        info = _bsdinfo(pid)
        if info is None or info[1] != uid or info[2] == _SZOMB or info[3] < started:
            continue
        if _exec_environment(pid):          # readable and non-empty: the marker scan covers it
            continue
        if sandboxed_only and not _is_sandboxed(pid):
            continue
        out.append({"pid": pid, "path": _exec_path(pid), "reason": UNVERIFIED_REASON})
    return out
