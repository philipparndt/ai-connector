package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/voices"
)

const voicesUsage = `usage:
  mlxlab voices list                     the voice library (<files>/voices): cloned and designed voices
  mlxlab voices import <voices.yaml>     adds the cloned voices of a voice set: each a reference clip and its exact
                                         transcript (dir, voices: name → language, reference, transcript); a name
                                         the library has already is left out. Use a voice by its name or id:
                                         mlxlab run fish-s2-pro "…" --voice de-male
`

// voicesCommand is "mlxlab voices": the speech models' voice library from the command line.
func voicesCommand(args []string) error {
	if len(args) == 0 || helpFlag(args) {
		fmt.Fprint(os.Stderr, voicesUsage)
		return nil
	}
	lib := voices.Open(filepath.Join(engine.ConfigDir(), "voices"))
	switch args[0] {
	case "list":
		vs, err := lib.List()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tKIND\tLANGUAGE\tSAYS")
		for _, v := range vs {
			says := v.Transcript
			if v.Kind == "design" {
				says = v.Description
			}
			if r := []rune(says); len(r) > 60 {
				says = string(r[:59]) + "…"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", v.ID, v.Name, v.Kind, v.Language, says)
		}
		return w.Flush()
	case "import":
		if len(args) != 2 {
			fmt.Fprint(os.Stderr, voicesUsage)
			return fmt.Errorf("voices import: one voices.yaml")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		res, err := lib.ImportSet(ctx, args[1])
		for _, v := range res.Added {
			fmt.Printf("added %s (%s, %s)\n", v.Name, v.ID, v.Language)
		}
		names := make([]string, 0, len(res.Skipped))
		for n := range res.Skipped {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("left out %s: %s\n", n, res.Skipped[n])
		}
		return err
	}
	fmt.Fprint(os.Stderr, voicesUsage)
	return fmt.Errorf("voices: unknown command %q", args[0])
}
