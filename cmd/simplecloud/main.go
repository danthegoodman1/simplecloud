// Command simplecloud deploys Docker Compose projects onto Archil sandboxes.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/cli"
)

func main() {
	if err := cli.Root().Execute(); err != nil {
		// Cobra has already printed usage errors; print anything else plainly,
		// without a stack or a Go type name in front of it.
		var silent cli.SilentError
		if !errors.As(err, &silent) {
			fmt.Fprintln(os.Stderr, "error: "+err.Error())
		}
		os.Exit(1)
	}
}

var _ = cobra.Command{}
