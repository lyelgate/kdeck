// Command kdeck is kdeck: kubectl plus shortcuts, silent-failure checks,
// a krew-compatible plugin manager and Linode LKE management in one binary.
package main

import (
	"os"

	"github.com/lyelgate/kdeck/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args))
}
