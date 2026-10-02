package machine

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Render prints the evaluated plan phase by phase, in execution order, and a
// count per status.
func Render(w io.Writer, plan *Plan, results []Result) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	writeHeader(tw, plan)

	counts := map[Status]int{}
	for i, phase := range Phases {
		fmt.Fprintf(tw, "\nPhase %d: %s\n", i+1, phase)
		if !hasPhase(plan, phase) {
			fmt.Fprintln(tw, nothingToDo)
		}
		for _, r := range results {
			if r.Step.Phase != phase {
				continue
			}
			counts[r.Status]++
			line := fmt.Sprintf("  %s\t%s\t%s", r.Status, r.Step.ID, r.Step.Describe())
			if r.Detail != "" {
				line += "\t" + r.Detail
			}
			fmt.Fprintln(tw, line)
		}
	}

	var summary []string
	for _, s := range []Status{StatusWouldDo, StatusDone, StatusUnknown} {
		if counts[s] > 0 {
			summary = append(summary, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	fmt.Fprintf(tw, "\n%s\n", strings.Join(summary, ", "))
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing plan: %w", err)
	}
	return nil
}

// writeHeader prints the Machine's Hosts, the Fleet checkout, the credential
// source and the plan's warnings.
func writeHeader(tw *tabwriter.Writer, plan *Plan) {
	fmt.Fprintf(tw, "Machine %s: %d Hosts\n", plan.Machine, len(plan.Hosts))
	for _, h := range plan.Hosts {
		runsAs := "rootful"
		if !h.Rootful() {
			runsAs = "user " + h.User
		}
		fmt.Fprintf(tw, "  %s\t%s\tlisten port %d\n", h.Hostname, runsAs, h.ListenPort)
	}
	fmt.Fprintf(tw, "Fleet checkout: %s\n", plan.RepoDir)
	if plan.SecretsDir == "" {
		fmt.Fprintln(tw, "Credential files: none (no --secrets-dir)")
	} else {
		fmt.Fprintf(tw, "Credential files: --secrets-dir %s\n", plan.SecretsDir)
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(tw, "warning: %s\n", w)
	}
}
