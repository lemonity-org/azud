package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/adriancarayol/azud/pkg/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of Azud",
	Long:  `Print the version number, build information, and Go runtime version.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("Azud %s\n", version.Version)
		fmt.Printf("  Commit: %s\n", version.Commit)
		fmt.Printf("  Built:  %s\n", version.BuildDate)
		fmt.Printf("  Go:     %s\n", runtime.Version())
		fmt.Printf("  OS/Arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	},
}
