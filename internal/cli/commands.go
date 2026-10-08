package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"strings"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/config"
	"github.com/codingseeker/netforge/internal/logging"
)

type fileOptions struct {
	strictDefaults bool
}

func registry() map[string]*Command {
	return map[string]*Command{
		"help":       helpCommand(),
		"version":    versionCommand(),
		"config":     configCommand(),
		"limits":     limitsCommand(),
		"experiment": experimentCommand(),
		"recovery":   recoveryCommand(),
		"reconcile":  reconcileCommand(),
	}
}

func configSubcommands() map[string]*Command {
	return map[string]*Command{
		"validate": configValidate(),
		"show":     configShow(),
	}
}

func limitsSubcommands() map[string]*Command {
	return map[string]*Command{
		"show": limitsShow(),
	}
}

func versionCommand() *Command {
	return &Command{
		Name:    "version",
		Summary: "Print NetForge version information.",
		Usage:   "netforge version",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			if err := rejectExtraArgs("version", args); err != nil {
				return err
			}
			writeVersion(session.Env)
			return nil
		},
	}
}

func helpCommand() *Command {
	return &Command{
		Name:    "help",
		Summary: "Print help for a command.",
		Usage:   "netforge help [command]",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			if len(args) == 0 {
				writeRootHelp(session.Env, session.Env.stdout())
				return nil
			}
			commands := registry()
			command, ok := commands[args[0]]
			if !ok {
				return apperr.InvalidArgument("cli.help", "unknown command "+quote(args[0]), nil)
			}
			if len(args) > 1 {
				subcommands, ok := subcommandsOf(command.Name)
				if !ok {
					return apperr.InvalidArgument("cli.help", "command "+quote(command.Name)+" has no subcommands", nil)
				}
				if len(args) > 2 {
					return apperr.InvalidArgument("cli.help", "too many help arguments", nil)
				}
				sub, found := subcommands[args[1]]
				if !found {
					return apperr.InvalidArgument("cli.help", "unknown subcommand "+quote(args[0])+" "+quote(args[1]), nil)
				}
				writeCommandHelp(session.Env, sub)
				return nil
			}
			if subcommands, ok := subcommandsOf(command.Name); ok {
				writeGroupHelp(session.Env, session.Env.stdout(), command, subcommands)
				return nil
			}
			writeCommandHelp(session.Env, command)
			return nil
		},
	}
}

func subcommandsOf(name string) (map[string]*Command, bool) {
	switch name {
	case "config":
		return configSubcommands(), true
	case "limits":
		return limitsSubcommands(), true
	case "experiment":
		return experimentSubcommands(), true
	case "recovery":
		return recoverySubcommands(), true
	case "reconcile":
		return reconcileSubcommands(), true
	default:
		return nil, false
	}
}

func configCommand() *Command {
	return &Command{
		Name:    "config",
		Summary: "Validate and render experiment configuration.",
		Usage:   "netforge config <validate|show> <file|->",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			subcommands := configSubcommands()
			if len(args) == 0 {
				writeGroupHelp(session.Env, session.Env.stderr(), commands()["config"], subcommands)
				return apperr.InvalidArgument("cli.config", "missing subcommand; run "+session.Env.program()+" help config", nil)
			}
			subcommand, ok := subcommands[args[0]]
			if !ok {
				return apperr.InvalidArgument("cli.config", "unknown subcommand "+quote(args[0])+"; run "+session.Env.program()+" help config", nil)
			}
			return runCommand(ctx, session, subcommand, args[1:])
		},
	}
}

func configValidate() *Command {
	return &Command{
		Name:    "validate",
		Parent:  "config",
		Summary: "Validate a configuration file without side effects.",
		Usage:   "netforge config validate <file|->",
		Setup: func(fs *flag.FlagSet) any {
			opts := &fileOptions{}
			fs.BoolVar(&opts.strictDefaults, "show-effective", false, "print the effective configuration after validation")
			return opts
		},
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			if err := requireOnePath("config validate", args); err != nil {
				return err
			}
			cfg, err := config.LoadFrom(args[0], session.Env.stdin())
			if err != nil {
				return err
			}

			session.Logger.Info(logging.EventConfigurationValidated,
				slog.String(logging.FieldExperimentID, cfg.ExperimentID()),
				slog.String("configuration", cfg.Name),
				slog.String("scope_network", cfg.Network()),
				slog.Int("scope_namespaces", len(cfg.NamespaceNames())),
			)

			fmt.Fprintf(session.Env.stdout(), "configuration: %s\n", cfg.Name)
			fmt.Fprintf(session.Env.stdout(), "experiment_id: %s\n", cfg.ExperimentID())
			fmt.Fprintf(session.Env.stdout(), "scope.network: %s\n", cfg.Network())
			fmt.Fprintf(session.Env.stdout(), "scope.namespaces: %s\n", strings.Join(cfg.NamespaceNames(), ","))

			opts, _ := options.(*fileOptions)
			if opts != nil && opts.strictDefaults {
				rendered, err := config.Marshal(*cfg)
				if err != nil {
					return err
				}
				fmt.Fprintln(session.Env.stdout(), "effective configuration:")
				fmt.Fprint(session.Env.stdout(), string(rendered))
			}
			return nil
		},
	}
}

