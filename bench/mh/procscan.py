"""Find and kill every process that carries an episode's environment marker.

A shell command that double-forks and setsid()s leaves the process group, the
session and the parent chain of the episode, so killing the group (which
run_bounded_capture does) cannot reach it, and on macOS there is no PID
namespace to take it down with the episode. What such a process cannot shed is
the environment it was exec'd with: every child of the episode's shell inherits
the marker the ChildScope put there, and the runner (same uid) can read any
such process's exec-time environment: sysctl KERN_PROCARGS2 on macOS,
/proc/<pid>/environ on Linux. reap_marked() finds the carriers and kills them.

Limit, stated rather than hidden: a process that clears its own environment
before it is exec'd (`env -i setsid ...`) carries no marker and is not found
here. Nothing but a namespace (bwrap's --unshare-pid on Linux) catches that.
"""
import ctypes
import os
import signal
import sys
import time

MARKER_VAR = "MH_EPISODE"
# Bounds: the scan never walks more than this many processes, and
# reap_marked() never works longer than its deadline.
MAX_PIDS = 1 << 18
REAP_DEADLINE_S = 2.0
_POLL_S = 0.05

_KERN_PROCARGS2 = (1, 49)       # CTL_KERN, KERN_PROCARGS2 (sys/sysctl.h)


def _darwin_libs():
    libc = ctypes.CDLL(None, use_errno=True)
    libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    return libc, libproc


def _all_pids():
    """Every pid visible to this user, capped at MAX_PIDS."""
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
            if got < size or size == MAX_PIDS:
                return [p for p in buf[:got] if p > 0]
            slack *= 4
    pids = []
    with os.scandir("/proc") as it:
        for e in it:
            if e.name.isdigit():
                pids.append(int(e.name))
                if len(pids) >= MAX_PIDS:
                    break
    return pids


def _exec_environment(pid):
    """The argv+environment block pid was exec'd with, or None when unreadable.

    Unreadable means gone, a zombie, or another user's: in every case there is
    nothing of this episode to kill there.
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
        return buf.raw[:size.value]
    try:
        with open(f"/proc/{pid}/environ", "rb") as f:
            return f.read()
    except OSError:
        return None


def _carries(pid, needle):
    raw = _exec_environment(pid)
    return raw is not None and (b"\0" + needle + b"\0") in raw


def marked_pids(marker, exclude=()):
    """Pids whose exec-time environment holds MH_EPISODE=<marker>, ours excluded."""
    needle = f"{MARKER_VAR}={marker}".encode()
    skip = set(exclude) | {os.getpid()}
    return [p for p in _all_pids() if p not in skip and _carries(p, needle)]


def reap_marked(marker, deadline_s=REAP_DEADLINE_S):
    """Kill every process carrying `marker`; return the pids still alive afterwards.

    Kill, rescan, repeat until a scan finds none or the deadline passes. A pid's
    marker is read again immediately before the SIGKILL, so a pid recycled
    since the scan is not shot. The returned list is empty only when a scan
    that ran after the last kill found nothing; callers report anything else.
    """
    if not marker:
        raise ValueError("reap_marked needs a non-empty marker")
    needle = f"{MARKER_VAR}={marker}".encode()
    end = time.monotonic() + deadline_s
    while True:
        found = marked_pids(marker)
        if not found:
            return []
        if time.monotonic() >= end:
            return found
        for pid in found:
            if _carries(pid, needle):
                try:
                    os.kill(pid, signal.SIGKILL)
                except (ProcessLookupError, PermissionError):
                    pass
        time.sleep(_POLL_S)
