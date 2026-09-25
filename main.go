// Command planner is a dry-run rename planner for a case-insensitive shared
// drive. It reads a JSON manifest (from -f or stdin) and prints a plan of
// atomic moves plus reverse rollback steps. It never opens a directory for
// writing and never touches the files named in the manifest.
//
// In -resume mode it instead takes the original manifest, the number of steps
// already completed, and the exact file names observed on the drive, and (only
// when the observation matches the replayed plan prefix) prints the remaining
// forward moves plus a rollback of the executed prefix. This mode is also
// purely in-memory.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// resumeRequest is the -resume input: the original manifest plus the operator's
// observation. Done and Observed may be supplied in JSON or overridden with
// the -done and -observed flags.
type resumeRequest struct {
	Files    []string `json:"files"`
	Renames  []Rename `json:"renames"`
	Done     int      `json:"done"`
	Observed []string `json:"observed"`
}

// resumeReport is the -resume stdout envelope.
type resumeReport struct {
	Status string `json:"status"` // "ok" or "mismatch"
	*Recovery
	Diffs []Diff `json:"diffs,omitempty"`
}

func main() {
	path := flag.String("f", "", "path to the JSON manifest (default: stdin)")
	resumeMode := flag.Bool("resume", false, "interrupt-recovery mode: certify observed names against the plan prefix")
	done := flag.Int("done", -1, "resume: number of plan steps already completed (overrides JSON)")
	observed := flag.String("observed", "", "resume: comma-separated exact observed file names (overrides JSON)")
	flag.Parse()

	var data []byte
	var err error
	if *path == "" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(*path)
	}
	if err != nil {
		fatal(err)
	}

	if !*resumeMode {
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			fatal(fmt.Errorf("parse manifest: %w", err))
		}
		p, err := plan(m)
		if err != nil {
			fatal(err)
		}
		writeJSON(p)
		return
	}

	var req resumeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		fatal(fmt.Errorf("parse resume request: %w", err))
	}
	completed := req.Done
	if *done >= 0 {
		completed = *done
	}
	obs := req.Observed
	if *observed != "" {
		obs = splitCSV(*observed)
	}

	m := Manifest{Files: req.Files, Renames: req.Renames}
	rec, diffs, err := resume(m, completed, obs)
	if err != nil {
		fatal(err)
	}
	if diffs != nil {
		writeJSON(resumeReport{Status: "mismatch", Diffs: diffs})
		fmt.Fprintf(os.Stderr, "planner: observed layout does not match the first %d steps; %d difference(s); no actions emitted\n", completed, len(diffs))
		os.Exit(1)
	}
	writeJSON(resumeReport{Status: "ok", Recovery: &rec})
}

// splitCSV splits a comma-separated observed-name list. Empty entries (e.g. a
// trailing comma) are rejected downstream as non-ASCII names rather than
// silently dropped.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p)
	}
	return out
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "planner:", err)
	os.Exit(1)
}
