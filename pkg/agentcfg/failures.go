package agentcfg

import "strings"

// Failures is the error of checks that report every failure they find
// (Load's credential overlap check, CheckStartup): one error per failure,
// each naming its setting and path. errors.Is and errors.As see each one.
// Its text is one line, since it reaches dashboard events, MQTT and
// deployment statuses.
type Failures []error

func (f Failures) Error() string {
	msgs := make([]string, len(f))
	for i, err := range f {
		msgs[i] = err.Error()
	}
	return strings.Join(msgs, "; ")
}

func (f Failures) Unwrap() []error { return f }

// joinFailures is the Failures of the non-nil errs, nil when there are none.
func joinFailures(errs []error) error {
	var f Failures
	for _, err := range errs {
		if err != nil {
			f = append(f, err)
		}
	}
	if len(f) == 0 {
		return nil
	}
	return f
}
