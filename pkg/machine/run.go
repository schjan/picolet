package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
)

// runPhases are the phases a run carries out. The per-Host bootstrap, which
// starts the Agents, is not run yet.
var runPhases = []Phase{PhaseSetup, PhaseCredentials}

// outcome is what a run did with a step.
type outcome string

const (
	outcomeApplied outcome = "applied"
	outcomeDone    outcome = "already done"
	outcomeFailed  outcome = "failed"
	outcomeSkipped outcome = "skipped"
)

// Run bootstraps cfg.Machine as root: every step of runPhases, in plan order,
// is checked through the read side of ops and, when its end state does not
// hold, applied through the write side. A run on a bootstrapped Machine
// applies nothing; an interrupted one resumes where it stopped. A failing
// step stops its Host, skipping the Host's later steps, while the other Hosts
// go on; Run then returns the error of every stopped Host. It fails before
// the first step when the Machine is not fit for a run.
func Run(ctx context.Context, cfg Config, ops HostOps) error {
	if err := cfg.Env.checkRun(); err != nil {
		return err
	}
	plan, err := load(cfg)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(cfg.Stdout, 0, 0, 2, ' ', 0)
	writeHeader(tw, plan)
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing run: %w", err)
	}

	r := newRunner(plan, ops, cfg.Stdout)
	for i, phase := range runPhases {
		fmt.Fprintf(r.w, "\nPhase %d: %s\n", i+1, phase)
		for _, step := range plan.Steps {
			if step.Phase != phase {
				continue
			}
			if err := r.step(ctx, step); err != nil {
				return errors.Join(append(r.errs, err)...)
			}
		}
	}

	var summary []string
	for _, o := range []outcome{outcomeApplied, outcomeDone, outcomeFailed, outcomeSkipped} {
		if r.counts[o] > 0 {
			summary = append(summary, fmt.Sprintf("%d %s", r.counts[o], o))
		}
	}
	fmt.Fprintf(r.w, "\n%s\nNo Agent started: the per-Host bootstrap is not run yet.\n", strings.Join(summary, ", "))
	return errors.Join(r.errs...)
}

// runner carries a run's state across steps.
type runner struct {
	ops HostOps
	w   io.Writer
	// idWidth and describeWidth align the step columns.
	idWidth, describeWidth int

	facts map[string]*hostFacts
	// stopped maps a stopped Host to the id of the step that failed.
	stopped map[string]string
	errs    []error
	counts  map[outcome]int
}

// outcomeWidth is the width of the outcome column: its longest label.
var outcomeWidth = len(outcomeDone)

func newRunner(plan *Plan, ops HostOps, w io.Writer) *runner {
	r := &runner{ops: ops, w: w, facts: map[string]*hostFacts{}, stopped: map[string]string{}, counts: map[outcome]int{}}
	for _, s := range plan.Steps {
		if slices.Contains(runPhases, s.Phase) {
			r.idWidth = max(r.idWidth, len(s.ID))
			r.describeWidth = max(r.describeWidth, len(s.Describe()))
		}
	}
	return r
}

// step checks s and applies it when its end state does not hold. Only an
// interruption is an error; a failing step stops its Host.
func (r *runner) step(ctx context.Context, s Step) error {
	host := s.Host.Hostname
	if at, stopped := r.stopped[host]; stopped {
		r.print(outcomeSkipped, s, host+" stopped at "+at)
		return nil
	}
	f := factsOf(r.facts, s.Host, r.ops)
	res, err := f.check(ctx, s)
	if err == nil && res.Status == StatusDone {
		r.print(outcomeDone, s, res.Detail)
		return nil
	}
	if err == nil {
		err = r.apply(ctx, f, s)
		f.forget()
	}
	if ctx.Err() != nil {
		return fmt.Errorf("interrupted at %s: %w", s.ID, ctx.Err())
	}
	if err != nil {
		r.print(outcomeFailed, s, err.Error())
		r.stopped[host] = s.ID
		r.errs = append(r.errs, fmt.Errorf("%s: %w", s.ID, err))
		return nil
	}
	r.print(outcomeApplied, s, "")
	return nil
}

// apply brings s's end state about.
func (r *runner) apply(ctx context.Context, f *hostFacts, s Step) error {
	switch {
	case s.Kind == StepUser:
		return r.ops.CreateUser(ctx, s.Host.User)
	case s.Kind == StepCheckout:
		return r.ops.MakeWorldReadable(s.Path)
	case s.Host.Rootful():
		return r.applyRootful(ctx, s)
	}
	return r.applyUser(ctx, f, s)
}

func (r *runner) applyRootful(ctx context.Context, s Step) error {
	switch s.Kind {
	case StepPodmanSocket:
		return r.ops.EnableSystemUnit(ctx, podmanSocket)
	case StepDir:
		return r.ops.EnsureDir("/", strings.TrimPrefix(s.Path, "/"), Owner{}, s.Mode)
	}
	return fmt.Errorf("step kind %d cannot be applied", s.Kind)
}

// applyUser applies a step of the Host's user, which must exist by now.
func (r *runner) applyUser(ctx context.Context, f *hostFacts, s Step) error {
	user, err := f.existingUser()
	if err != nil {
		return err
	}
	switch s.Kind {
	case StepSubIDs:
		subuid, subgid, err := r.ops.SubIDRanges(user)
		if err != nil {
			return err
		}
		return missingSubIDs(user.Name, subuid, subgid)
	case StepLinger:
		return r.ops.EnableLinger(ctx, user)
	case StepUserManager:
		return r.ops.WaitUserManager(ctx, user)
	case StepPodmanSocket:
		return r.ops.EnableUserUnit(ctx, user, podmanSocket)
	case StepDir:
		return r.ops.EnsureDir(user.Home, s.Path, user.Owner(), s.Mode)
	}
	return fmt.Errorf("step kind %d cannot be applied", s.Kind)
}

func (r *runner) print(o outcome, s Step, detail string) {
	r.counts[o]++
	cols := fmt.Sprintf("  %-*s  %-*s  ", outcomeWidth, o, r.idWidth, s.ID)
	if detail == "" {
		fmt.Fprintln(r.w, cols+s.Describe())
		return
	}
	fmt.Fprintf(r.w, "%s%-*s  %s\n", cols, r.describeWidth, s.Describe(), detail)
}
