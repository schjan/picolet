package validator

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/schjan/picolet/pkg/sops"
)

// validateSOPSFiles runs the Fleet-wide SOPS passes over every `*.sops.*`
// file in fsys, whether or not a Host references it:
//
//   - Encrypted-file pass: the file must be at most sops.MaxFileSize and
//     carry SOPS metadata (sops.version and sops.mac).
//   - Creation-rule pass: when the Fleet-root `.sops.yaml` has a rule
//     matching the file, the file's age Recipients must equal the rule's.
//   - Audience pass (skipped when aud is nil): every age Recipient of the
//     file must belong to an Operator or to a Host that reaches the file.
//
// None of the passes needs a key.
func validateSOPSFiles(fsys fs.FS, aud *audience) error {
	p := sopsPass{fsys: fsys, audience: aud}
	rules, err := sops.ReadCreationRules(fsys)
	if err != nil {
		p.errs = append(p.errs, err)
	}
	p.rules = rules
	walkErr := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			p.errs = append(p.errs, fmt.Errorf("scanning for SOPS files: %w", err))
			return nil // WalkDir skips an unreadable directory
		case d.IsDir() && path == ".git":
			return fs.SkipDir
		case !d.IsDir() && sops.IsSOPSPath(path):
			p.check(path)
		}
		return nil
	})
	if walkErr != nil {
		p.errs = append(p.errs, fmt.Errorf("scanning for SOPS files: %w", walkErr))
	}
	return errors.Join(p.errs...)
}

// sopsPass collects the errors of the Fleet-wide SOPS passes.
type sopsPass struct {
	fsys fs.FS
	// rules are the root `.sops.yaml`'s; nil skips the creation-rule pass.
	rules *sops.CreationRules
	// audience is the audience pass's input; nil skips the pass.
	audience *audience
	errs     []error
}

// check runs the passes on the SOPS file at path.
func (p *sopsPass) check(path string) {
	f, err := checkSOPSFile(p.fsys, path)
	if err != nil {
		p.errs = append(p.errs, err)
		return
	}
	if p.audience != nil {
		p.errs = append(p.errs, p.audience.check(path, f)...)
	}
	if p.rules == nil {
		return
	}
	rule, ok, err := p.rules.Match(path)
	switch {
	case err != nil:
		p.errs = append(p.errs, err)
		p.rules = nil // an invalid rule would fail every later file the same way
	case ok:
		if err := rule.CheckRecipients(path, f); err != nil {
			p.errs = append(p.errs, err)
		}
	}
}

// checkSOPSFile reads and inspects the SOPS file at path; a file that is too
// large or not encrypted is an error.
func checkSOPSFile(fsys fs.FS, path string) (*sops.File, error) {
	data, err := sops.ReadFile(fsys, path)
	if err != nil {
		return nil, err
	}
	f, err := sops.Inspect(path, data)
	if err != nil {
		return nil, err
	}
	if !f.IsSOPS {
		return nil, fmt.Errorf("`%s` is not SOPS-encrypted (%s): its content is in git in plaintext; "+
			"rotate every value it held, then commit it encrypted (rewriting git history does not unpublish it)",
			path, f.NotSOPSReason)
	}
	return f, nil
}
