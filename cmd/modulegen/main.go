package main

import (
	"flag"
	"fmt"
	"os"

	"starter-kit/internal/scaffold"
)

func main() {
	name := flag.String("name", "", "module name, e.g. order or coffee_fund")
	path := flag.String("path", "", "route relative to /api/v1; defaults to the module name")
	root := flag.String("root", ".", "starter kit root directory")
	dryRun := flag.Bool("dry-run", false, "preview changes without writing files")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments; use -name order")
		os.Exit(1)
	}
	files, err := scaffold.Generate(*root, scaffold.Options{Name: *name, Path: *path, DryRun: *dryRun})
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate module:", err)
		os.Exit(1)
	}
	for _, file := range files {
		fmt.Println(file)
	}
	if *dryRun {
		fmt.Println("Preview only; no files changed.")
	} else {
		fmt.Println("Module registered. Run make test, then make migrate before restarting the API (SQLite/PostgreSQL).")
	}
}
