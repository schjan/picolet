package machine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	// AgentRestart: applying the step writes a file the Host's running
	// Agent has not read, so the Agent needs a restart to see it.
	AgentRestart bool
	// OwnerModeOnly: the content already holds, applying the step only sets
	// owner and mode.
	OwnerModeOnly bool
}

// Evaluate runs every step's check through the read side of ops, in plan
// order. It writes nothing. A check the invoking user cannot perform yields
// StatusUnknown; any other check error aborts the evaluation.
func Evaluate(ctx context.Context, plan *Plan, ops HostOps) ([]Result, error) {
	facts := map[string]*hostFacts{}
	results := make([]Result, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		f := factsOf(facts, step.Host, ops)
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

// factsOf returns the facts of h in facts, created on first use.
func factsOf(facts map[string]*hostFacts, h Host, ops HostOps) *hostFacts {
	f, ok := facts[h.Hostname]
	if !ok {
		f = &hostFacts{host: h, ops: ops}
		facts[h.Hostname] = f
	}
	return f
}

// forget drops the cached facts: a step was applied and may have changed them.
func (f *hostFacts) forget() {
	f.userChecked, f.managerChecked = false, false
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

// existingUser is the Host's user, which a user-level step needs to exist.
func (f *hostFacts) existingUser() (User, error) {
	user, found, err := f.lookupUser()
	if err == nil && !found {
		err = fmt.Errorf("user %s does not exist", f.host.User)
	}
	return user, err
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
		return f.checkPodmanSocket(ctx)
	case StepDir:
		return f.checkDir(s)
	case StepCheckout:
		return boolResult(f.ops.WorldReadableTree(s.Path))("", "not readable by every user")
	case StepCredential:
		return f.checkCredential(s)
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
	if err != nil {
		return Result{}, err
	}
	if err := missingSubIDs(user.Name, subuid, subgid); err != nil {
		return Result{Status: StatusWouldDo, Detail: "a run stops this Host: " + err.Error()}, nil
	}
	return Result{Status: StatusDone}, nil
}

// missingSubIDs is the error of a user without a subuid or subgid range,
// with the command that adds them; nil when both exist. Bootstrap verifies
// ranges and never allocates them: the operator adds them.
func missingSubIDs(user string, subuid, subgid bool) error {
	var missing []string
	if !subuid {
		missing = append(missing, "subuid")
	}
	if !subgid {
		missing = append(missing, "subgid")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("no %s range; add one: usermod --add-subuids 100000-165535 --add-subgids 100000-165535 %s",
		strings.Join(missing, "/"), user)
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

func (f *hostFacts) checkPodmanSocket(ctx context.Context) (Result, error) {
	if f.host.Rootful() {
		return unitResult(f.ops.SystemUnitState(ctx, podmanSocket))
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
	return unitResult(f.ops.UserUnitState(ctx, f.user, podmanSocket))
}

// unitResult is done for a unit both enabled and active: enable --now
// brings about both, and an enabled unit that is not running would leave
// the Agent without its socket until the next boot.
func unitResult(state UnitState, err error) (Result, error) {
	switch {
	case err != nil:
		return Result{}, err
	case state.Enabled && state.Active:
		return Result{Status: StatusDone}, nil
	case state.Enabled:
		return Result{Status: StatusWouldDo, Detail: "enabled, not running"}, nil
	case state.Active:
		return Result{Status: StatusWouldDo, Detail: "running, not enabled"}, nil
	}
	return Result{Status: StatusWouldDo}, nil
}

// base is the directory the Host's step paths are below, the user's home or
// / for the rootful Host, and the owner what it places gets; ok is false
// when the Host's user, and so its home, does not exist yet.
func (f *hostFacts) base() (dir string, owner Owner, ok bool, err error) {
	if f.host.Rootful() {
		return "/", Owner{}, true, nil
	}
	user, found, err := f.lookupUser()
	if err != nil || !found {
		return "", Owner{}, false, err
	}
	return user.Home, user.Owner(), true, nil
}

// existingBase is base for a step that needs the Host's user to exist.
func (f *hostFacts) existingBase() (string, Owner, error) {
	dir, owner, ok, err := f.base()
	if err == nil && !ok {
		err = fmt.Errorf("user %s does not exist", f.host.User)
	}
	return dir, owner, err
}

// onMachine is the absolute path of s.Path and the owner it should have; ok
// as for base.
func (f *hostFacts) onMachine(s Step) (p string, owner Owner, ok bool, err error) {
	dir, owner, ok, err := f.base()
	return path.Join(dir, s.Path), owner, ok, err
}

func (f *hostFacts) checkDir(s Step) (Result, error) {
	dir, owner, ok, err := f.onMachine(s)
	if err != nil || !ok {
		return createdLater, err
	}
	info, err := f.ops.Stat(dir)
	switch {
	case err != nil:
		return Result{}, err
	case !info.Exists:
		return Result{Status: StatusWouldDo, Detail: "missing"}, nil
	case !info.Mode.IsDir():
		return Result{Status: StatusWouldDo, Detail: "exists and is not a directory"}, nil
	}
	return ownedResult(info, owner, s.Mode), nil
}

// checkCredential compares the file with the step's content without ever
// showing either. Writing the content, new or changed, needs an Agent
// restart; a file with current content but another owner or mode gets them
// in place.
func (f *hostFacts) checkCredential(s Step) (Result, error) {
	file, owner, ok, err := f.onMachine(s)
	if err != nil || !ok {
		return createdLater, err
	}
	info, err := f.ops.Stat(file)
	switch {
	case err != nil:
		return Result{}, err
	case !info.Exists:
		return credentialWrite("missing"), nil
	case !info.Mode.IsRegular():
		return credentialWrite("exists and is not a regular file"), nil
	}
	same, err := f.ops.FileContentEquals(file, s.Content)
	switch {
	case err != nil:
		return Result{}, err
	case !same:
		return credentialWrite("content differs"), nil
	}
	r := ownedResult(info, owner, s.Mode)
	r.OwnerModeOnly = r.Status == StatusWouldDo
	return r, nil
}

// credentialWrite is the result of a credential file whose content a run
// writes.
func credentialWrite(detail string) Result {
	return Result{Status: StatusWouldDo, Detail: detail + ", Agent restart required", AgentRestart: true}
}

// ownedResult is done for an existing path with owner and permission mode.
func ownedResult(info PathInfo, owner Owner, mode fs.FileMode) Result {
	if info.UID != owner.UID || info.GID != owner.GID || info.Mode.Perm() != mode {
		return Result{Status: StatusWouldDo, Detail: fmt.Sprintf("is %d:%d %04o", info.UID, info.GID, info.Mode.Perm())}
	}
	return Result{Status: StatusDone}
}
