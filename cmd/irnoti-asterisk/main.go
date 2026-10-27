// irnoti-asterisk installs the IRNoti integration on an Asterisk server and keeps a tunnel to the API open.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/irnoti/asterisk-agent/internal/api"
	"github.com/irnoti/asterisk-agent/internal/ari"
	"github.com/irnoti/asterisk-agent/internal/config"
	"github.com/irnoti/asterisk-agent/internal/installer"
	"github.com/irnoti/asterisk-agent/internal/tunnel"
	"github.com/irnoti/asterisk-agent/internal/version"
)

const usage = `irnoti-asterisk %s

Usage:
  irnoti-asterisk install   --api URL --token TOKEN [--asterisk-dir DIR] [--extensions-file FILE] [--no-service] [--dry-run]
  irnoti-asterisk run       [--config FILE]            (the daemon; installed as a systemd service)
  irnoti-asterisk status    [--config FILE]
  irnoti-asterisk rollback  [--config FILE]            (restore the files saved before the last change)
  irnoti-asterisk uninstall [--config FILE] [--purge]
  irnoti-asterisk version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version.Version)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "install":
		err = cmdInstall(ctx, log, os.Args[2:])
	case "run":
		err = cmdRun(ctx, log, os.Args[2:])
	case "status":
		err = cmdStatus(ctx, os.Args[2:])
	case "rollback":
		err = cmdRollback(ctx, log, os.Args[2:])
	case "uninstall":
		err = cmdUninstall(ctx, log, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version.Version)
	case "help", "--help", "-h":
		fmt.Printf(usage, version.Version)
	default:
		fmt.Fprintf(os.Stderr, usage, version.Version)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (config.Config, error) {
	c, err := config.Load(path)
	if err != nil {
		return c, fmt.Errorf("cannot read %s (run `irnoti-asterisk install` first): %w", path, err)
	}
	return c, nil
}

func newInstaller(c config.Config, path string, log *slog.Logger) *installer.Installer {
	in := &installer.Installer{Cfg: c, ConfigPath: path, Runner: installer.ExecRunner{}, Log: log}
	in.Verify = func(ctx context.Context) error {
		// Asterisk needs a moment after the reload before ARI answers
		var last error
		for i := 0; i < 20; i++ {
			if _, last = ari.New(in.Cfg.ARIURL, in.Cfg.ARIUser, in.Cfg.ARIPassword).Info(ctx); last == nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timeAfterSecond():
			}
		}
		return last
	}
	return in
}

func cmdInstall(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	apiURL := fs.String("api", "", "base URL of the IRNoti API, e.g. https://api.irnoti.com")
	token := fs.String("token", os.Getenv("IRNOTI_AGENT_TOKEN"), "agent token from the panel (or IRNOTI_AGENT_TOKEN)")
	dir := fs.String("asterisk-dir", config.DefaultAsteriskDir, "Asterisk configuration directory")
	extFile := fs.String("extensions-file", "", "dialplan file that gets the include (default: extensions_custom.conf if it exists, else extensions.conf)")
	cfgPath := fs.String("config", config.DefaultPath, "where to save the agent settings")
	noService := fs.Bool("no-service", false, "do not install/start the systemd service")
	dry := fs.Bool("dry-run", false, "show what would change without touching anything")
	_ = fs.Parse(args)
	if *apiURL == "" || *token == "" {
		return errors.New("--api and --token are required")
	}
	if !strings.HasPrefix(*apiURL, "https://") && !strings.HasPrefix(*apiURL, "http://localhost") && !strings.HasPrefix(*apiURL, "http://127.0.0.1") {
		return errors.New("--api must be an https:// URL")
	}
	// keep the ARI password across re-installs so a running Asterisk and the agent stay in step
	c := config.Config{}
	if old, err := config.Load(*cfgPath); err == nil {
		c = old
	}
	c.APIURL, c.Token, c.AsteriskDir, c.ExtensionsFile = *apiURL, *token, *dir, *extFile
	c = c.WithDefaults()

	client := api.New(c.APIURL, c.Token)
	if _, err := client.Hello(ctx, api.Hello{Version: version.Version, Hostname: hostname(), OS: "linux", Arch: goarch()}); err != nil {
		return fmt.Errorf("the API did not accept the token: %w", err)
	}
	bundle, err := client.Config(ctx)
	if err != nil {
		return fmt.Errorf("cannot fetch the configuration: %w", err)
	}
	in := newInstaller(c, *cfgPath, log)
	in.DryRun, in.NoService = *dry, *noService
	if err := in.Install(ctx, bundle); err != nil {
		return err
	}
	if *dry {
		fmt.Println("dry run: nothing was changed")
		return nil
	}
	fmt.Println("installed. Point your trunk's inbound context at irnoti-inbound to hand calls to the platform.")
	return nil
}

func cmdRun(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "agent settings file")
	_ = fs.Parse(args)
	c, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	in := newInstaller(c, *cfgPath, log)
	client := api.New(c.APIURL, c.Token)
	a := &tunnel.Agent{
		API: client, ARI: ari.New(c.ARIURL, c.ARIUser, c.ARIPassword), App: c.App, Log: log,
		Sync: in.Sync, Sounds: client, SoundDir: c.SoundsDir,
	}
	log.Info("irnoti-asterisk started", "version", version.Version)
	return a.Run(ctx)
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "agent settings file")
	_ = fs.Parse(args)
	c, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	ver, ariErr := ari.New(c.ARIURL, c.ARIUser, c.ARIPassword).Info(ctx)
	fmt.Printf("agent      %s\napi        %s\n", version.Version, c.APIURL)
	if ariErr != nil {
		fmt.Printf("asterisk   unreachable: %v\n", ariErr)
	} else {
		fmt.Printf("asterisk   %s (ARI ok at %s)\n", ver, c.ARIURL)
	}
	if _, err := api.New(c.APIURL, c.Token).Heartbeat(ctx, api.Heartbeat{AriUp: ariErr == nil}); err != nil {
		fmt.Printf("platform   unreachable: %v\n", err)
		return err
	}
	fmt.Println("platform   ok")
	return nil
}

func cmdRollback(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "agent settings file")
	_ = fs.Parse(args)
	c, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	return newInstaller(c, *cfgPath, log).Rollback(ctx)
}

func cmdUninstall(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "agent settings file")
	purge := fs.Bool("purge", false, "also delete the agent settings file (token and ARI password)")
	_ = fs.Parse(args)
	c, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	return newInstaller(c, *cfgPath, log).Uninstall(ctx, *purge)
}
