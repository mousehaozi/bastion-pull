package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is injected at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfgPath := flag.String("c", "config.yaml", "path to the config file")
	once := flag.Bool("once", false, "check every project once, then exit (non-zero exit code on failure)")
	showVersion := flag.Bool("version", false, "print version and exit")
	notifyTest := flag.Bool("notify-test", false, "send a test notification to every configured target, then exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("bastion-pull", version)
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}

	if *notifyTest {
		if cfg.Notify.PushPlus == nil {
			fmt.Fprintln(os.Stderr, "no notify.pushplus section in the config")
			os.Exit(2)
		}
		if err := cfg.Notify.PushPlus.sendTest(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "notify test failed:", err)
			os.Exit(1)
		}
		fmt.Println("test notification sent")
		return
	}

	unlock, err := lockFile(cfg.StateFile + ".lock")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start:", err)
		os.Exit(1)
	}
	defer unlock()

	app, err := newApp(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("bastion-pull started", "version", version, "projects", len(cfg.Projects), "interval", cfg.Interval)

	if *once {
		if failed := app.checkAll(ctx); failed > 0 {
			os.Exit(1)
		}
		return
	}

	app.checkAll(ctx)
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return
		case <-ticker.C:
			app.checkAll(ctx)
		}
	}
}

// lockFile takes an exclusive lock so two instances never deploy at the same time.
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another bastion-pull instance is already running (lock %s)", path)
	}
	return func() { f.Close() }, nil
}
