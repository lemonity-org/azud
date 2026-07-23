package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

const helpRule = "--------------------------------------------------------"

func configureHelp(command *cobra.Command) {
	command.SetHelpFunc(renderHelp)
	command.SetUsageFunc(renderUsage)
}

func renderHelp(command *cobra.Command, _ []string) {
	writer := command.OutOrStdout()
	title := strings.TrimPrefix(command.CommandPath(), command.Root().CommandPath()+" ")
	title = strings.ToUpper(strings.ReplaceAll(title, " ", " / "))
	if command == command.Root() {
		title = "COMMAND INDEX"
	}

	_, _ = fmt.Fprintf(writer, "AZUD / %s\n%s\n", title, helpRuleForWriter(writer))
	description, embeddedExamples := splitEmbeddedExamples(command.Long)
	if description != "" {
		writeHelpText(writer, description, helpWidth(writer), "")
	} else if command.Short != "" {
		writeHelpText(writer, command.Short, helpWidth(writer), "")
	}

	writeHelpSection(writer, "USAGE")
	_, _ = fmt.Fprintf(writer, "  %s\n", helpUsageLine(command))

	if command.HasAvailableSubCommands() {
		writeCommandIndex(writer, command, helpWidth(writer))
	}
	examples := embeddedExamples
	if command.HasExample() {
		if examples != "" {
			examples += "\n"
		}
		examples += dedentBlock(command.Example)
	}
	if examples != "" {
		writeHelpSection(writer, "EXAMPLES")
		writeExamples(writer, examples, helpWidth(writer))
	}
	if command.HasAvailableLocalFlags() {
		title := "OPTIONS"
		if command == command.Root() {
			title = "GLOBAL OPTIONS"
		}
		writeFlagSection(writer, title, command.NonInheritedFlags())
	}
	if command.HasAvailableInheritedFlags() {
		writeFlagSection(writer, "GLOBAL OPTIONS", command.InheritedFlags())
	}

	writeHelpSection(writer, "HELP")
	if command.HasAvailableSubCommands() {
		_, _ = fmt.Fprintf(writer, "  %s <command> --help\n", command.CommandPath())
	} else {
		_, _ = fmt.Fprintf(writer, "  %s --help\n", command.CommandPath())
	}
}

func renderUsage(command *cobra.Command) error {
	writer := command.ErrOrStderr()
	_, err := fmt.Fprintf(writer, "USAGE / %s\n%s\n  %s\n", strings.ToUpper(command.CommandPath()), helpRuleForWriter(writer), helpUsageLine(command))
	return err
}

func helpUsageLine(command *cobra.Command) string {
	if command.HasAvailableSubCommands() && !command.Runnable() {
		return command.CommandPath() + " <command> [options]"
	}
	return command.UseLine()
}

func writeCommandIndex(writer io.Writer, command *cobra.Command, widthLimit int) {
	groups := map[string][]*cobra.Command{}
	for _, child := range command.Commands() {
		if !child.IsAvailableCommand() || child.Name() == "help" {
			continue
		}
		group := "COMMANDS"
		if command == command.Root() {
			group = rootCommandGroup(child.Name())
		}
		groups[group] = append(groups[group], child)
	}

	order := []string{"DEPLOY", "OPERATE", "SYSTEM", "REFERENCE", "COMMANDS"}
	for _, group := range order {
		commands := groups[group]
		if len(commands) == 0 {
			continue
		}
		sort.Slice(commands, func(left, right int) bool {
			return commands[left].Name() < commands[right].Name()
		})

		writeHelpSection(writer, group)
		width := 0
		for _, child := range commands {
			if len(child.Name()) > width {
				width = len(child.Name())
			}
		}
		for _, child := range commands {
			if widthLimit < 60 {
				_, _ = fmt.Fprintf(writer, "  %s\n", child.Name())
				writeHelpText(writer, child.Short, widthLimit, "    ")
			} else {
				_, _ = fmt.Fprintf(writer, "  %-*s  %s\n", width, child.Name(), child.Short)
			}
		}
	}
}

func rootCommandGroup(name string) string {
	switch name {
	case "build", "deploy", "history", "preflight", "redeploy", "rollback", "setup":
		return "DEPLOY"
	case "accessory", "app", "canary", "cron", "proxy", "scale":
		return "OPERATE"
	case "config", "env", "hooks", "init", "registry", "server", "ssh", "systemd":
		return "SYSTEM"
	default:
		return "REFERENCE"
	}
}

func writeFlagSection(writer io.Writer, title string, flags *pflag.FlagSet) {
	if flags == nil || !flags.HasAvailableFlags() {
		return
	}
	writeHelpSection(writer, title)
	width := helpWidth(writer)
	if width < 60 {
		writeStackedFlags(writer, flags, width)
		return
	}
	_, _ = fmt.Fprint(writer, flags.FlagUsagesWrapped(width))
}

func writeHelpSection(writer io.Writer, title string) {
	_, _ = fmt.Fprintf(writer, "\n%s\n", title)
}

func splitEmbeddedExamples(value string) (string, string) {
	for _, marker := range []string{"\nExamples:\n", "\nExample:\n"} {
		if index := strings.Index(value, marker); index >= 0 {
			return strings.TrimSpace(value[:index]), dedentBlock(value[index+len(marker):])
		}
	}
	return strings.TrimSpace(value), ""
}

