// ftw-history-endpoints repairs completed historical minutes from bounding
// power means. Preparing and applying leave equipment control to the live Core.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/srcfl/ftw/go/internal/state"
	"io"
	"os"
	"time"
)

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("use prepare or apply")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	source := f.String("state", "", "existing state.db")
	planFile := f.String("plan", "", "reviewable plan JSON")
	switch args[0] {
	case "prepare":
		since := f.String("since", "", "RFC3339 start")
		until := f.String("until", "", "RFC3339 completed end")
		gap := f.Duration("max-gap", 15*time.Minute, "maximum distance between valid endpoints (1–30m)")
		reason := f.String("reason", "", "audit reason")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *source == "" || *planFile == "" || f.NArg() != 0 {
			return errors.New("state and new plan path required")
		}
		from, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			return err
		}
		to, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			return err
		}
		p, err := state.PrepareEndpointRepair(ctx, *source, state.EndpointRepairRequest{SinceMS: from.UnixMilli(), UntilMS: to.UnixMilli(), MaxGapMS: gap.Milliseconds(), Reason: *reason})
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return err
		}
		file, err := os.OpenFile(*planFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = file.Write(append(b, '\n'))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return json.NewEncoder(out).Encode(map[string]any{"plan_sha256": p.SHA256, "correction_minutes": len(p.Minutes), "skipped": p.Skipped})
	case "apply":
		sha := f.String("sha256", "", "exact reviewed plan digest")
		backup := f.String("backup", "", "independent verified history.db backup")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *source == "" || *planFile == "" || *backup == "" || f.NArg() != 0 {
			return errors.New("state, plan and backup required")
		}
		b, err := os.ReadFile(*planFile)
		if err != nil {
			return err
		}
		var p state.EndpointRepairPlan
		if err = json.Unmarshal(b, &p); err != nil {
			return err
		}
		n, err := state.ApplyEndpointRepair(ctx, *source, *backup, p, *sha)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"plan_sha256": p.SHA256, "applied_minutes": n, "method": "endpoint_mean_v1"})
	default:
		return errors.New("unknown operation")
	}
}
func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
