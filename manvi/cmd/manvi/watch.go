package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// --- watch ---

// watch renders the harness event stream.
//
// It is here to make the structural point executable rather than only
// documented: the terminal is not a privileged consumer. The same events drive
// the JSON face, so anything visible here is visible to a CI job or an editor
// without a second code path to keep in step.
func watch(out io.Writer, reg *flags.Registry, args []string) (err error) {
	asJSON := slices.Contains(args, "--json")

	scrubber := newArmedScrubber(nil)

	var sink ui.Sink
	if asJSON {
		sink = ui.NewJSONSink(out, scrubber)
	} else {
		sink = ui.NewRenderer(out, scrubber)
	}
	// Every return below reports on what the store said, and every one of those
	// reports leaves through this sink. A line that did not make it out makes
	// the report incomplete, and stdout is where the report went, so the fact
	// has to come back as the status. Deferred rather than repeated at each of
	// the five returns, because a path that forgets it exits 0 on a short
	// answer — which is the failure being closed here, rebuilt one level up.
	defer func() { err = outputStatus(err, os.Stderr, scrubber.Clean, faceFailure(sink)) }()

	posture, _, err := reg.String(flags.HarnessPosture)
	if err != nil {
		return err
	}
	model, _, err := reg.String(flags.LLMDefaultProvider)
	if err != nil {
		return err
	}
	sink.Emit(ui.Event{Kind: ui.KindSessionStart, At: time.Now().UTC(), Posture: posture, Model: model})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := storeClient()
	if err := client.Available(ctx); err != nil {
		sink.Emit(ui.Event{Kind: ui.KindError, At: time.Now().UTC(),
			Text: fmt.Sprintf("store unreachable: %v", err)})
		sink.Emit(ui.Event{Kind: ui.KindNotice, At: time.Now().UTC(),
			Text: "lease checks cannot run; writes that need one will be refused"})
		return nil
	}
	leases, err := client.ActiveLeases(ctx)
	if err != nil {
		return err
	}
	if len(leases) == 0 {
		sink.Emit(ui.Event{Kind: ui.KindNotice, At: time.Now().UTC(), Text: "no active leases"})
	}
	for _, lease := range leases {
		sink.Emit(ui.Event{
			Kind: ui.KindLease, At: time.Now().UTC(), TaskID: lease.TaskID,
			Text: fmt.Sprintf("%s held by %s until %s", lease.TaskID, lease.Owner, lease.ExpiresAt),
		})
	}

	ready, err := client.ReadyTasks(ctx)
	if err != nil {
		return err
	}
	sink.Emit(ui.Event{Kind: ui.KindReport, At: time.Now().UTC(),
		Text: fmt.Sprintf("%d task(s) ready, %d lease(s) active", len(ready), len(leases))})
	return nil
}
