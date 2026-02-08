package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/deploy"
	"github.com/lemonity-org/azud/internal/output"
)

var historyCmd = &cobra.Command{
	Use:     "history",
	Aliases: []string{"releases"},
	Short:   "Show deployment history",
	Long: `View deployment history records stored in .azud/history.

Examples:
  azud history list
  azud history list --limit 50
  azud history show deploy_123456789`,
}

var historyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recent deployments",
	Long: `List deployment history for the configured service.

Examples:
  azud history list
  azud history list --limit 50`,
	RunE: runHistoryList,
}

var historyShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show deployment details",
	Long: `Show full details for a deployment record by ID.

Example:
  azud history show deploy_123456789`,
	Args: cobra.ExactArgs(1),
	RunE: runHistoryShow,
}

var historyLimit int

func init() {
	historyListCmd.Flags().IntVar(&historyLimit, "limit", 20, "Maximum number of records to show (0 = all)")

	historyCmd.AddCommand(historyListCmd)
	historyCmd.AddCommand(historyShowCmd)

	rootCmd.AddCommand(historyCmd)
}

func runHistoryList(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	if historyLimit < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}

	log.Header("Deployment History")

	history := newHistoryStore(log)
	records, err := history.List(cfg.Service, historyLimit)
	if err != nil {
		return fmt.Errorf("failed to list deployment history: %w", err)
	}

	if len(records) == 0 {
		log.Info("No deployment history found for service %s", cfg.Service)
		return nil
	}

	rows := make([][]string, 0, len(records))
	for _, record := range records {
		rows = append(rows, []string{
			record.ID,
			valueOrDash(record.Version),
			string(record.Status),
			formatHistoryTime(record.StartedAt),
			formatHistoryDuration(record),
			valueOrDash(record.Destination),
			formatHistoryHosts(record.Hosts),
		})
	}

	log.Table(
		[]string{"ID", "Version", "Status", "Started", "Duration", "Destination", "Hosts"},
		rows,
	)
	log.Info("Show details with: azud history show <id>")
	return nil
}

func runHistoryShow(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	id := args[0]

	history := newHistoryStore(log)
	record, err := history.Get(id)
	if err != nil {
		if strings.Contains(err.Error(), "deployment record not found") {
			return fmt.Errorf("deployment record %s not found", id)
		}
		return fmt.Errorf("failed to load deployment history: %w", err)
	}

	log.Header("Deployment %s", record.ID)
	log.Println("Service: %s", record.Service)
	log.Println("Version: %s", valueOrDash(record.Version))
	log.Println("Status: %s", record.Status)
	log.Println("Image: %s", valueOrDash(record.Image))
	log.Println("Destination: %s", valueOrDash(record.Destination))
	log.Println("Started: %s", formatHistoryTime(record.StartedAt))
	log.Println("Completed: %s", formatHistoryTime(record.CompletedAt))
	log.Println("Duration: %s", formatHistoryDuration(record))
	log.Println("Hosts: %s", formatHistoryHostList(record.Hosts))
	log.Println("Previous Version: %s", valueOrDash(record.PreviousVersion))
	log.Println("Rolled Back: %t", record.RolledBack || record.Status == deploy.StatusRolledBack)

	if record.Error != "" {
		log.Println("Error: %s", record.Error)
	}

	if len(record.Metadata) > 0 {
		log.Println("")
		log.Println("Metadata:")

		keys := make([]string, 0, len(record.Metadata))
		for key := range record.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		rows := make([][]string, 0, len(keys))
		for _, key := range keys {
			rows = append(rows, []string{key, record.Metadata[key]})
		}

		log.Table([]string{"Key", "Value"}, rows)
	}

	return nil
}

func newHistoryStore(log *output.Logger) *deploy.HistoryStore {
	return deploy.NewHistoryStore(".", cfg.Deploy.RetainHistory, log)
}

func formatHistoryTime(ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	return ts.Local().Format("2006-01-02 15:04:05")
}

func formatHistoryDuration(record *deploy.DeploymentRecord) string {
	duration := record.Duration
	if duration <= 0 && !record.StartedAt.IsZero() && !record.CompletedAt.IsZero() {
		duration = record.CompletedAt.Sub(record.StartedAt)
	}
	if duration <= 0 {
		return "-"
	}
	if duration < time.Second {
		return "<1s"
	}
	return duration.Round(time.Second).String()
}

func formatHistoryHosts(hosts []string) string {
	switch len(hosts) {
	case 0:
		return "-"
	case 1:
		return hosts[0]
	case 2:
		return strings.Join(hosts, ",")
	default:
		return fmt.Sprintf("%s +%d", hosts[0], len(hosts)-1)
	}
}

func formatHistoryHostList(hosts []string) string {
	if len(hosts) == 0 {
		return "-"
	}
	return strings.Join(hosts, ", ")
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
