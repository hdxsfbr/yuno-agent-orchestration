// Command yuno is the single binary behind every service in compose:
//
//	yuno api            HTTP API, run engine, scheduler, OpenClaw client, MCP server, model proxy
//	yuno migrate        apply database migrations and exit
//	yuno seed           create the demo agents and the Chief of Staff, instantiate the
//	                    shipped templates once, bind the chief to Telegram when unbound (idempotent);
//	                    --reset-shipped also restores the shipped agents' prompts and allowlists
//	                    and the instantiated templates to the current shipped versions
//	yuno openclaw-init  write the baseline openclaw.json into the state directory, create its
//	                    sandboxes/ folder, and exit
//	yuno deploytarget   local release store the deploy tool publishes to
//	yuno eval           run the evaluation suite and write a report
//	yuno forgefake      the fake forge the evals run against (FORGEFAKE_TOKEN, FORGEFAKE_ADDR;
//	                    compose profile eval only, DESIGN 5.29 rule 17)
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"yuno/internal/config"
	"yuno/internal/deploytarget"
	"yuno/internal/evals"
	"yuno/internal/forgefake"
	"yuno/internal/httpapi"
	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/seed"
	"yuno/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := config.FromEnv()
	log := obs.NewLogger(cfg.LogLevel)

	var err error
	switch os.Args[1] {
	case "api":
		err = httpapi.Run(ctx, cfg, log)
	case "migrate":
		err = store.MigrateURL(ctx, cfg.DatabaseURL, log)
	case "openclaw-init":
		var changed bool
		changed, err = openclaw.WriteBaseline(cfg.OpenClawStateDir, openclaw.BaselineInputs{
			GatewayToken:           cfg.OpenClawGatewayToken,
			MCPToken:               cfg.MCPToken,
			StateDir:               cfg.OpenClawStateDir,
			APIBase:                cfg.APIBaseForOpenClaw,
			DefaultModel:           cfg.DefaultModel,
			SandboxImage:           cfg.OpenClawSandboxImage,
			TelegramMode:           cfg.TelegramMode,
			TelegramWebhookSecret:  cfg.TelegramWebhookSecret,
			TelegramAllowedUserIDs: cfg.TelegramAllowedUserIDs,
			PublicBaseURL:          cfg.PublicBaseURL,
		})
		if err == nil {
			log.Info("openclaw baseline config", "state_dir", cfg.OpenClawStateDir, "changed", changed)
			// The sessions' private workspaces live under sandboxes/, which the api writes
			// into before OpenClaw does (DESIGN 5.30 rule 2).
			err = openclaw.EnsureSandboxesDir(cfg.OpenClawStateDir)
		}
	case "deploytarget":
		err = deploytarget.Run(ctx, cfg, log)
	case "seed":
		var opts seed.Options
		for _, arg := range os.Args[2:] {
			switch arg {
			case "--reset-shipped":
				opts.ResetShipped = true
			default:
				fmt.Fprintln(os.Stderr, "usage: yuno seed [--reset-shipped]")
				os.Exit(2)
			}
		}
		err = seed.Run(ctx, cfg, log, opts)
	case "eval":
		err = evals.Run(ctx, cfg, log)
	case "forgefake":
		err = forgefake.Run(ctx, os.Getenv("FORGEFAKE_ADDR"), log)
	case "version":
		fmt.Println(config.Version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exit", "cmd", os.Args[1], "error", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: yuno <api|migrate|seed|openclaw-init|deploytarget|eval|forgefake|version>")
}
