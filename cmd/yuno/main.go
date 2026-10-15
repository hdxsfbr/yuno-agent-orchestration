// Command yuno is the single binary behind every service in compose:
//
//	yuno api            HTTP API, run engine, scheduler, OpenClaw client, MCP server, model proxy
//	yuno migrate        apply database migrations and exit
//	yuno seed           load workflow templates and demo agents
//	yuno openclaw-init  write the baseline openclaw.json into the state directory and exit
//	yuno deploytarget   local release store the deploy tool publishes to
//	yuno eval           run the evaluation suite and write a report
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
	"yuno/internal/httpapi"
	"yuno/internal/obs"
	"yuno/internal/openclaw"
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
		}
	case "deploytarget":
		err = deploytarget.Run(ctx, cfg, log)
	case "seed":
		err = errors.New("seed: not implemented yet")
	case "eval":
		err = errors.New("eval: not implemented yet")
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
	fmt.Fprintln(os.Stderr, "usage: yuno <api|migrate|seed|openclaw-init|deploytarget|eval|version>")
}
