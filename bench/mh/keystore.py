"""Provider credentials: held in memory, handed between processes over a pipe.

A same-uid process can read another's exec-time argv and environment
(sysctl KERN_PROCARGS2 on macOS, /proc/<pid>/environ on Linux), and no sandbox
rule closes that route; unsetenv does not either, because the kernel keeps the
block the process was exec'd with. So a key that was in the runner's environment
when the runner started stays readable to the model's shell for the runner's
whole life.

seal_environment() is the fix: take the keys out of the environment, keep them
in this module's memory, and exec the same command line again with a clean
environment, so the process image the kernel reports holds no key. The keys
cross the exec on an inherited pipe: written before the exec (the payload is
bounded far below the pipe's capacity, so the write cannot block), read once by
the new image, and closed. No file, no environment variable, no argv carries a
key. handoff() does the same for a child the process starts (grid.py's runners).

Called from the `if __name__ == "__main__"` entry of each process that is
launched from a shell, never from a function a test calls in-process: it
replaces the process.

Not covered, and stated: a key the operator's own terminal or launcher put in
ITS exec-time environment is readable there, and so is any other same-uid
process that holds one. This module cleans the processes of this benchmark.
"""
import json
import os
import re
import selectors
import stat
import sys
import time
from urllib.parse import urlsplit

PIPE_FD_VAR = "MH_KEYSTORE_FD"
SECRET_SUFFIXES = ("_API_KEY", "_TOKEN", "_SECRET", "_SECRET_KEY", "_SECRET_ACCESS_KEY",
                   "_PASSWORD")
# A descriptor number is 1-6 ASCII digits, nothing else: int() also accepts
# superscripts, other scripts' digits, signs and spaces, and OverflowError on
# anything the C layer cannot hold.
_FD_RE = re.compile(r"[0-9]{1,6}")
# Longest the reader waits for the writer to finish and close; the writer wrote
# everything before the exec, so a pipe still open after this is not a hand-off.
READ_DEADLINE_S = 5.0
# A pipe holds at least 16 KiB; the payload stays far under it so the writer,
# which runs before the exec and has no reader yet, never blocks.
MAX_PAYLOAD_BYTES = 8192

_store = {}
_restored = {}      # proxy variables put back into os.environ in this image, name -> value


class KeystoreError(RuntimeError):
    """The key hand-off failed; the process must not carry on with keys exposed."""


def is_secret_name(name):
    return name.upper().endswith(SECRET_SUFFIXES)


def _proxy_has_userinfo(value):
    """True when a proxy URL carries user[:password]@ credentials."""
    try:
        netloc = urlsplit(value if "://" in value else "//" + value).netloc
    except ValueError:
        return True         # unparseable: cannot show it is clean, so treat it as secret
    return "@" in netloc


def is_proxy_name(name):
    return name.upper().endswith("_PROXY")


def is_secret_entry(name, value):
    """A name that is a credential by its suffix, or a proxy URL that embeds one."""
    return is_secret_name(name) or (is_proxy_name(name) and _proxy_has_userinfo(value))


def get(name):
    """The in-memory value for `name`, or None."""
    return _store.get(name)


def names():
    return sorted(_store)


def _payload():
    data = json.dumps(_store, separators=(",", ":")).encode("utf-8")
    if len(data) > MAX_PAYLOAD_BYTES:
        raise KeystoreError(
            f"{len(data)} bytes of credentials exceed the {MAX_PAYLOAD_BYTES}-byte "
            f"hand-off bound; refusing to fall back to the environment")
    return data


def _clean_environ(environ):
    return {k: v for k, v in environ.items()
            if not is_secret_entry(k, v) and k != PIPE_FD_VAR}


def _pipe_with_payload():
    """A pipe whose read end is inheritable and already holds the payload."""
    data = _payload()
    r, w = os.pipe()
    try:
        os.write(w, data)
    except BaseException:
        os.close(r)
        raise
    finally:
        os.close(w)
    os.set_inheritable(r, True)
    return r


