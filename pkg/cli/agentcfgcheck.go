package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/schjan/picolet/pkg/agentcfg"
)

// errAgentConfigCheck is the error of a failed agentcfg check; the failures
// themselves are printed.
var errAgentConfigCheck = errors.New("agent config check failed")

// runAgentConfigCheck runs the checks the run command's startup runs
// (loadRunConfig: agentcfg.Load and Config.CheckStartup) on the Agent config
// at configPath, without starting the Agent. It prints one line per failure,
// each naming the setting and path; the checks' errors never quote a
// credential file's content.
func runAgentConfigCheck(w io.Writer, configPath string) error {
	if _, _, err := loadRunConfig(configPath); err != nil {
		_, _ = fmt.Fprintf(w, "%s: FAIL\n", configPath)
		for _, failure := range failures(err) {
			_, _ = fmt.Fprintf(w, "  %s\n", failure)
		}
		return errAgentConfigCheck
	}
	_, _ = fmt.Fprintf(w, "%s: ok\n", configPath)
	return nil
}

// failures lists err's agentcfg.Failures, dropping the wrapping above them
// (the config path, printed once); an err without them is one failure.
func failures(err error) []error {
	var f agentcfg.Failures
	if !errors.As(err, &f) {
		return []error{err}
	}
	return f
}
