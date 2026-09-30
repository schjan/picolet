package machine

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
)

// Status is a step's state as observed on the Machine.
type Status int

const (
	// StatusWouldDo: a run would carry out the step.
	StatusWouldDo Status = iota + 1
	// StatusDone: the step's end state already holds; a run skips it.
	StatusDone
	// StatusUnknown: the check needs root (see ErrUnprivileged).
	StatusUnknown
)

func (s Status) String() string {
	switch s {
	case StatusWouldDo:
		return "would do"
	case StatusDone:
		return "already done"
	case StatusUnknown:
		return "unknown"
	}
	return fmt.Sprintf("status %d", int(s))
}

// Result is a step with its observed Status.
type Result struct {
	Step   Step
	Status Status
	// Detail is what the check found, when it adds to Status.
	Detail string
}

// Evaluate runs every step's check through the read side of ops, in plan
// order. It writes nothing. A check the invoking user cannot perform yields
// StatusUnknown; any other check error aborts the evaluation.
func Evaluate(ctx context.Context, plan *Plan, ops HostOps) ([]Result, error) {
	facts := map[string]*hostFacts{}
	results := make([]Result, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		f, ok := facts[step.Host.Hostname]
		if !ok {
			f = &hostFacts{host: step.Host, ops: ops}
			facts[step.Host.Hostname] = f
		}
		r, err := f.check(ctx, step)
		if errors.Is(err, ErrUnprivileged) {
			r, err = Result{Status: StatusUnknown, Detail: "needs root to check"}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("checking %s: %w", step.ID, err)
		}
		r.Step = step
		results = append(results, r)
	}
	return results, nil
}

// hostFacts caches what several of a Host's checks depend on, so the result
// does not depend on which step asks first.
type hostFacts struct {
	host Host
	ops  HostOps

	userChecked bool
	user        User
	userFound   bool

	managerChecked bool
	manager        Result
	managerErr     error
}

func (f *hostFacts) lookupUser() (User, bool, error) {
	if !f.userChecked {
		user, found, err := f.ops.LookupUser(f.host.User)
		if err != nil {
			return User{}, false, err
		}
		f.user, f.userFound, f.userChecked = user, found, true
	}
	return f.user, f.userFound, nil
}

// createdLater is the result of a check on something of a user that does
// not exist yet: creating the user comes first, so the step would run.
var createdLater = Result{Status: StatusWouldDo, Detail: "user does not exist yet"}

func (f *hostFacts) check(ctx context.Context, s Step) (Result, error) {
	switch s.Kind {
	case StepUser:
		return f.checkUser()
	case StepSubIDs:
		return f.checkSubIDs()
	case StepLinger:
		return f.checkLinger()
	case StepUserManager:
		return f.checkUserManager()
	case StepPodmanSocket:
		return f.checkPodmanSocket(ctx, s.Unit)
	case StepDir:
		return f.checkDir(s)
	case StepCheckout:
		return boolResult(f.ops.WorldReadableTree(s.Path))("", "not readable by every user")
	case StepCredentials:
		return Result{Status: StatusDone, Detail: "no credential source given, nothing to place"}, nil
	case StepHostBootstrap:
		return Result{Status: StatusWouldDo}, nil
	}
	return Result{}, fmt.Errorf("unknown step kind %d", s.Kind)
}

// boolResult maps a yes/no check to StatusDone/StatusWouldDo with the detail
// for each answer.
func boolResult(ok bool, err error) func(doneDetail, todoDetail string) (Result, error) {
	return func(doneDetail, todoDetail string) (Result, error) {
		switch {
		case err != nil:
			return Result{}, err
		case ok:
			return Result{Status: StatusDone, Detail: doneDetail}, nil
		default:
			return Result{Status: StatusWouldDo, Detail: todoDetail}, nil
		}
	}
}

func (f *hostFacts) checkUser() (Result, error) {
	user, found, err := f.lookupUser()
	return boolResult(found, err)(fmt.Sprintf("uid %d", user.UID), "missing")
}

func (f *hostFacts) checkSubIDs() (Result, error) {
	user, found, err := f.lookupUser()
	if err != nil || !found {
		return createdLater, err
	}
	subuid, subgid, err := f.ops.SubIDRanges(user)
	if err != nil || (subuid && subgid) {
		return boolResult(true, err)("", "")
	}
	var missing, flags []string
	if !subuid {
		missing = append(missing, "subuid")
		flags = append(flags, "--add-subuids <first>-<last>")
	}
	if !subgid {
		missing = append(missing, "subgid")
		flags = append(flags, "--add-subgids <first>-<last>")
	}
	// Bootstrap verifies ranges and never allocates them: the run stops this
	// Host here until the operator adds them.
	return Result{Status: StatusWouldDo, Detail: fmt.Sprintf("no %s range, the run stops this Host; add one: usermod %s %s",
		strings.Join(missing, "/"), strings.Join(flags, " "), user.Name)}, nil
}

func (f *hostFacts) checkLinger() (Result, error) {
	user, found, err := f.lookupUser()
	if err != nil || !found {
		return createdLater, err
	}
	return boolResult(f.ops.LingerEnabled(user))("", "")
}

func (f *hostFacts) checkUserManager() (Result, error) {
	if !f.managerChecked {
		f.manager, f.managerErr = f.userManager()
		f.managerChecked = true
	}
	return f.manager, f.managerErr
}

func (f *hostFacts) userManager() (Result, error) {
	user, found, err := f.lookupUser()
	if err != nil || !found {
		return createdLater, err
	}
	return boolResult(f.ops.UserManagerRunning(user))("", "not running")
}

func (f *hostFacts) checkPodmanSocket(ctx context.Context, unit string) (Result, error) {
	if f.host.Rootful() {
		return boolResult(f.ops.SystemUnitEnabled(ctx, unit))("", "")
	}
	manager, err := f.checkUserManager()
	if err != nil || manager.Status != StatusDone {
		// Without a running manager there is nothing to ask; the run
		// enables the socket once the manager is up.
		if manager.Status == StatusWouldDo && f.userFound {
			manager.Detail = "user manager not running"
		}
		return manager, err
	}
	return boolResult(f.ops.UserUnitEnabled(ctx, f.user, unit))("", "")
}

func (f *hostFacts) checkDir(s Step) (Result, error) {
	dir, uid, gid := s.Path, 0, 0
	if !f.host.Rootful() {
		user, found, err := f.lookupUser()
		if err != nil || !found {
			return createdLater, err
		}
		dir, uid, gid = path.Join(user.Home, s.Path), user.UID, user.GID
	}
	info, err := f.ops.Stat(dir)
	switch {
	case err != nil:
		return Result{}, err
	case !info.Exists:
		return Result{Status: StatusWouldDo, Detail: "missing"}, nil
	case !info.Mode.IsDir():
		return Result{Status: StatusWouldDo, Detail: "exists and is not a directory, the run stops this Host"}, nil
	case info.UID != uid || info.GID != gid || info.Mode.Perm() != s.Mode:
		return Result{Status: StatusWouldDo, Detail: fmt.Sprintf("is %d:%d %04o", info.UID, info.GID, info.Mode.Perm())}, nil
	}
	return Result{Status: StatusDone}, nil
}
