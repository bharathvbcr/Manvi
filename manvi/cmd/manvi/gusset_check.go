package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/gussetcheck"
)

func gussetCheck(out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := gussetcheck.Run(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "gusset-check: ok")
	return nil
}
