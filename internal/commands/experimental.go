package commands

import (
	"strings"

	"github.com/spf13/cobra"
)

func NewExperimentalCommand(rt Runtime, debug *bool, noCache *bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "experimental",
		Short: "Experimental commands with known sync bugs",
		Long: strings.TrimSpace(`
Experimental commands with known sync bugs.

Do not use these commands on production data.

For agent workflows, prefer the manual and safer sequence:
1. Copy locally to the new name/path.
2. Upload the new file with "ob1 put <new-path>".
3. Remove the old remote file with "ob1 rm <old-path>".
`),
	}

	cmd.AddCommand(NewMoveCommand(rt, debug, noCache))

	return cmd
}
