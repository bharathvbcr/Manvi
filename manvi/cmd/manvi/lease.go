package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/store"
)

// --- lease ---

func lease(out io.Writer, args []string) error {
	if len(args) == 0 {
		return errors.New("lease needs a subcommand: list, acquire, release")
	}
	client := storeClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch args[0] {
	case "list":
		leases, err := client.ActiveLeases(ctx)
		if err != nil {
			return err
		}
		if len(leases) == 0 {
			_, _ = fmt.Fprintln(out, "no active leases")
			return nil
		}
		for _, l := range leases {
			_, _ = fmt.Fprintf(out, "%-14s %-16s expires %s\n", l.TaskID, l.Owner, l.ExpiresAt)
		}
		return nil

	case "acquire":
		if len(args) < 3 {
			return errors.New("usage: manvi lease acquire TASK OWNER [--ttl 15m]")
		}
		ttl := 15 * time.Minute
		if raw := flagValue(args, "--ttl"); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("--ttl %q: %w", raw, err)
			}
			ttl = parsed
		}
		l, err := client.Acquire(ctx, store.AcquireRequest{
			TaskID: args[1], Owner: args[2], TTL: ttl,
		})
		var conflict *store.Conflict
		if errors.As(err, &conflict) {
			// Contention is routable information, not a crash.
			_, _ = fmt.Fprintf(out, "held by %s — take another task (%s)\n", conflict.Holder, conflict.Code())
			return nil
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "acquired %s\ntoken %s\nexpires %s\n", l.TaskID, l.Token, l.ExpiresAt)
		return nil

	case "release":
		if len(args) < 3 {
			return errors.New("usage: manvi lease release TASK TOKEN")
		}
		released, err := client.Release(ctx, args[1], args[2])
		if err != nil {
			return err
		}
		if !released {
			_, _ = fmt.Fprintln(out, "nothing to release")
			return nil
		}
		_, _ = fmt.Fprintln(out, "released")
		return nil
	}
	return fmt.Errorf("unknown lease subcommand %q", args[0])
}
