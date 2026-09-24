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
