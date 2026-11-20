package main

import (
	"flag"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// The CLI surface is the part of the mayfly command that a caller can depend
// on: command names, flag names and forms, flag types and defaults, and the
// environment variables the command reads. docs/deprecation.md records the
// promise. testdata/cli-surface.txt records the surface, and
// hack/surface-diff.sh fails a pull request that changes the record without a
// "Breaking changes" entry.
//
// The surface comes from the flag sets the commands parse with, so it cannot
// describe a flag that the code does not have. The format of the golden file
// belongs to the gate, not to the CLI.

// surfaceCommand is one command word of the CLI surface.
type surfaceCommand struct {
	// name is the command word. A subcommand carries its parent, as in
	// "plugin init".
	name string
	// aliases are other words that dispatch this command.
	aliases []string
	// subcommands are the words that exist only below this command.
	subcommands []string
	// newFlags builds the flag set the command parses. A nil newFlags
	// marks a command that parses no flag, such as help.
	newFlags func() *flag.FlagSet
	// flagAliases pairs a short flag name with the long name that freezes
	// with it.
	flagAliases [][2]string
}

// surfaceEnv is one environment variable the command reads.
type surfaceEnv struct {
	name string
	// defaultValue is the value the command uses when the variable is
	// unset. An empty defaultValue means the variable has no default.
	defaultValue string
	// deprecatedBy names the variable that replaces this one.
	deprecatedBy string
}

// cliSurface lists every command word of the surface. The commands map in
// main.go lists the words main dispatches, and TestSurfaceCoversEveryCommand
// fails when the two disagree.
var cliSurface = []surfaceCommand{
	{name: "agents", newFlags: func() *flag.FlagSet { return newAgentsFlags().FlagSet }},
	{name: "artifact", newFlags: func() *flag.FlagSet { return newArtifactFlags().FlagSet }, flagAliases: [][2]string{{"o", "output"}}},
	{name: "attach", newFlags: func() *flag.FlagSet { return newAttachFlags().FlagSet }},
	{name: "close", newFlags: func() *flag.FlagSet { return newCloseFlags().FlagSet }},
	{name: "delete", newFlags: func() *flag.FlagSet { return newDeleteFlags().FlagSet }},
	{name: "events", newFlags: func() *flag.FlagSet { return newEventsFlags().FlagSet }},
	{name: "get", newFlags: func() *flag.FlagSet { return newGetFlags().FlagSet }},
	{name: "help", aliases: []string{"-h", "--help"}},
	{name: "ls", newFlags: func() *flag.FlagSet { return newListFlags().FlagSet }},
	{name: "plugin", subcommands: []string{"init", "check", "test"}},
	{name: "plugin check", newFlags: func() *flag.FlagSet { return newPluginCheckFlags().FlagSet }},
	{name: "plugin init", newFlags: func() *flag.FlagSet { return newPluginInitFlags().FlagSet }},
	{name: "plugin test", newFlags: func() *flag.FlagSet { return newPluginTestFlags().FlagSet }},
	{name: "run", newFlags: func() *flag.FlagSet { return newRunFlags().FlagSet }},
	{name: "send", newFlags: func() *flag.FlagSet { return newSendFlags().FlagSet }},
	{name: "transcript", newFlags: func() *flag.FlagSet { return newTranscriptFlags().FlagSet }},
	{name: "version", aliases: []string{"-v", "--version"}, newFlags: func() *flag.FlagSet { return newVersionFlags().FlagSet }},
}

// surfaceEnvVars lists the environment variables the command reads.
var surfaceEnvVars = []surfaceEnv{
	{name: envURL, defaultValue: defaultURL},
	{name: envToken},
	{name: envTokenAlias, deprecatedBy: envToken},
}

// surfaceLines returns the surface as sorted text, one line per command,
// subcommand, flag, or environment variable.
func surfaceLines() []string {
	var lines []string
	for _, cmd := range cliSurface {
		lines = append(lines, "command "+cmd.name)
		for _, alias := range cmd.aliases {
			lines = append(lines, "command "+cmd.name+" alias-command "+alias)
		}
		for _, sub := range cmd.subcommands {
			lines = append(lines, "command "+cmd.name+" subcommand "+sub)
		}
		if cmd.newFlags != nil {
			cmd.newFlags().VisitAll(func(f *flag.Flag) {
				lines = append(lines, fmt.Sprintf("command %s flag %s type %s default %q", cmd.name, f.Name, surfaceFlagType(f), f.DefValue))
			})
		}
		for _, alias := range cmd.flagAliases {
			lines = append(lines, "command "+cmd.name+" flag "+alias[0]+" alias "+alias[1])
		}
	}
	for _, e := range surfaceEnvVars {
		line := "env " + e.name
		if e.defaultValue != "" {
			line += fmt.Sprintf(" default %q", e.defaultValue)
		}
		if e.deprecatedBy != "" {
			line += " deprecated-by " + e.deprecatedBy
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
}

// surfaceFlagType returns the value type of one flag as the surface reports
// it. The flag package names a built-in value after its type with a Value
// suffix, as in *flag.stringValue.
func surfaceFlagType(f *flag.Flag) string {
	t := reflect.TypeOf(f.Value)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if name := strings.TrimSuffix(t.Name(), "Value"); name != "" {
		return name
	}
	return t.String()
}
