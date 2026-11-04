package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lemonity-org/azud/internal/agent"
	"github.com/lemonity-org/azud/internal/config"
	"github.com/lemonity-org/azud/internal/local"
	"github.com/lemonity-org/azud/internal/output"
	"github.com/lemonity-org/azud/internal/podman"
	"github.com/lemonity-org/azud/internal/registry"
	"github.com/lemonity-org/azud/internal/state"
)

// statusTagTimeout bounds how long status waits for the registry, so that an unreachable
// one costs one line and not the rest.
const statusTagTimeout = 10 * time.Second

var statusCmd = &cobra.Command{
	Use: "status",
	Annotations: map[string]string{
		scopeAnnotation:       scopeServer,
		skipSecretsAnnotation: "true",
	},
	Short: "Show what is deployed and what happens next",
	Long: `Show what this server runs and since when, what the tag names, a failed release
and why, a deploy that has not finished and what happens next, and accessories and cron
jobs that differ from the release, with the command that applies each.

It reads state and Podman and changes nothing. If the registry cannot be reached it says
so and shows the rest. Run it on the server.

Example:
  azud status
  azud status --details   # Also list containers`,
	Args: cobra.NoArgs,
	RunE: runStatus,
}

var statusDetails bool

func init() {
	statusCmd.Flags().BoolVar(&statusDetails, "details", false, "Also list containers")
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	output.SetVerbose(verbose)
	log := output.DefaultLogger

	if !cfg.Agent.Configured() {
		return fmt.Errorf("azud status reads the agent's state, and this configuration has no agent section. For a pushed deploy, run `azud app details`")
	}
	stateDir, err := state.LocalDir()
	if err != nil {
		return err
	}
	store := agent.NewStore(stateDir, cfg.Service)
	st, err := store.Load()
	if err != nil {
		return err
	}

	log.Header("Status / %s", cfg.Service)

	tag, tagErr := statusTag(cmd.Context())
	effective, effectiveErr := agent.LoadEffective(cfg, stateDir)
	log.Table([]string{"Field", "Value"}, statusRows(st, store, tag, tagErr, effective, effectiveErr))

	host, err := agentHost(cfg)
	if err != nil {
		log.Warn("Cannot read containers: %v", err)
		return nil
	}
	runner := local.New(local.Options{Context: cmd.Context(), User: cfg.SSH.User, CommandTimeout: cfg.SSH.CommandTimeout, Host: host})
	defer func() { _ = runner.Close() }()
	containers := podman.NewContainerManager(podman.NewClient(runner))
	list, err := containers.List(host, true, map[string]string{"label": "azud.service=" + cfg.Service})
	if err != nil {
		log.Warn("Cannot read containers: %v", err)
		return nil
	}

	printDrift(log, effective, host, list)
	if statusDetails {
		printContainers(log, containers, host, list)
	}
	return nil
}

// statusTag resolves what the tag names now. Secrets are loaded only here, because this
// is the one thing status needs them for, and a secrets_command that fails must cost
// this line and not the rest. That is why the command itself does not load them.
func statusTag(ctx context.Context) (string, error) {
	if cfg.Agent.Track == "" {
		return "", fmt.Errorf("agent.track is not configured")
	}
	ref, err := registry.ParseReference(imageRepository(cfg.Image) + ":" + cfg.Agent.Track)
	if err != nil {
		return "", err
	}
	if cfg.Registry.Username != "" && len(cfg.Registry.Password) > 0 {
		if _, ok := getSecret(cfg.Registry.Password[0]); !ok {
			if _, err := config.NewLoader(GetConfigPath(), GetDestination()).Load(); err != nil {
				return "", err
			}
		}
	}
	creds, err := registryCredentials(cfg, ref)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, statusTagTimeout)
	defer cancel()
	digest, err := newRegistryResolver().Digest(ctx, ref, creds)
	if err != nil {
		return "", fmt.Errorf("the registry cannot be reached: %w", err)
	}
	return digest, nil
}

