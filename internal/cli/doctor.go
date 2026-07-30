package cli

import (
	"errors"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/daropotter/chmura/internal/doctor"
)

// errDoctorProblems makes a doctor run with a failing check exit non-zero
// (execution error) without adding a separate message path.
var errDoctorProblems = errors.New("doctor found problems")

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the local environment for problems",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			results := doctor.Run([]doctor.Check{
				doctor.ContainerRuntime(exec.LookPath),
			})
			doctor.Report(cmd.OutOrStdout(), results)
			if doctor.ExitCode(results) != 0 {
				return errDoctorProblems
			}
			return nil
		},
	}
}
