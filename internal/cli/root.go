// Package cli wires the command surface.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/danthegoodman1/simplecloud/internal/archil"
	"github.com/danthegoodman1/simplecloud/internal/compose"
	"github.com/danthegoodman1/simplecloud/internal/config"
	"github.com/danthegoodman1/simplecloud/internal/deploy"
	"github.com/danthegoodman1/simplecloud/internal/plan"
	"github.com/danthegoodman1/simplecloud/internal/state"
)

// SilentError is already reported, so main should not print it again.
type SilentError struct{ error }

func (e SilentError) Error() string { return e.error.Error() }
func (e SilentError) Unwrap() error { return e.error }

var flags struct {
	config.Flags
	Project string
	JSON    bool
}

func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "simplecloud",
		Short: "Deploy Compose projects with per-project private networks and services that sleep",
		Long: "simplecloud deploys a Docker Compose project so that each service runs in its own\n" +
			"sandbox, services reach each other by name over a private network, and idle\n" +
			"services sleep and wake on demand.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	p := root.PersistentFlags()
	p.StringVar(&flags.Region, "region", "", "compute region")
	p.StringVar(&flags.Hub, "hub", "", "WireGuard hub SSH target, such as root@203.0.113.10")
	p.StringVar(&flags.HubIdentity, "identity", "", "SSH identity file for the hub")
	p.StringVar(&flags.HubEndpoint, "endpoint", "", "WireGuard endpoint host, when it differs from the SSH target")
	p.StringVar(&flags.Registry, "registry", "", "registry for built images")
	p.StringSliceVar(&flags.Profiles, "profile", nil, "additional Compose profiles to activate")
	p.StringVarP(&flags.Project, "project", "p", "", "project name, when not running inside its directory")
	p.BoolVar(&flags.JSON, "json", false, "machine-readable output")

	root.AddCommand(
		upCmd(),
		planCmd(),
		lsCmd(),
		psCmd(),
		sleepCmd(),
		wakeCmd(),
		reapCmd(),
		endpointsCmd(),
		urlCmd(),
		logsCmd(),
		showCmd(),
		volumesCmd(),
		execCmd(),
		downCmd(),
		hubCmd(),
		buildCmd(),
		registryAddCmd(),
		linkCmd(),
		doctorCmd(),
		reconcileCmd(),
		versionCmd(),
	)
	return root
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "simplecloud 0.1.0-dev")
			return nil
		},
	}
}

// env carries everything a command needs, resolved once.
type env struct {
	ctx    context.Context
	cfg    *config.Config
	store  *state.Store
	client *archil.Client
}

func openEnv(cmd *cobra.Command, needAPI bool) (*env, func(), error) {
	cfg, err := config.Load(flags.Flags)
	if err != nil {
		return nil, nil, err
	}
	st, err := state.Open(cfg.StatePath())
	if err != nil {
		return nil, nil, err
	}
	e := &env{ctx: cmd.Context(), cfg: cfg, store: st}
	if e.ctx == nil {
		e.ctx = context.Background()
	}
	if needAPI {
		if err := cfg.RequireAPIKey(); err != nil {
			st.Close()
			return nil, nil, err
		}
		c, err := archil.New(cfg.APIKey, cfg.Region)
		if err != nil {
			st.Close()
			return nil, nil, err
		}
		e.client = c
	}
	return e, func() { st.Close() }, nil
}

// loadProject finds the Compose file for the current directory and the project
// record bound to it, creating the binding on first use.
func (e *env) loadProject(create bool) (*compose.Project, *state.Project, error) {
	if flags.Project != "" {
		proj, err := e.store.ProjectByName(flags.Project)
		if err != nil {
			return nil, nil, err
		}
		file, err := compose.FindFile(proj.Dir)
		if err != nil {
			return nil, nil, fmt.Errorf("project %q is bound to %s, which has no Compose file.\n  Run simplecloud link from its new location", proj.Name, proj.Dir)
		}
		cp, err := compose.Load(file)
		return cp, proj, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	file, err := compose.FindFile(cwd)
	if err != nil {
		return nil, nil, err
	}
	cp, err := compose.Load(file)
	if err != nil {
		return nil, nil, err
	}
	proj, err := e.store.ProjectForDir(cp.Dir)
	if err == state.ErrNoProject {
		if !create {
			return nil, nil, fmt.Errorf("%s is not deployed yet.\n  Run simplecloud up to deploy it", cp.Dir)
		}
		region := e.cfg.Region
		if region == "" {
			region = archil.DefaultRegion
		}
		proj, err = e.store.CreateProject(cp.Name, cp.Dir, region)
		if err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, err
	}
	return cp, proj, nil
}

// deployerQuiet builds a deployer whose progress output is discarded, for read
// commands that only need the agent control client.
func (e *env) deployerQuiet() (*deploy.Deployer, func(), error) {
	client, rec, err := e.connectHub(false)
	if err != nil {
		return nil, nil, err
	}
	return &deploy.Deployer{
		Ctx: e.ctx, Store: e.store, Client: e.client, Hub: client, HubRec: rec,
		Fetcher: plan.NewFetcher(), Out: io.Discard,
	}, func() { client.Close() }, nil
}

func (e *env) buildPlan(cp *compose.Project, proj *state.Project) (*plan.Plan, error) {
	return plan.Build(e.ctx, e.store, plan.NewFetcher(), cp, proj, e.cfg.Endpoint(), e.cfg.Profiles)
}
