// ftw-history-repair prepares and applies historical household corrections on
// a separate database copy. It never writes the owner's source databases.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("use prepare --state PATH --workdir NEW_DIR --since RFC3339 --until RFC3339 --grid DRIVER:METRIC --pv DRIVER:METRIC|none --battery DRIVER:AC_METRIC|none --ev DRIVER:METRIC|none --v2x DRIVER:METRIC|none --reason TEXT [--max-gap 15s], then apply --workdir DIR --sha256 REVIEWED_PLAN_HASH")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	workdir := f.String("workdir", "", "new private working directory for prepare; same directory for apply")
	if args[0] == "apply" {
		sha := f.String("sha256", "", "SHA-256 of the exact reviewed plan")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *workdir == "" || f.NArg() != 0 {
			return errors.New("workdir is required; no positional arguments")
		}
		result, err := state.ApplyHistoryRepair(ctx, *workdir, *sha)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(result)
	}
	if args[0] != "prepare" {
		return errors.New("unknown repair operation")
	}
	source := f.String("state", "", "source state.db; opened read-only")
	since := f.String("since", "", "start RFC3339 with timezone, on a minute boundary")
	until := f.String("until", "", "end RFC3339 with timezone, on a completed minute boundary")
	reason := f.String("reason", "", "reason for this historical estimate")
	gap := f.Duration("max-gap", 15*time.Second, "largest source-point gap to interpolate; up to 180s, explicitly reviewed")
	flows := map[string]*string{}
	for _, name := range []string{"grid", "pv", "battery", "ev", "v2x"} {
		flows[name] = f.String(name, "", "explicit driver:power_metric or none; battery must be site-side AC")
	}
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if *source == "" || *workdir == "" || f.NArg() != 0 {
		return errors.New("state and workdir required; no positional arguments")
	}
	start, err := time.Parse(time.RFC3339, *since)
	if err != nil {
		return err
	}
	end, err := time.Parse(time.RFC3339, *until)
	if err != nil {
		return err
	}
	r := state.HistoryRepairRequest{SinceMS: start.UnixMilli(), UntilMS: end.UnixMilli(), MaxGapMS: gap.Milliseconds(), Reason: *reason}
	for name, value := range flows {
		v := state.RepairPowerSource{Flow: name}
		if *value == "none" {
			v.Absent = true
		} else {
			parts := strings.SplitN(*value, ":", 2)
			if len(parts) != 2 {
				return fmt.Errorf("%s needs driver:power_metric or explicit none", name)
			}
			v.Driver, v.Metric = parts[0], parts[1]
		}
		r.Sources = append(r.Sources, v)
	}
	state.SortRepairSources(r.Sources)
	plan, err := state.PrepareHistoryRepair(ctx, *source, *workdir, r)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(plan)
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
