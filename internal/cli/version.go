package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/pkg/version"
)

func newVersionCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "version",
		Short: "Print version and build metadata",
		Long: `Print the version, source revision, build date, Go runtime, and target.

Use --short for a stable, unstyled version value in scripts.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			short, err := cmd.Flags().GetBool("short")
			if err != nil {
				return err
			}
			defer func() {
				flag := cmd.Flags().Lookup("short")
				if flag == nil {
					return
				}
				_ = flag.Value.Set("false")
				flag.Changed = false
			}()

			writer := cmd.OutOrStdout()
			if short {
				_, err = fmt.Fprintln(writer, version.Version)
				return err
			}

			// Keep the first line stable for older integrations. New
			// automation should use --short rather than scrape this view.
			if _, err = fmt.Fprintf(writer, "Azud %s\n", version.Version); err != nil {
				return err
			}
			if _, err = fmt.Fprintf(writer, "  COMMIT  %s\n", version.Commit); err != nil {
				return err
			}
			if _, err = fmt.Fprintf(writer, "  BUILT   %s\n", version.BuildDate); err != nil {
				return err
			}
			if _, err = fmt.Fprintf(writer, "  GO      %s\n", runtime.Version()); err != nil {
				return err
			}
			_, err = fmt.Fprintf(writer, "  TARGET  %s/%s\n", runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
	command.Flags().Bool("short", false, "Print only the version value")
	return command
}

var versionCmd = newVersionCommand()
