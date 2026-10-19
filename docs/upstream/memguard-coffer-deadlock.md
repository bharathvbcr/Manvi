# Draft: memguard coffer-mutex deadlock on mlock failure

**Status: draft, not posted.** Intended as a comment on
[awnumar/memguard#173](https://github.com/awnumar/memguard/issues/173) (open), which
reports the key-mutex half of this and has an open fix in PR #174. Posting it is the
owner's call. Checked against memguard v0.23.0, the latest tag on 2026-10-19.

Manvi's workaround is `manvi/credentials/lockedmem.go`; it can be removed once memguard
returns these failures as errors (#175) in a release this module pins.

---

There is a second self-deadlock on the same path that #174 does not cover: the coffer's
own mutex.

`Coffer.View` (`core/coffer.go:78`) holds the coffer mutex while it calls `NewBuffer(32)`.
If that `mlock` fails, `Panic` → `Purge` → `key.Destroyed()` (`core/exit.go:24`) →
`Coffer.Destroyed` → `s.Lock()` (`core/coffer.go:170`) on the mutex the same goroutine
already holds. It does not need first-time initialisation: it happens on any
`NewEnclave` or `Enclave.Open` once the key exists. With #174 applied, `View` passes
`keyMtxLocked=false`, which is right for `keyMtx`, but `Purge` still takes `key.Lock()`
through `Destroyed()` and then again directly.

Reproduction on macOS (arm64, Go 1.27, memguard v0.23.0): seal one enclave while memory
is available, then `unix.Setrlimit(unix.RLIMIT_MEMLOCK, &unix.Rlimit{})` and call
`memguard.NewEnclave` again. The sealing goroutine:

```
sync.(*Mutex).Lock
github.com/awnumar/memguard/core.(*Coffer).Destroyed       core/coffer.go:170
github.com/awnumar/memguard/core.Purge.func1               core/exit.go:24
github.com/awnumar/memguard/core.Purge                     core/exit.go:54
github.com/awnumar/memguard/core.Panic                     core/exit.go:85
github.com/awnumar/memguard/core.NewBuffer(0x20)           core/buffer.go:74
github.com/awnumar/memguard/core.(*Coffer).View            core/coffer.go:83
github.com/awnumar/memguard/core.NewEnclave                core/enclave.go:54
github.com/awnumar/memguard.NewEnclave                     enclave.go:20
```

The re-key goroutine (`Coffer.Rekey`, `core/coffer.go:100`) then waits behind it on the
same mutex. In a quiet test binary the runtime reports "all goroutines are asleep"; in a
process with live timers and pollers it is a silent hang (we saw one of 9m25s, ended by a
test timeout).

Related: where no lock is held (`Enclave.Open`'s buffer, `memguard.NewBuffer`), the same
failure completes `Purge` and then panics, which destroys every live `LockedBuffer`. A
destroyed buffer's `String()` is `""`, so code that keeps views for matching silently
matches the empty string afterwards. #175 (errors instead of panics) would remove both
classes.

Our workaround outside memguard: probe that the pages can be mlocked before each call,
serialise our memguard calls, and bound each call with a watchdog.
