package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/gussetcheck"
)

func gussetCheck(out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := gussetcheck.SelfTest(ctx)
	_, _ = gussetcheck.DrainLogs(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "gusset-check: ok (parity, match-any, panic firewall)")
	return nil
}
