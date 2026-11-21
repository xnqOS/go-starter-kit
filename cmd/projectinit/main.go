package main

import (
	"flag"
	"fmt"
	"os"

	"starter-kit/internal/projectinit"
)

func main() {
	module := flag.String("module", "", "new Go module path, e.g. github.com/acme/my-api")
	destination := flag.String("dest", "", "export to a new directory instead of changing this project")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(1)
	}
	root, err := projectinit.Initialize(".", *module, *destination)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Initialized %s in %s\nNext: cd into the project, copy .env.example to .env, run go mod tidy (fetches the shared kit module), then make check.\n", *module, root)
}
