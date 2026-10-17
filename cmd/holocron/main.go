// Command holocron is the single executable of the Holocron NVR: every action
// (start, stop, configure, cameras, web app, install, logs) is a subcommand of
// it. See tasks/portable-nvr/os-independent-hld.md (rules R1 and R2).
package main

import (
	"fmt"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("holocron", version)
		return
	}
	fmt.Fprintln(os.Stderr, "holocron: the command tree is not built yet (see tasks/portable-nvr/os-independent-lld.md section 2.12)")
	os.Exit(2)
}
