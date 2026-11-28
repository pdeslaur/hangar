// Command ha-config-sync keeps Home Assistant's configuration in a GitHub
// repository, beside Home Assistant in its pod: it deploys the repository's
// commits whose Checks passed, and carries the UI's edits back as pull
// requests it merges once their Checks pass.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/google/go-github/v93/github"
	"github.com/pdeslaur/hangar/internal/haconfig"
	"github.com/pdeslaur/hangar/internal/haconfig/integration"
	"github.com/pdeslaur/hangar/internal/systemd"
	"github.com/sethvargo/go-envconfig/v2"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"golang.org/x/oauth2"
)

type config struct {
	// ConfigDir is Home Assistant's configuration directory, shared with
	// its container.
	ConfigDir string `env:"CONFIG_DIR,default=/config"`
	// Repository is the configuration's, owner/name.
	Repository string `env:"REPOSITORY,required"`
	// Branch is the branch it deploys, and merges the UI's edits into.
	Branch string `env:"BRANCH,default=main"`
	// SyncBranch carries the UI's edits; it moves it by force.
	SyncBranch string `env:"SYNC_BRANCH,default=ha-ui-sync"`
	// OctoSTSURL is octo-sts's, which mints its GitHub tokens.
	OctoSTSURL string `env:"OCTO_STS_URL,default=https://octo-sts.dev"`
	// OctoSTSAudience is its JWT-SVIDs'.
	OctoSTSAudience string `env:"OCTO_STS_AUDIENCE,default=octo-sts.dev"`
	// Identity names the repository's trust policy,
	// .github/chainguard/<Identity>.sts.yaml.
	Identity string `env:"IDENTITY,default=ha-config-sync"`
	// PollInterval is how often it looks for new commits and Checks.
	PollInterval time.Duration `env:"POLL_INTERVAL,default=30s"`
	// Debounce is how long the UI's files stay unwritten before it syncs.
	Debounce time.Duration `env:"DEBOUNCE,default=30s"`
	// ChecksTimeout is how long it waits for a commit's Checks.
	ChecksTimeout time.Duration `env:"CHECKS_TIMEOUT,default=30m"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := envconfig.MustProcess(ctx, &config{})
	if err := run(ctx, cfg); err != nil {
		clog.ErrorContext(ctx, "syncing", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config) error {
	owner, repo, ok := strings.Cut(cfg.Repository, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return fmt.Errorf("REPOSITORY %q isn't owner/name", cfg.Repository)
	}

	// Before Home Assistant starts, which waits for this container to be
	// ready: it loads an integration's code only as it starts.
	changed, err := integration.Install(cfg.ConfigDir)
	if err != nil {
		return fmt.Errorf("installing the %s integration: %w", integration.Domain, err)
	}
	if changed {
		clog.InfoContext(ctx, "installed the integration; a running Home Assistant loads it at its next restart", "domain", integration.Domain)
	}
	if err := systemd.Ready(); err != nil {
		return err
	}

	// The Workload API's address comes from SPIFFE_ENDPOINT_SOCKET.
	svids, err := workloadapi.New(ctx)
	if err != nil {
		return err
	}
	defer svids.Close()
	tokens := haconfig.OctoSTS(ctx, svids, cfg.OctoSTSURL, cfg.OctoSTSAudience, cfg.Repository, cfg.Identity)
	client, err := github.NewClient(
		github.WithHTTPClient(oauth2.NewClient(ctx, tokens)),
		github.WithTimeout(2*time.Minute),
	)
	if err != nil {
		return err
	}
	gh := &haconfig.GitHub{Client: client, Owner: owner, Repo: repo}
	hass := haconfig.NewHass(filepath.Join(cfg.ConfigDir, haconfig.StateDir, "api.sock"))

	deployer := &haconfig.Deployer{
		Dir:           cfg.ConfigDir,
		Branch:        cfg.Branch,
		GitHub:        gh,
		Hass:          hass,
		Interval:      cfg.PollInterval,
		ChecksTimeout: cfg.ChecksTimeout,
	}
	syncer := &haconfig.Syncer{
		Dir:           cfg.ConfigDir,
		Branch:        cfg.Branch,
		SyncBranch:    cfg.SyncBranch,
		GitHub:        gh,
		Hass:          hass,
		Debounce:      cfg.Debounce,
		Interval:      cfg.PollInterval,
		ChecksTimeout: cfg.ChecksTimeout,
	}
	clog.InfoContext(ctx, "syncing", "repository", cfg.Repository, "branch", cfg.Branch, "dir", cfg.ConfigDir)

	// Either stopping, on an error it can't retry, stops both.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- deployer.Run(ctx) }()
	go func() { errs <- syncer.Run(ctx) }()
	err = <-errs
	cancel()
	return errors.Join(err, <-errs)
}
