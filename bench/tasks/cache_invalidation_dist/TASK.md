Implement `DistCache` in `distcache.py` exactly as `SPEC.md` describes.

`python3 test_dist.py` only checks the no-partition happy path. The hidden
check splits the cluster, writes on both sides, invalidates, heals, and
checks that a stale replica cannot resurrect a deleted key.

Do not modify the test or the spec.
