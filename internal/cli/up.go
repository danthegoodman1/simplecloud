package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/archil"
	"github.com/danthegoodman1/simplecloud/internal/deploy"
	"github.com/danthegoodman1/simplecloud/internal/plan"
)

func upCmd() *cobra.Command {
	var forceAgent bool
	c := &cobra.Command{
		Use:   "up [service...]",
		Short: "Deploy the project, or update only the named services",
		Long: "Converges the project on its Compose file. Naming services restricts the work\n" +
			"to those, so one service can be updated without disturbing the rest of the stack.\n" +
			"A service whose image and configuration are unchanged is left alone.",
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
			for _, name := range args {
				if cp.Service(name) == nil {
					return fmt.Errorf("no service named %q in %s", name, cp.File)
				}
			}
			d, closeHub, err := e.deployer(cmd)
			if err != nil {
				return err
			}
			defer closeHub()
			d.ForceAgent = forceAgent

			// Builds happen before planning, so the plan sees an ordinary image
			// reference and the config hash covers the built digest.
			if err := d.BuildAll(cp, proj, e.cfg.Registry, args); err != nil {
				return err
			}
			pl, err := e.buildPlan(cp, proj)
			if err != nil {
				return err
			}
			if err := d.Up(cp, proj, pl, args); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout())
			return e.printEndpoints(cmd, proj, true)
		},
	}
	c.Flags().BoolVar(&forceAgent, "force-agent", false, "reinstall the agent even where it is answering")
	return c
}

// deployer assembles everything a converge needs, including the hub connection.
func (e *env) deployer(cmd *cobra.Command) (*deploy.Deployer, func(), error) {
	client, rec, err := e.connectHub(false)
	if err != nil {
		return nil, nil, err
	}
	if err := e.store.PutHub(rec); err != nil {
		client.Close()
		return nil, nil, err
	}
	d := &deploy.Deployer{
		Ctx: e.ctx, Store: e.store, Client: e.client, Hub: client, HubRec: rec,
		Fetcher: plan.NewFetcher(), Out: cmd.OutOrStdout(),
		Registry: registryAuth(e),
	}
	return d, func() { client.Close() }, nil
}

// registryAuth is the read-only credential Archil uses to pull a private image.
// Pushing uses the local Docker credential store instead, so nothing wider than a
// pull credential ever leaves this machine.
func registryAuth(e *env) *archil.RegistryAuth {
	user, pass := archilRegistryEnv()
	if user == "" || pass == "" {
		return nil
	}
	return &archil.RegistryAuth{Username: user, Password: pass}
}

func archilRegistryEnv() (string, string) {
	return envOr("SIMPLECLOUD_REGISTRY_USERNAME"), envOr("SIMPLECLOUD_REGISTRY_PASSWORD")
}

func envOr(key string) string {
	v, _ := lookupEnv(key)
	return strings.TrimSpace(v)
}
