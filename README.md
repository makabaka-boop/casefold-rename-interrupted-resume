# rename-planner

Dry-run rename planner for a **case-insensitive shared drive**. Given a JSON
manifest of current file names and desired `source → target` mappings, it
prints an ordered plan of atomic moves — plus the reverse rollback steps —
without ever touching the files themselves.

On a case-insensitive drive, naive one-by-one renames can overwrite files that
are not part of the batch (`mv a B` clobbers an existing `b`), and a case-only
rename (`mv a A`) can be misreported as done when nothing happened. The
planner avoids both by construction.

## Manifest

```json
{
  "files": ["a.txt", "b.txt", "c.txt"],
  "renames": [
    {"source": "a.txt", "target": "b.txt"},
    {"source": "b.txt", "target": "a.txt"},
    {"source": "c.txt", "target": "C.TXT"}
  ]
}
```

Validation rules (violations are rejected with an error, no plan is emitted):

- 1–200 current file names, all printable ASCII.
- Current names are unique after ASCII lower-casing.
- Every source is one of the current names (exact match), at most once.
- No no-op renames (`source == target`); case-only changes are allowed.
- Targets are unique after lower-casing.
- A target may not occupy the name of a file that does not participate.

## Planning rules

- Every step moves a file to a name that is **free at that moment**.
- The rename graph (each source points at the participant currently holding
  its target) decomposes into **chains** and **cycles**:
  - *Chains* are executed in reverse dependency order, tail first.
  - *Cycles* (including length-1 self loops, i.e. case-only renames) park
    their minimum source at a temp name once, then unwind — exactly **one
    extra temp move per cycle**.
- The temp name is the smallest-numbered `__rename_tmp_<i>__` that collides
  with no current name and no target; components run sequentially, so the
  same temp name is reused.
- Components are emitted in order of their minimum (case-folded) source name.
- `rollback` is the exact reverse sequence of inverse moves; replaying it
  restores the original layout and never overwrites.
- Move count is minimal: `len(renames) + number_of_cycles`.

## Usage

```sh
go build -o planner .
./planner -f manifest.json        # or: cat manifest.json | ./planner
```

Output:

```json
{
  "moves": 3,
  "steps": [
    {"from": "a.txt", "to": "__rename_tmp_0__"},
    {"from": "b.txt", "to": "a.txt"},
    {"from": "__rename_tmp_0__", "to": "b.txt"}
  ],
  "rollback": [
    {"from": "b.txt", "to": "__rename_tmp_0__"},
    {"from": "a.txt", "to": "b.txt"},
    {"from": "__rename_tmp_0__", "to": "a.txt"}
  ]
}
```

## Interrupt recovery (`-resume`)

If a batch was interrupted halfway, run with `-resume`. The input adds the
number of steps already executed and the **exact** file names observed on the
drive:

```json
{
  "files": ["a.txt", "b.txt"],
  "renames": [
    {"source": "a.txt", "target": "b.txt"},
    {"source": "b.txt", "target": "a.txt"}
  ],
  "done": 1,
  "observed": ["b.txt", "__rename_tmp_0__"]
}
```

```sh
./planner -resume -f resume.json
```

Recovery:

1. Rebuilds the identical deterministic plan from the original manifest (the
   temp name is **never reselected** — it comes from the rebuilt plan).
2. Replays the first `done` steps on an in-memory case-insensitive occupancy
   table, without touching the file system.
3. Accepts the observation only when it matches the expected layout both in
   case-folded occupancy **and** in exact casing.

On a match it prints `remaining` (the unexecuted forward moves, for
continuing) and `reverse` (inverse moves for the executed prefix only, for a
safe partial withdrawal — this equals the tail of the original `rollback`):

```json
{
  "match": true,
  "done": 1,
  "total": 3,
  "remaining": [
    {"from": "b.txt", "to": "a.txt"},
    {"from": "__rename_tmp_0__", "to": "b.txt"}
  ],
  "reverse": [
    {"from": "__rename_tmp_0__", "to": "a.txt"}
  ]
}
```

On any discrepancy — a foreign file, a missing name, case drift, or a temp
slot occupied by the wrong temp name (e.g. `__rename_tmp_1__` instead of
`__rename_tmp_0__`) — `match` is `false`, `diffs` reports each slot
(`missing` / `foreign` / `case-drift`, with `temp: true` for temp-related
rows), no executable actions are emitted, and the process exits non-zero.
This includes the critical case where a file is found still parked at its
temp name: continuing and withdrawing are both offered, but only when the
parked temp name is exactly the one the rebuilt plan used.

Without `-resume`, output is byte-identical to before; recovery is a separate
mode and the original planner output is unchanged.

## Docker / Compose

The container is strictly dry-run: read-only root filesystem, and only the
manifest is mounted (read-only). It prints the plan to stdout.

```sh
docker compose run --rm planner          # uses ./manifest.json
```

The image build also runs `go vet` and the full test suite.

## Tests

```sh
go test ./...
```

- **Exhaustive small-scale enumeration** (1–4 files, ~15.5k manifests): every
  valid mapping is planned, then replayed on an in-memory case-insensitive
  file table that refuses to overwrite. Checks: final names match the mapping
  exactly, bystanders untouched, move count equals the proven minimum
  (`renames + cycles`), exactly one temp store/load pair per cycle, temp name
  collision-free, components sorted, plan deterministic, and rollback restores
  the original layout without ever moving onto an occupied name.
- Targeted unit tests for chain reversal, 2- and 3-node cycles, case-only
  renames, mixed-case cycles, temp-name collision skipping, component
  ordering, the 200-file limit, and every validation error.
- **Interrupt recovery** walks every interrupt point (`0..len(steps)`) of a
  chain, a 2-node swap, a 3-node cycle, a case-only rename, and mixed
  components: the rebuilt prefix must match observed names exactly, the
  remaining steps must continue to the final mapping, and the prefix-only
  reverse steps must restore the original layout — with special focus on the
  point where the file sits parked at its temp name. Mismatch tests cover
  foreign files, missing files, case drift, and wrong-index temp occupancy;
  each reports diffs and emits no actions.