func statusRows(st agent.State, store *agent.Store, tag string, tagErr error, effective *agent.Effective, effectiveErr error) [][]string {
	rows := [][]string{
		{"Following", imageRepository(cfg.Image) + ":" + cfg.Agent.Track},
		{"Deployed", orText(st.DeployedDigest, "nothing yet")},
	}
	switch {
	case effectiveErr != nil:
		rows = append(rows, []string{"Release", "cannot be read: " + firstLine(effectiveErr.Error())})
	case effective.Deployed:
		rows = append(rows, []string{"Image", effective.Config.Image})
	}
	if !st.DeployedAt.IsZero() {
		rows = append(rows, []string{"Since", statusTime(st.DeployedAt)})
	}

	tagRow := tag
	if tagErr != nil {
		tagRow = "unknown: " + firstLine(tagErr.Error())
		if st.LastSeenDigest != "" {
			rows = append(rows, []string{"Tag last named", st.LastSeenDigest + " at " + statusTime(st.LastCheckAt)})
		}
	}
	rows = append(rows, []string{"Tag names", tagRow})

	if st.FailedDigest != "" {
		label := "Held"
		if st.LastError != "" {
			label = "Failed"
		}
		rows = append(rows, []string{label, st.FailedDigest + " at " + statusTime(st.FailedAt)})
	}
	if st.LastError != "" {
		rows = append(rows, []string{"Last error", firstLine(st.LastError)})
	}
	if j := st.Journal; j != nil {
		rows = append(rows, []string{"Not finished", fmt.Sprintf("%s of %s, started %s", journalDirection(j), j.Digest, statusTime(j.StartedAt))})
	}
	rows = append(rows, []string{"Next", agent.Next(st, tag, time.Now(), cfg.Agent.DeployCommand != "")})

	if cfg.Agent.DeployCommand != "" {
		rows = append(rows, []string{"Deploys with", "agent.deploy_command, which owns this service's containers and proxy"})
	}
	if cfg.Agent.Bundles() {
		rows = append(rows, bundleStatusRows(store.Records(imageRepository(cfg.Image)), st.Current)...)
	}
	if edited := configEditedRow(effective); edited != nil {
		rows = append(rows, edited)
	}
	if intent, err := store.Intent(); err != nil {
		rows = append(rows, []string{"Stopped", "cannot be read: " + err.Error()})
	} else {
		if len(intent.Roles) > 0 {
			rows = append(rows, []string{"Stopped roles", strings.Join(intent.Roles, ", ") + " (azud app start starts them)"})
		}
		if len(intent.Accessories) > 0 {
			rows = append(rows, []string{"Stopped accessories", strings.Join(intent.Accessories, ", ") + " (azud accessory boot <name> starts one)"})
		}
	}
	return rows
}

// configEditedRow says when an image release's configuration is not the file on disk,
// and how to apply the file.
func configEditedRow(effective *agent.Effective) []string {
	if effective == nil || effective.Record == nil || effective.Record.Source != agent.SourceImage {
		return nil
	}
	files, err := agent.ReadConfigFiles(GetConfigPath(), GetDestination())
	if err != nil || effective.Record.HasFiles(files) {
		return nil
	}
	return []string{"Configuration", "edited since this release was applied; azud redeploy applies it"}
}

func journalDirection(j *agent.Journal) string {
	switch {
	case j.Phase == agent.PhaseBack:
		return "undo"
	case j.Move:
		return "move"
	}
	return "deploy"
}

// printDrift lists what differs from the release. Before the first deploy a bundle server
// has no release to compare with.
func printDrift(log *output.Logger, effective *agent.Effective, host string, containers []podman.Container) {
	if effective == nil || !effective.Deployed && cfg.Agent.Bundles() {
		return
	}
	drift := agent.DetectDrift(effective.Config, host, containers)
	if len(drift) == 0 {
		log.Info("Accessories and cron jobs match the release")
		return
	}
	rows := make([][]string, 0, len(drift))
	for _, d := range drift {
		rows = append(rows, []string{d.Kind, d.Name, string(d.State), d.Command})
	}
	log.Println("")
	log.Println("Differs from the release:")
	log.Table([]string{"Kind", "Name", "State", "Applies it"}, rows)
}

// printContainers lists this service's containers, with stats for those running, as
// azud app details does for roles.
func printContainers(log *output.Logger, containers *podman.ContainerManager, host string, list []podman.Container) {
	rows := make([][]string, 0, len(list))
	for _, container := range list {
		stats := "-"
		if container.State == "running" {
			if got, err := containers.Stats(host, container.Name); err == nil {
				stats = got
			}
		}
		rows = append(rows, []string{container.Name, containerKind(container), container.State, container.Image, stats})
	}
	log.Println("")
	log.Println("Containers:")
	log.Table([]string{"Container", "Kind", "State", "Image", "Stats"}, rows)
}

func containerKind(container podman.Container) string {
	switch {
	case container.Labels["azud.accessory"] != "":
		return agent.DriftAccessory
	case container.Labels["azud.cron"] != "":
		return agent.DriftCron
	case container.Labels["azud.role"] != "":
		return "role " + container.Labels["azud.role"]
	}
	return "-"
}

func statusTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return fmt.Sprintf("%s (%s ago)", t.Local().Format(time.RFC3339), time.Since(t).Round(time.Second))
}

func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