func configShow() *Command {
	return &Command{
		Name:    "show",
		Parent:  "config",
		Summary: "Print the canonical form of a configuration file.",
		Usage:   "netforge config show <file|->",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			if err := requireOnePath("config show", args); err != nil {
				return err
			}
			cfg, err := config.LoadFrom(args[0], session.Env.stdin())
			if err != nil {
				return err
			}
			rendered, err := config.Marshal(*cfg)
			if err != nil {
				return err
			}
			if _, err := session.Env.stdout().Write(rendered); err != nil {
				return apperr.IO("cli.config.show", "cannot write configuration", err)
			}
			return nil
		},
	}
}

func limitsCommand() *Command {
	return &Command{
		Name:    "limits",
		Summary: "Inspect the resource limits enforced by NetForge.",
		Usage:   "netforge limits show",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			subcommands := limitsSubcommands()
			if len(args) == 0 {
				writeGroupHelp(session.Env, session.Env.stderr(), commands()["limits"], subcommands)
				return apperr.InvalidArgument("cli.limits", "missing subcommand; run "+session.Env.program()+" help limits", nil)
			}
			subcommand, ok := subcommands[args[0]]
			if !ok {
				return apperr.InvalidArgument("cli.limits", "unknown subcommand "+quote(args[0])+"; run "+session.Env.program()+" help limits", nil)
			}
			return runCommand(ctx, session, subcommand, args[1:])
		},
	}
}

func limitsShow() *Command {
	return &Command{
		Name:    "show",
		Parent:  "limits",
		Summary: "Print the default and maximum supported resource limits.",
		Usage:   "netforge limits show",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			if err := rejectExtraArgs("limits show", args); err != nil {
				return err
			}
			defaults := config.DefaultLimits()
			out := session.Env.stdout()
			fmt.Fprintf(out, "max_config_bytes: %d\n", config.MaxConfigBytes)
			fmt.Fprintf(out, "max_name_length: %d\n", config.MaxNameLength)
			fmt.Fprintf(out, "default.max_namespaces: %d\n", defaults.MaxNamespaces)
			fmt.Fprintf(out, "default.max_interfaces: %d\n", defaults.MaxInterfaces)
			fmt.Fprintf(out, "default.max_duration: %s\n", defaults.MaxDuration)
			fmt.Fprintf(out, "default.max_bandwidth: %s\n", defaults.MaxBandwidth)
			fmt.Fprintf(out, "default.max_packet_rate: %d\n", defaults.MaxPacketRate)
			fmt.Fprintf(out, "default.max_concurrent_probes: %d\n", defaults.MaxConcurrentProbes)
			fmt.Fprintf(out, "ceiling.max_namespaces: %d\n", config.CeilingNamespaces)
			fmt.Fprintf(out, "ceiling.max_interfaces: %d\n", config.CeilingInterfaces)
			fmt.Fprintf(out, "ceiling.max_duration: %s\n", config.CeilingDuration)
			fmt.Fprintf(out, "ceiling.max_bandwidth: %s\n", config.CeilingBandwidth)
			fmt.Fprintf(out, "ceiling.max_packet_rate: %d\n", config.CeilingPacketRate)
			fmt.Fprintf(out, "ceiling.max_concurrent_probes: %d\n", config.CeilingConcurrentProbes)
			return nil
		},
	}
}

func commands() map[string]*Command {
	return registry()
}

func rejectExtraArgs(name string, args []string) error {
	if len(args) > 0 {
		return apperr.InvalidArgument("cli."+name, "unexpected argument "+quote(args[0]), nil)
	}
	return nil
}

func requireOnePath(name string, args []string) error {
	switch len(args) {
	case 1:
		return nil
	case 0:
		return apperr.InvalidArgument("cli."+name, "a configuration path is required; use - to read standard input", nil)
	default:
		return apperr.InvalidArgument("cli."+name, "exactly one configuration path is accepted, got "+fmt.Sprint(len(args)), nil)
	}
}
