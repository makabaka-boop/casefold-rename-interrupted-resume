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

If the batch was interrupted mid-run, the operator must confirm that the names
seen on the drive really correspond to one of this plan's execution prefixes
before continuing or rolling back. Resume mode takes the **original**
manifest, the number of steps already completed, and the **exact** file names
currently observed:

```sh
./planner -resume -f state.json
# state.json adds "done" and "observed" to the original manifest:
# {
#   "files": ["a.txt", "b.txt"],
#   "renames": [{"source": "a.txt", "target": "b.txt"},
#               {"source": "b.txt", "target": "a.txt"}],
#   "done": 1,
#   "observed": ["__rename_tmp_0__", "b.txt"]
# }
```

`done`/`observed` may instead be supplied as flags (`-done 1 -observed
'__rename_tmp_0__,b.txt'`).

What it does, entirely in memory (no file system access):

1. Rebuilds the **same deterministic plan** from the original manifest — the
   temp name is whatever that plan chose; it is never reselected to fit the
   observation.
2. Replays the first `done` steps on a case-insensitive occupancy table that
   refuses to overwrite.
3. Compares the observed set against the predicted prefix layout **twice**:
   normalized occupancy (foreign/missing) **and** exact case (case drift).
4. Only on an exact match does it print `forward` (the untouched plan tail)
   and `rollback` (the inverse of the executed prefix only). Replaying either
   sequence from the observed layout never overwrites: `forward` finishes the
   batch, `rollback` restores the original names.

This covers the critical *parked* state — the file that has just moved to the
temp name. For a swap/`N`-cycle the tail first vacates a real name, then loads
the parked file; the safe undo is the single move `temp → parked source`. For a
case-only rename (length-1 cycle) there is nobody else to vacate, so the tail
loads the parked file straight to its final name.

On any discrepancy the report is `{"status": "mismatch", "diffs": [...]}`, the
process exits non-zero, and **no executable actions are emitted**. Each diff is
one of:

- `foreign` — an observed name the prefix cannot have produced;
- `missing` — a name the prefix predicts that is not observed;
- `case-drift` — normalized name matches but the exact spelling differs
  (including a temp slot held with different case).

A wrong `done` count manifests as foreign+missing pairs; a stranger occupying
the expected temp index is reported, never worked around.

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
- **Interrupt recovery**: every interruption point (cuts 0…N) of a chain, a
  long chain, a 2-node swap cycle, a 3-node cycle, a case-only rename, and a
  multi-component plan is rebuilt and replayed — the returned `forward` tail
  must finish the batch and `rollback` must restore the original layout
  without overwriting. The parked-at-temp cuts (including the file sitting at
  the temp name across components) are checked explicitly for both continue
  and undo. Mismatch tests cover foreign files, missing files, case drift on
  results/bystanders/temp names, wrong temp index (no reselection), wrong
  step counts, and combined discrepancies.