def _read_inherited(fd):
    """Read, once and completely, the payload on the inherited fd; close it.

    Every failure is a KeystoreError, including a writer that never closes: the
    read has an overall deadline (READ_DEADLINE_S).
    """
    if fd <= 2:
        raise KeystoreError(f"{PIPE_FD_VAR}={fd} is a standard stream, not a hand-off pipe")
    try:
        mode = os.fstat(fd).st_mode
    except OSError as e:
        raise KeystoreError(f"{PIPE_FD_VAR}={fd} is not an open descriptor: {e}") from e
    if not stat.S_ISFIFO(mode):
        os.close(fd)
        raise KeystoreError(f"{PIPE_FD_VAR}={fd} is not a pipe")
    chunks, total = [], 0
    end = time.monotonic() + READ_DEADLINE_S
    try:
        with selectors.DefaultSelector() as sel:
            sel.register(fd, selectors.EVENT_READ)
            while True:
                remaining = end - time.monotonic()
                if remaining <= 0 or not sel.select(remaining):
                    raise KeystoreError(f"the key hand-off pipe was still open after "
                                        f"{READ_DEADLINE_S}s; its writer did not finish")
                chunk = os.read(fd, 4096)
                if not chunk:
                    break
                total += len(chunk)
                if total > MAX_PAYLOAD_BYTES:
                    raise KeystoreError("the key hand-off payload exceeds its bound")
                chunks.append(chunk)
    except OSError as e:
        raise KeystoreError(f"the key hand-off pipe could not be read: {e}") from e
    finally:
        os.close(fd)
    try:
        data = json.loads(b"".join(chunks))
    except ValueError as e:
        raise KeystoreError(f"the key hand-off payload is not JSON: {e}") from e
    if not isinstance(data, dict) or not all(
            isinstance(k, str) and isinstance(v, str) for k, v in data.items()):
        raise KeystoreError("the key hand-off payload is not a name -> value object")
    return data


def _restore_proxies():
    """Put credentialed proxy URLs back into os.environ of THIS image.

    urllib reads *_PROXY from os.environ for every provider call, so the sealed
    runner needs them there. setenv after the exec does not touch the kernel's
    exec-time block, which is what the shell can read, and the shell's own
    environment is an allow-list that never carries them.
    """
    for name, value in _store.items():
        if is_proxy_name(name):
            os.environ[name] = value
            _restored[name] = value


def seal_environment():
    """Move the keys out of this process's exec-time environment; keep them in memory.

    Returns "inherited" when the keys arrived on the hand-off pipe of a sealed
    parent and the environment was already clean, "none" when there was nothing
    to hide, and never returns when it had to re-exec (the new image calls this
    again and takes the inherited branch). Raises KeystoreError rather than
    carry on with a key still in the environment.
    """
    inherited = False
    raw_fd = os.environ.pop(PIPE_FD_VAR, None)
    if raw_fd is not None:
        if not _FD_RE.fullmatch(raw_fd):
            raise KeystoreError(f"{PIPE_FD_VAR}={raw_fd!r} is not a descriptor number")
        _store.update(_read_inherited(int(raw_fd)))
        inherited = True
    # A proxy URL with credentials was restored into os.environ by an earlier
    # seal in this image (below); it is not in the exec-time block, so it is no
    # exposure and must not trigger another exec.
    secret_names = [k for k in os.environ
                    if is_secret_entry(k, os.environ[k]) and _restored.get(k) != os.environ[k]]
    if not secret_names:
        _restore_proxies()
        return "inherited" if inherited else "none"
    # Also reached by an inherited image whose environment still named a key
    # (a parent that did not scrub it): that exec-time exposure is what this
    # call exists to remove, and another exec removes it.
    _store.update({k: os.environ[k] for k in secret_names if os.environ[k]})
    argv = getattr(sys, "orig_argv", None)
    if not argv:
        raise KeystoreError("cannot re-exec: this interpreter has no sys.orig_argv")
    fd = _pipe_with_payload()
    env = _clean_environ(os.environ)
    env[PIPE_FD_VAR] = str(fd)
    sys.stdout.flush()
    sys.stderr.flush()
    try:
        os.execve(sys.executable, argv, env)
    except OSError as e:
        os.close(fd)
        raise KeystoreError(f"could not re-exec to drop credentials from the "
                            f"environment: {e}") from e


def handoff(env):
    """(env, pass_fds, done) for a child that should receive the keys.

    `env` comes back without any secret-named variable and with the pipe's
    descriptor number in PIPE_FD_VAR (when this process holds keys); pass
    `pass_fds` to the Popen and call `done()` once it has started, which closes
    this side's copy of the read end.
    """
    clean = _clean_environ(env)
    if not _store:
        return clean, (), lambda: None
    fd = _pipe_with_payload()
    clean[PIPE_FD_VAR] = str(fd)

    def done():
        try:
            os.close(fd)
        except OSError:
            pass
    return clean, (fd,), done
