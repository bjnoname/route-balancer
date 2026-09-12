package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bjnoname/route-balancer/internal/actor"
	"github.com/bjnoname/route-balancer/internal/claim"
	"github.com/bjnoname/route-balancer/internal/config"
	"github.com/bjnoname/route-balancer/internal/reconcile"
	"github.com/bjnoname/route-balancer/internal/settings"
)

func main() {
	cfg := config.Default()

	if path := config.ParseFlags(); path != "" {
		c, err := config.Load(path)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		cfg = c
	}

	setupLogging(cfg.LogLevel)

	if os.Geteuid() != 0 {
		log.Fatal("route-balancer must run as root (needs CAP_NET_ADMIN)")
	}

	slog.Info("route-balancer starting")

	if err := run(cfg); err != nil {
		log.Fatalf("%v", err)
	}

	slog.Info("route-balancer cleanup complete")
}

func run(cfg *config.Config) error {

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	set, err := settings.Resolve(cfg)
	if err != nil {
		return err
	}

	events := actor.NewMailbox[reconcile.Event](reconcile.QueueDepth)
	probes := actor.NewMailbox[claim.Probe](claim.QueueDepth)

	rec, err := reconcile.New(ctx, set, events, claim.Offer(probes))
	if err != nil {
		return err
	}
	clm := claim.New(ctx, set.Nd, probes, reconcile.Verdicts(events))

	rec.Start()
	clm.Start()

	rec.Wait()
	stop()
	clm.Wait()

	return nil
}

func setupLogging(level string) {
	lvl := slog.LevelInfo
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: lvl,
	})))
}
