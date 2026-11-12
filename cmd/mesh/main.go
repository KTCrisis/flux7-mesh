// Command mesh is the operator CLI of a running mesh7: approvals, emergency
// stop, upstreams, catalogue pins. The same commands are subcommands of mesh7
// (`mesh7 approve`, `mesh7 halt`, ...); this binary is kept for the scripts
// and habits that call it by this name.
package main

import (
	"os"

	"github.com/KTCrisis/flux7-mesh/internal/opcli"
)

func main() {
	opcli.Main("mesh", os.Args[1:])
}
