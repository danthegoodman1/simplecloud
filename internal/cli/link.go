package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func linkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "link <project>",
		Short: "Rebind an existing project to the current directory",
		Long: "A project's identity is bound to the directory holding its Compose file, so two\n" +
			"checkouts with the same name stay distinct. Moving the directory breaks that\n" +
			"binding, and this restores it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, done, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			proj, err := e.store.ProjectByName(args[0])
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			old := proj.Dir
			if err := e.store.RebindProject(proj.ID, cwd); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "project %s rebound\n  from %s\n  to   %s\n", proj.Name, old, cwd)
			return nil
		},
	}
}
