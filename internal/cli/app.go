package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/cleanup"
	"github.com/netforge/netforge/internal/logging"
)

const ProgramName = "netforge"

type Env struct {
	Name    string
	Version string
	Commit  string
	Args    []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string

	context context.Context
}

func (e Env) program() string {
	if e.Name == "" {
		return ProgramName
	}
	return e.Name
}

func (e Env) lookupEnv(key string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(key)
}

func (e Env) stdin() io.Reader {
	if e.Stdin == nil {
		return strings.NewReader("")
	}
	return e.Stdin
}

func (e Env) stdout() io.Writer {
	if e.Stdout == nil {
		return io.Discard
	}
	return e.Stdout
}

func (e Env) stderr() io.Writer {
	if e.Stderr == nil {
		return io.Discard
	}
	return e.Stderr
}

type Session struct {
	Env    Env
	Logger *slog.Logger
	Stack  *cleanup.Stack
}

type Command struct {
	Name string

	Parent  string
	Summary string
	Usage   string
	Setup   func(fs *flag.FlagSet) any
	Run     func(ctx context.Context, session *Session, options any, args []string) error
}

var commandRegistry = registry

type rootOptions struct {
	logLevel  string
	logFormat string
	version   bool
	help      bool
}

type App struct {
	Env     Env
	Version string
	Commit  string
}

func (a *App) Run(ctx context.Context) error {
	env := a.Env
	if env.Name == "" {
		env.Name = ProgramName
	}
	if env.Version == "" {
		env.Version = a.Version
	}
	if env.Commit == "" {
		env.Commit = a.Commit
	}
	if ctx == nil {
		ctx = context.Background()
	}
	env.context = ctx

	options := &rootOptions{}
	fs := newFlagSet(env.program())
	fs.StringVar(&options.logLevel, "log-level", options.logLevel, "log level: debug, info, warn, error")
	fs.StringVar(&options.logFormat, "log-format", options.logFormat, "log format: text, json")
	fs.BoolVar(&options.version, "version", false, "print the version and exit")
	fs.BoolVar(&options.help, "help", false, "print this help and exit")

	if err := fs.Parse(env.Args); err != nil {
		if isHelpRequest(err) {
			writeRootHelp(env, env.stdout())
			return nil
		}
		return fail(env, apperr.InvalidArgument("cli.parse", "invalid global flags: "+err.Error(), err))
	}
	args := fs.Args()

	logger, err := newLogger(env, options)
	if err != nil {
		return fail(env, err)
	}

	if options.version {
		writeVersion(env)
		return nil
	}
	if options.help {
		writeRootHelp(env, env.stdout())
		return nil
	}
	if len(args) == 0 {
		return fail(env, apperr.InvalidArgument("cli", "no command given; run "+env.program()+" help", nil))
	}

	command, ok := commandRegistry()[args[0]]
	if !ok {
		return fail(env, apperr.InvalidArgument("cli.dispatch", "unknown command "+quote(args[0])+"; run "+env.program()+" help", nil))
	}

	session := &Session{
		Env:    env,
		Logger: logger.With(slog.String(logging.FieldComponent, command.Name)),
		Stack:  cleanup.New(),
	}
	session.Logger.Debug("command dispatch", slog.String(logging.FieldEvent, "dispatch"))

	if err := session.Stack.Guard(ctx, func() error {
		return runCommand(ctx, session, command, args[1:])
	}); err != nil {
		session.Logger.Debug("command failed",
			slog.String(logging.FieldEvent, "command_failed"),
			slog.String("error", err.Error()))
		return fail(env, err)
	}
	return nil
}

func newLogger(env Env, options *rootOptions) (*slog.Logger, error) {
	level, err := logging.ParseLevel(firstNonEmpty(options.logLevel, env.lookupEnv("NETFORGE_LOG_LEVEL"), "info"))
	if err != nil {
		return nil, err
	}
	format, err := logging.ParseFormat(firstNonEmpty(options.logFormat, env.lookupEnv("NETFORGE_LOG_FORMAT"), "text"))
	if err != nil {
		return nil, err
	}
	return logging.New(env.stderr(), logging.Options{Level: level, Format: format})
}

func runCommand(ctx context.Context, session *Session, command *Command, args []string) error {
	fs := newFlagSet(command.Name)
	var options any
	if command.Setup != nil {
		options = command.Setup(fs)
	}

	if err := fs.Parse(args); err != nil {
		if isHelpRequest(err) {
			writeCommandHelp(session.Env, command)
			return nil
		}
		return apperr.InvalidArgument("cli.parse", "invalid flags for command "+quote(command.Name)+": "+err.Error(), err)
	}
	remaining := fs.Args()

	if ctx.Err() != nil {
		return apperr.FromContext(ctx, command.Name)
	}
	return command.Run(ctx, session, options, remaining)
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

func fail(env Env, err error) error {
	if err == nil {
		return nil
	}
	fmt.Fprintf(env.stderr(), "%s: %s\n", env.program(), err.Error())
	return err
}

func isHelpRequest(err error) bool {
	return err != nil && err == flag.ErrHelp
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func quote(value string) string {
	return "\"" + value + "\""
}

func sortedNames(commands map[string]*Command) []string {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