func writeStackedFlags(writer io.Writer, flags *pflag.FlagSet, width int) {
	flags.VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden {
			return
		}
		signature := "    --" + flag.Name
		if flag.Shorthand != "" && flag.ShorthandDeprecated == "" {
			signature = fmt.Sprintf("  -%s, --%s", flag.Shorthand, flag.Name)
		}
		valueName, usage := pflag.UnquoteUsage(flag)
		if valueName != "" {
			signature += " " + valueName
		}
		_, _ = fmt.Fprintln(writer, signature)

		if !flagDefaultIsZero(flag) {
			if flag.Value.Type() == "string" {
				usage += fmt.Sprintf(" (default %q)", flag.DefValue)
			} else {
				usage += fmt.Sprintf(" (default %s)", flag.DefValue)
			}
		}
		if flag.Deprecated != "" {
			usage += fmt.Sprintf(" (DEPRECATED: %s)", flag.Deprecated)
		}
		writeHelpText(writer, usage, width, "    ")
	})
}

func flagDefaultIsZero(flag *pflag.Flag) bool {
	switch flag.Value.Type() {
	case "bool", "boolfunc":
		return flag.DefValue == "" || flag.DefValue == "false"
	case "duration":
		return flag.DefValue == "0" || flag.DefValue == "0s"
	case "float32", "float64", "int", "int8", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "count":
		return flag.DefValue == "0"
	case "string":
		return flag.DefValue == ""
	case "ip", "ipMask", "ipNet":
		return flag.DefValue == "<nil>"
	case "intSlice", "stringSlice", "stringArray":
		return flag.DefValue == "[]"
	default:
		return flag.DefValue == "" || flag.DefValue == "false" || flag.DefValue == "0" || flag.DefValue == "<nil>"
	}
}

func writeHelpText(writer io.Writer, value string, width int, prefix string) {
	for _, sourceLine := range strings.Split(strings.Trim(value, "\r\n"), "\n") {
		if strings.TrimSpace(sourceLine) == "" {
			_, _ = fmt.Fprintln(writer)
			continue
		}
		leading := sourceLine[:len(sourceLine)-len(strings.TrimLeft(sourceLine, " \t"))]
		linePrefix := prefix + leading
		content := strings.TrimSpace(sourceLine)
		if marker, body, ok := splitHelpListMarker(content); ok {
			lines := wrapHelpWords(body, width-len(linePrefix)-len(marker))
			for index, line := range lines {
				if index == 0 {
					_, _ = fmt.Fprintf(writer, "%s%s%s\n", linePrefix, marker, line)
				} else {
					_, _ = fmt.Fprintf(writer, "%s%s%s\n", linePrefix, strings.Repeat(" ", len(marker)), line)
				}
			}
			continue
		}
		for _, line := range wrapHelpWords(content, width-len(linePrefix)) {
			_, _ = fmt.Fprintf(writer, "%s%s\n", linePrefix, line)
		}
	}
}

func splitHelpListMarker(value string) (string, string, bool) {
	if strings.HasPrefix(value, "- ") || strings.HasPrefix(value, "* ") {
		return value[:2], value[2:], true
	}
	index := 0
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		index++
	}
	if index > 0 && strings.HasPrefix(value[index:], ". ") {
		return value[:index+2], value[index+2:], true
	}
	return "", value, false
}

func writeExamples(writer io.Writer, value string, width int) {
	for _, sourceLine := range strings.Split(value, "\n") {
		line := strings.TrimSpace(sourceLine)
		if line == "" {
			_, _ = fmt.Fprintln(writer)
			continue
		}
		if width < 60 {
			if command, comment, ok := strings.Cut(line, " # "); ok {
				_, _ = fmt.Fprintf(writer, "  %s\n", strings.TrimSpace(command))
				writeHelpText(writer, "# "+comment, width, "    ")
				continue
			}
		}
		_, _ = fmt.Fprintf(writer, "  %s\n", line)
	}
}

func wrapHelpWords(value string, width int) []string {
	if value == "" {
		return []string{""}
	}
	if width < 1 || len(value) <= width {
		return []string{value}
	}
	words := strings.Fields(value)
	if len(words) == 0 {
		return []string{""}
	}
	lines := make([]string, 0, len(words))
	current := words[0]
	for _, word := range words[1:] {
		if len(current)+1+len(word) <= width {
			current += " " + word
			continue
		}
		lines = append(lines, current)
		current = word
	}
	return append(lines, current)
}

func dedentBlock(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}

	indent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		current := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent < 0 || current < indent {
			indent = current
		}
	}
	if indent <= 0 {
		return strings.Join(lines, "\n")
	}
	for index, line := range lines {
		if len(line) >= indent {
			lines[index] = line[indent:]
		}
	}
	return strings.Join(lines, "\n")
}

func helpRuleForWriter(writer io.Writer) string {
	width := helpWidth(writer)
	if width >= len(helpRule) {
		return helpRule
	}
	return strings.Repeat("-", width)
}

func helpWidth(writer io.Writer) int {
	const defaultWidth = 96
	file, ok := writer.(*os.File)
	if !ok {
		return defaultWidth
	}
	columns, _, err := term.GetSize(int(file.Fd()))
	if err != nil {
		if !isatty.IsTerminal(file.Fd()) && !isatty.IsCygwinTerminal(file.Fd()) {
			return defaultWidth
		}
		fallback, parseErr := strconv.Atoi(os.Getenv("COLUMNS"))
		if parseErr != nil || fallback <= 0 {
			return defaultWidth
		}
		columns = fallback
	}
	if columns <= 0 {
		return defaultWidth
	}
	if columns < 24 {
		return 24
	}
	if columns > defaultWidth {
		return defaultWidth
	}
	return columns
}
