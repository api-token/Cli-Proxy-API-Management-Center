// Command deriver derives request_facts (and the messages/request_summary
// views) from the raw tables written by the clickhouse-sink plugin.
//
//	deriver            loop: one pass every --interval
//	deriver --rebuild  truncate request_facts, derive everything, exit
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	rebuild := flag.Bool("rebuild", false, "truncate request_facts, derive all raw rows, then exit (stop the loop service first)")
	interval := flag.Duration("interval", 30*time.Second, "time between passes")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ch := newClickHouse(env("CLICKHOUSE_URL", "http://clickhouse:8123"), os.Getenv("CLICKHOUSE_USER"), os.Getenv("CLICKHOUSE_PASSWORD"))
	d := newDeriver(ch, env("CLICKHOUSE_DB", "cliproxy"), log)

	if err := waitSetup(ctx, d, *interval); err != nil {
		log.Error("setup aborted", "err", err)
		os.Exit(1)
	}

	if *rebuild {
		start := time.Now()
		n, err := d.rebuild(ctx)
		if err != nil {
			log.Error("rebuild failed", "rows", n, "err", err)
			os.Exit(1)
		}
		log.Info("rebuild done", "rows", n, "took", time.Since(start).Round(time.Millisecond))
		return
	}

	log.Info("deriver running", "db", d.db, "interval", interval.String())
	for {
		if _, err := d.pass(ctx); err != nil && ctx.Err() == nil {
			log.Error("pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			log.Info("deriver stopped")
			return
		case <-time.After(*interval):
		}
	}
}

// waitSetup creates the derived schema, retrying until the raw schema exists
// (the views need the plugin's requests table and usage_by_request view).
func waitSetup(ctx context.Context, d *deriver, retry time.Duration) error {
	for {
		err := d.setup(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var chErr *chError
		detail := err.Error()
		if errors.As(err, &chErr) {
			detail = chErr.detail()
		}
		d.log.Warn("schema setup failed, retrying", "err", detail, "retry_in", retry.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retry):
		}
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
