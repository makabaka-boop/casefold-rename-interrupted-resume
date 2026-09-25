// Command planner is a dry-run rename planner for a case-insensitive shared
// drive. It reads a JSON manifest (from -f or stdin) and prints a plan of
// atomic moves plus reverse rollback steps. It never opens a directory for
// writing and never touches the files named in the manifest.
//
// With -resume it runs interrupt recovery instead: the input gains "done"
// (executed step count) and "observed" (exact file names seen on the drive),
// and it prints either the remaining forward moves plus a reverse path for the
// executed prefix, or a mismatch report with no executable actions.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	path := flag.String("f", "", "path to the JSON input (default: stdin)")
	resumeMode := flag.Bool("resume", false, "interrupt recovery: input carries done and observed file names")
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

	var out any
	var mismatch bool
	if *resumeMode {
		var req ResumeRequest
		if err := json.Unmarshal(data, &req); err != nil {
			fatal(fmt.Errorf("parse resume request: %w", err))
		}
		r, err := resume(Manifest{Files: req.Files, Renames: req.Renames}, req.Done, req.Observed)
		if err != nil {
			fatal(err)
		}
		out = r
		mismatch = !r.Match
	} else {
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			fatal(fmt.Errorf("parse manifest: %w", err))
		}
		p, err := plan(m)
		if err != nil {
			fatal(err)
		}
		out = p
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fatal(err)
	}
	if mismatch {
		fmt.Fprintln(os.Stderr, "planner: observed layout does not match the executed prefix; no actions emitted")
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "planner:", err)
	os.Exit(1)
}
