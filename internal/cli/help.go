package cli

import (
	"flag"
	"fmt"
	"io"
	"runtime"
	"text/tabwriter"
)

type flagDoc struct {
	name  string
	usage string
}

type writer = io.Writer

func writeVersion(env Env) {
	out := env.stdout()
	fmt.Fprintf(out, "%s %s\n", env.program(), versionValue(env))
	fmt.Fprintf(out, "commit: %s\n", commitValue(env))
	fmt.Fprintf(out, "go: %s\n", runtime.Version())
	fmt.Fprintf(out, "platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

func versionValue(env Env) string {
	if env.Version == "" {
		return "dev"
	}
	return env.Version
}

func commitValue(env Env) string {
	if env.Commit == "" {
		return "unknown"
	}
	return env.Commit
}

func writeRootHelp(env Env, out writer) {
	program := env.program()
	fmt.Fprintf(out, "%s - Linux networking, failure-injection and diagnostics laboratory\n\n", program)
	fmt.Fprintf(out, "Usage:\n  %s\n\n", rootUsage(program))
	fmt.Fprintln(out, "Commands:")
	writeCommandTable(out, registry())
	fmt.Fprintln(out, "\nGlobal flags:")
	writeFlags(out, []flagDoc{
		{"--log-level", "log level: debug, info, warn, error (default info)"},
		{"--log-format", "log format: text, json (default text)"},
		{"--help", "print this help and exit"},
		{"--version", "print the version and exit"},
	})
	fmt.Fprintf(out, "\nRun \"%s help <command>\" for command details.\n", program)
}

func writeGroupHelp(env Env, out writer, command *Command, subcommands map[string]*Command) {
	program := env.program()
	fmt.Fprintf(out, "%s %s - %s\n\n", program, command.Name, command.Summary)
	fmt.Fprintf(out, "Usage:\n  %s\n\n", command.Usage)
	fmt.Fprintln(out, "Subcommands:")
	writeCommandTable(out, subcommands)
	writeCommandFlags(out, command)
	fmt.Fprintf(out, "\nRun \"%s help %s <subcommand>\" for details.\n", program, command.Name)
}

func writeCommandHelp(env Env, command *Command) {
	program := env.program()
	out := env.stdout()
	fmt.Fprintf(out, "%s - %s\n\n", commandPath(program, command), command.Summary)
	fmt.Fprintf(out, "Usage:\n  %s\n", command.Usage)

	if subcommands, ok := subcommandsOf(command.Name); ok {
		fmt.Fprintln(out, "\nSubcommands:")
		writeCommandTable(out, subcommands)
	}
	writeCommandFlags(out, command)
}

func commandPath(program string, command *Command) string {
	if command.Parent == "" {
		return program + " " + command.Name
	}
	return program + " " + command.Parent + " " + command.Name
}

func writeCommandFlags(out writer, command *Command) {
	if command.Setup == nil {
		return
	}
	fs := newFlagSet(command.Name)
	command.Setup(fs)

	docs := make([]flagDoc, 0, 4)
	fs.VisitAll(func(f *flag.Flag) {
		docs = append(docs, flagDoc{name: "--" + f.Name, usage: f.Usage})
	})
	if len(docs) == 0 {
		return
	}
	fmt.Fprintln(out, "\nFlags:")
	writeFlags(out, docs)
}

func writeCommandTable(out writer, commands map[string]*Command) {
	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, name := range sortedNames(commands) {
		fmt.Fprintf(writer, "  %s\t%s\n", name, commands[name].Summary)
	}
	_ = writer.Flush()
}

func writeFlags(out writer, flags []flagDoc) {
	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, doc := range flags {
		fmt.Fprintf(writer, "  %s\t%s\n", doc.name, doc.usage)
	}
	_ = writer.Flush()
}

func rootUsage(program string) string {
	return program + " [global flags] <command> [arguments]"
}
