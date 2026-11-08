// Make benchmark NoCloud media using the runner's existing ISO writer.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/clcdpc/github-runners/internal/seed"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: seed INPUT_DIRECTORY OUTPUT_ISO")
		os.Exit(2)
	}
	files := map[string][]byte{}
	for _, name := range []string{"user-data", "meta-data", "network-config"} {
		b, err := os.ReadFile(filepath.Join(os.Args[1], name))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		files[name] = b
	}
	if err := seed.Write(os.Args[2], files); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
