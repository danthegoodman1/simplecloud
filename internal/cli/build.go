package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/build"
)

func buildCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "build [service...]",
		Short: "Build and push images without deploying",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, done, err := openEnv(cmd, true)
			if err != nil {
				return err
			}
			defer done()
			cp, proj, err := e.loadProject(true)
			if err != nil {
				return err
			}
			d, closeHub, err := e.deployer(cmd)
			if err != nil {
				return err
			}
			defer closeHub()
			if err := d.BuildAll(cp, proj, e.cfg.Registry, args); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "built for %s\n", build.Platform)
			return nil
		},
	}
}

func registryAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "registry add <host>",
		Short: "Record the registry built images are pushed to",
		Long: "Pushing uses your Docker credential store, so this records only where images\n" +
			"go. The credential Archil needs to pull a private image comes from\n" +
			"SIMPLECLOUD_REGISTRY_USERNAME and SIMPLECLOUD_REGISTRY_PASSWORD; make that one\n" +
			"read-only, so the token that leaves this machine can only fetch.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, done, err := openEnv(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			e.cfg.Registry = args[0]
			if err := e.cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "registry set to %s\n", args[0])
			return nil
		},
	}
}
