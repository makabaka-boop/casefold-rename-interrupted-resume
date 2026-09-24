// Command planner is a dry-run rename planner for a case-insensitive shared
// drive. It reads a JSON manifest (from -f or stdin) and prints a plan of
// atomic moves plus reverse rollback steps. It never opens a directory for
// writing and never touches the files named in the manifest.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	path := flag.String("f", "", "path to the JSON manifest (default: stdin)")
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

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		fatal(fmt.Errorf("parse manifest: %w", err))
	}

	p, err := plan(m)
	if err != nil {
		fatal(err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "planner:", err)
	os.Exit(1)
}
