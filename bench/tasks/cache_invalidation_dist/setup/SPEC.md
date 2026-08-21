# Split-brain cache

`DistCache(n_nodes)` simulates `n_nodes` replicas of a key-value cache
connected by a network that can partition.

Keys and values are strings. Node ids are `0 .. n_nodes-1`.

## Local store

Each node holds a map `key -> (tag, clock, origin)` where `tag` is either
the string value or the tombstone marker `"__tomb__"`. Missing keys have
no entry.

## Clocks

Each node has a Lamport clock, starting at 0. Every `put` or `invalidate`
on node `i` does:

1. `clock[i] = clock[i] + 1`
2. builds a record `(tag, clock[i], i)`
3. applies that record to every node currently **reachable** from `i`,
   including `i` itself
4. when applying to node `j`, also `clock[j] = max(clock[j], clock[i])`

A record `R` **wins** over an existing record `S` iff
`(R.clock, R.origin) > (S.clock, S.origin)` lexicographically. Equal
versions are identical; a missing key always loses to any record.

## Reachability

Initially every node is reachable from every other node (one component).

`split(groups)` takes a list of lists of node ids. The lists must be a
partition of `{0, 1, ..., n_nodes-1}` — every id once. After `split`,
node `a` can reach node `b` iff they sit in the same group.

`heal()` puts every node back in one component, then runs **anti-entropy**:
for each key that appears on any node, every node stores the winning
record (including tombstones). Clocks become `max(clock)` on every node.

## Operations

- `put(node, key, value)` — `value` must be a string and must not be
  `"__tomb__"`. Replicate as above.
- `invalidate(node, key)` — same, with tag `"__tomb__"`.
- `get(node, key)` — read **only** that node's local store. Return the
  string value, or `None` if the key is missing **or** a tombstone.

`get` never communicates. A node isolated by `split` does not see writes
on the other side until `heal()`.

Do not mutate caller-owned arguments.
