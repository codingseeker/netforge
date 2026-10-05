package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/chaos"
	"github.com/netforge/netforge/internal/config"
	"github.com/netforge/netforge/internal/experiment"
	"github.com/netforge/netforge/internal/logging"
	"github.com/netforge/netforge/internal/network"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/recovery"
	"github.com/netforge/netforge/internal/report"
)

type experimentOptions struct {
	namespace  string
	target     string
	port       uint
	protocol   string
	count      int
	size       int
	timeout    time.Duration
	deadline   time.Duration
	afterFault time.Duration
	settle     time.Duration
	recover    stringList
	authorize  bool
	reason     string
	output     string
	clients    int
	servers    int
	id         string

	routing         string
	servicePorts    uint
	serviceBasePort uint
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func experimentFlags(fs *flag.FlagSet, opts *experimentOptions) {
	fs.StringVar(&opts.namespace, "namespace", "", "laboratory namespace the experiment measures from")
	fs.StringVar(&opts.target, "target", "", "address the experiment observes")
	fs.UintVar(&opts.port, "port", 0, "port measured, for the protocols that use one")
	fs.StringVar(&opts.protocol, "protocol", "tcp", "protocol the observation is about: icmp, tcp, udp, dns, tls or http")
	fs.IntVar(&opts.count, "count", 5, "probes per measurement")
	fs.IntVar(&opts.size, "size", 64, "payload bytes per probe")
	fs.DurationVar(&opts.timeout, "timeout", time.Second, "how long one probe may wait for its reply")
	fs.DurationVar(&opts.deadline, "deadline", 5*time.Second, "how long one measurement may take")
	fs.DurationVar(&opts.afterFault, "after-fault", 200*time.Millisecond, "how long a fault is given to reach the kernel")
	fs.DurationVar(&opts.settle, "settle", 200*time.Millisecond, "how long a change is given to take effect")
	fs.Var(&opts.recover, "recover", "an authorized recovery action, as action:target; may be given more than once")
	fs.BoolVar(&opts.authorize, "authorize", false, "authorize the -recover actions; without it they are refused")
	fs.StringVar(&opts.reason, "why", "", "why the recovery actions were authorized")
}

func experimentSubcommands() map[string]*Command {
	return map[string]*Command{
		"plan": experimentPlan(),
		"run":  experimentRun(),
	}
}

func experimentCommand() *Command {
	return &Command{
		Name:    "experiment",
		Summary: "Validate an experiment document and show the lifecycle it would run.",
		Usage:   "netforge experiment <plan|run> [flags] <file|->",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			subcommands := experimentSubcommands()
			if len(args) == 0 {
				writeGroupHelp(session.Env, session.Env.stderr(), commands()["experiment"], subcommands)
				return apperr.InvalidArgument("cli.experiment", "missing subcommand; run "+session.Env.program()+" help experiment", nil)
			}
			subcommand, ok := subcommands[args[0]]
			if !ok {
				return apperr.InvalidArgument("cli.experiment", "unknown subcommand "+quote(args[0])+"; run "+session.Env.program()+" help experiment", nil)
			}
			return runCommand(ctx, session, subcommand, args[1:])
		},
	}
}

func experimentPlan() *Command {
	return &Command{
		Name:    "plan",
		Parent:  "experiment",
		Summary: "Compile a document into a plan and print the lifecycle, without running it. Flags come before the path.",
		Usage:   "netforge experiment plan [flags] <file|->",
		Setup: func(fs *flag.FlagSet) any {
			opts := &experimentOptions{}
			experimentFlags(fs, opts)
			return opts
		},
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			if err := requireOnePath("experiment plan", args); err != nil {
				return err
			}
			document, err := config.LoadFrom(args[0], session.Env.stdin())
			if err != nil {
				return err
			}
			opts, _ := options.(*experimentOptions)
			if opts == nil {
				opts = &experimentOptions{}
			}
			observation, err := opts.observation()
			if err != nil {
				return err
			}
			requests, err := opts.requests()
			if err != nil {
				return err
			}

			plan, err := experiment.Compile(*document, observation, requests,
				recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
			if err != nil {
				return err
			}

			out := session.Env.stdout()
			fmt.Fprintf(out, "name: %s\n", plan.Name)
			fmt.Fprintf(out, "experiment_id: %s\n", plan.ExperimentID)
			fmt.Fprintf(out, "scope: %s\n", plan.Scope)
			if plan.Seed != nil {
				fmt.Fprintf(out, "seed: %d\n", *plan.Seed)
			}
			fmt.Fprintf(out, "faults: %d\n", len(plan.Faults))
			fmt.Fprintf(out, "recoveries: %d\n", len(plan.Recoveries))
			fmt.Fprintf(out, "budget: %s\n", plan.Total)
			fmt.Fprintln(out, "lifecycle:")
			for _, step := range plan.Steps {
				fmt.Fprintf(out, "  %-8s %s\n", step.State, step.Describe())
			}
			return nil
		},
	}
}

func experimentRun() *Command {
	return &Command{
		Name:    "run",
		Parent:  "experiment",
		Summary: "Run the experiment a document describes and report it.",
		Usage:   "netforge experiment run [flags] <file|->",
		Setup: func(fs *flag.FlagSet) any {
			opts := &experimentOptions{}
			experimentFlags(fs, opts)
			fs.StringVar(&opts.output, "output", "text", "report format: text or json")
			fs.IntVar(&opts.clients, "clients", 1, "clients the laboratory creates")
			fs.IntVar(&opts.servers, "servers", 1, "servers the laboratory creates")
			fs.StringVar(&opts.id, "id", "", "the laboratory's identifier; its namespaces are nf-<role>-<id>")
			fs.StringVar(&opts.routing, "routing", "kernel",
				"who forwards in the router namespace: kernel or software")
			fs.UintVar(&opts.servicePorts, "service-ports", 0,
				"server namespaces that serve the endpoint the observation measures; 0 serves none, "+
					"and a protocol that needs a listener must ask for at least one")
			fs.UintVar(&opts.serviceBasePort, "service-base-port", 0,
				"the first port a serving namespace binds; the i-th binds base+i; default the observation's -port")
			return opts
		},
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			if err := requireOnePath("experiment run", args); err != nil {
				return err
			}
			document, err := config.LoadFrom(args[0], session.Env.stdin())
			if err != nil {
				return err
			}
			opts, _ := options.(*experimentOptions)
			if opts == nil {
				opts = &experimentOptions{}
				opts.output = "text"
				opts.clients = 1
				opts.servers = 1
				opts.routing = "kernel"
			}

			if err := opts.checkRunFlags(); err != nil {
				return err
			}
			observation, err := opts.observation()
			if err != nil {
				return err
			}
			requests, err := opts.requests()
			if err != nil {
				return err
			}

			plan, err := experiment.Compile(*document, observation, requests,
				recovery.DefaultCriteria(), experiment.DefaultLimits(), time.Now())
			if err != nil {
				return err
			}
			spec, err := opts.labSpec(*document, plan.ExperimentID)
			if err != nil {
				return err
			}
			environment, err := experiment.NewLabEnvironment(plan, spec, buildLaboratory)
			if err != nil {
				return err
			}
			controller, err := experiment.New(plan, environment, time.Now())
			if err != nil {
				return err
			}
			run, _ := controller.Run(ctx)

			reportErr := opts.writeReport(session.Env.stdout(), run, environment.Summary())
			logger := session.Logger.With(slog.String(logging.FieldExperimentID, run.ExperimentID))
			if run.Succeeded() {
				logger.Info("experiment run succeeded",
					slog.Duration("duration", run.Duration))
			} else {
				logger.Warn("experiment run failed",
					slog.String("state", string(run.Failed)),
					slog.Duration("duration", run.Duration))
			}
			if reportErr != nil {
				return apperr.IO("cli.experiment.run", "the report could not be written", reportErr)
			}
			if run.Err != nil {
				return run.Err
			}
			return nil
		},
	}
}

func (o *experimentOptions) checkRunFlags() error {
	if err := o.checkOutput(); err != nil {
		return err
	}
	_, err := o.routingMode()
	return err
}

func (o *experimentOptions) routingMode() (network.Routing, error) {
	const op = "cli.experiment.run"
	switch strings.ToLower(strings.TrimSpace(o.routing)) {
	case "", "kernel":
		return network.RoutingKernel, nil
	case string(network.RoutingSoftware):
		return network.RoutingSoftware, nil
	}
	return "", apperr.Unsupported(op,
		"-routing "+quote(o.routing)+" is not a mode this build forwards with, want kernel or software", nil)
}

func (o *experimentOptions) serviceRange() (count int, base uint16, err error) {
	const op = "cli.experiment.run"
	protocol := probe.Protocol(strings.ToLower(strings.TrimSpace(o.protocol)))
	count = int(o.servicePorts)
	if count == 0 {
		if o.serviceBasePort != 0 {
			return 0, 0, apperr.InvalidArgument(op, "-service-base-port is "+
				strconv.FormatUint(uint64(o.serviceBasePort), 10)+" but -service-ports is 0, "+
				"so no namespace would bind it", nil)
		}
		if protocol.NeedsListener() {
			return 0, 0, apperr.Validation(op, "-protocol "+string(protocol)+
				" is answered by a process, and -service-ports 0 asks for none: "+
				"an observation with nothing listening measures a closed port", nil)
		}
		return 0, 0, nil
	}
	if count > o.servers {
		return 0, 0, apperr.InvalidArgument(op, "-service-ports is "+strconv.Itoa(count)+
			" but -servers is "+strconv.Itoa(o.servers)+": there are only that many server namespaces to serve from", nil)
	}
	if protocol == probe.ProtocolICMP {
		return 0, 0, apperr.InvalidArgument(op, "-service-ports asks for a listener and -protocol icmp "+
			"needs none: an echo request is answered by the network itself", nil)
	}
	if protocol == probe.ProtocolDNS {
		return 0, 0, apperr.Unsupported(op, "-protocol dns cannot be observed from the command line: "+
			"an observation here carries no name to ask the resolver about, so a resolver would answer nothing", nil)
	}
	base64 := uint64(o.serviceBasePort)
	if base64 == 0 {

		base64 = uint64(o.port)
		if base64 == 0 {
			return 0, 0, apperr.Validation(op, "a service needs a port to bind: -port is 0 and "+
				"-service-base-port was not given", nil)
		}
	}
	last := base64 + uint64(count) - 1
	if base64 > maxPort || last > maxPort {
		return 0, 0, apperr.InvalidArgument(op, "-service-base-port is "+
			strconv.FormatUint(base64, 10)+" and "+strconv.Itoa(count)+
			" namespaces would bind up to "+strconv.FormatUint(last, 10)+
			", which is past the last port", nil)
	}
	if uint64(o.port) < base64 || uint64(o.port) > last {
		return 0, 0, apperr.Validation(op, "-port "+strconv.FormatUint(uint64(o.port), 10)+
			" is not one of the ports -service-ports "+strconv.Itoa(count)+" asks for, which are "+
			strconv.FormatUint(base64, 10)+" to "+strconv.FormatUint(last, 10), nil)
	}
	return count, uint16(base64), nil
}

const maxPort = 65535

func (o *experimentOptions) serviceSpecs(cidr string) ([]network.ServiceSpec, error) {
	const op = "cli.experiment.run"
	count, base, err := o.serviceRange()
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	plan, err := network.BuildPlan(o.labIdentifier(), cidr, maxInt(o.clients, 1), o.servers, 1500)
	if err != nil {
		return nil, apperr.Wrap(err, kindOf(err), op, "the laboratory could not be planned")
	}
	servers := plan.Servers()
	if len(servers) < count {
		return nil, apperr.InvalidArgument(op, "-service-ports is "+strconv.Itoa(count)+
			" but the plan has "+strconv.Itoa(len(servers))+" servers", nil)
	}
	protocol := probe.Protocol(strings.ToLower(strings.TrimSpace(o.protocol)))
	out := make([]network.ServiceSpec, 0, count)
	for i := 0; i < count; i++ {
		built, err := network.ServiceFor(servers[i].Namespace, protocol, base+uint16(i))
		if err != nil {
			return nil, apperr.Wrap(err, kindOf(err), op,
				"-service-ports asked for an endpoint NetForge does not serve")
		}
		out = append(out, built)
	}
	if err := o.checkAnswered(out, servers); err != nil {
		return nil, err
	}
	return out, nil
}

func (o *experimentOptions) checkAnswered(specs []network.ServiceSpec, servers []network.Node) error {
	const op = "cli.experiment.run"
	protocol := probe.Protocol(strings.ToLower(strings.TrimSpace(o.protocol)))
	if !protocol.NeedsListener() || len(specs) == 0 {
		return nil
	}
	served := make([]string, 0, len(specs))
	for i := range specs {
		if i >= len(servers) {
			break
		}
		address := servers[i].Address.Addr().String()
		if address == o.target {
			return nil
		}
		served = append(served, servers[i].Namespace+" "+address)
	}
	return apperr.Validation(op, "-target "+quote(o.target)+" is not served by this run: "+
		"-service-ports asks "+strings.Join(served, ", ")+
		", so a "+string(protocol)+" probe of it would be answered by nothing", nil)
}

func (o *experimentOptions) labIdentifier() string {
	if o.id != "" {
		return o.id
	}
	return "unidentified"
}

func maxInt(value, floor int) int {
	if value > floor {
		return value
	}
	return floor
}

func kindOf(err error) apperr.Kind {
	if kind, ok := apperr.KindOf(err); ok {
		return kind
	}
	return apperr.KindInternal
}

func (o *experimentOptions) checkOutput() error {
	switch o.output {
	case "text", "json":
		return nil
	}
	return apperr.Unsupported("cli.experiment.run",
		"-output "+quote(o.output)+" is not supported, want text or json", nil)
}

func (o *experimentOptions) writeReport(w io.Writer, run experiment.Report, laboratory *network.Summary) error {
	if o.output == "json" {
		return report.EncodeJSON(w, run, laboratory)
	}
	return report.EncodeText(w, run, laboratory)
}

func (o *experimentOptions) labSpec(document config.Config, experimentID string) (network.Spec, error) {
	const op = "cli.experiment.run"
	if o.clients < 1 || o.servers < 1 {
		return network.Spec{}, apperr.InvalidArgument(op,
			"-clients and -servers must each be at least 1", nil)
	}
	if o.clients > 8 || o.servers > 8 {
		return network.Spec{}, apperr.InvalidArgument(op,
			"a laboratory is limited to 8 clients and 8 servers", nil)
	}
	routing, err := o.routingMode()
	if err != nil {
		return network.Spec{}, err
	}
	spec := network.Spec{
		Network: document.Scope.Network,
		Clients: o.clients,
		Servers: o.servers,
		MTU:     1500,
		Names:   document.Scope.Namespaces,

		ID:       o.id,
		MaxNames: document.Limits.MaxNamespaces,
		MaxIfs:   document.Limits.MaxInterfaces,

		Routing: routing,

		Chaos: chaos.Limits{
			MaxFaults:    chaos.CeilingFaults,
			MaxDuration:  document.Limits.MaxDuration.Duration(),
			MaxLatency:   chaos.CeilingLatency,
			MaxBandwidth: uint64(document.Limits.MaxBandwidth),
			MaxQueue:     chaos.CeilingQueue,
			MaxGap:       chaos.CeilingGap,
		},

		Probes: probe.Limits{
			MaxDuration:   experiment.DefaultLimits().MaxStep,
			MaxConcurrent: 2,
		},
	}

	if spec.ID == "" {
		spec.ID = experimentID
	}
	o.id = spec.ID
	if spec.Services, err = o.serviceSpecs(spec.Network); err != nil {
		return network.Spec{}, err
	}
	return spec, nil
}

func buildLaboratory(ctx context.Context, spec network.Spec) (*network.Lab, error) {
	return network.Create(ctx, spec)
}

func (o *experimentOptions) observation() (experiment.Observation, error) {
	protocol := probe.Protocol(strings.ToLower(strings.TrimSpace(o.protocol)))
	if !protocol.Valid() {
		return experiment.Observation{}, apperr.Unsupported("cli.experiment.plan",
			"protocol "+quote(o.protocol)+" is not a protocol a laboratory measures, want one of "+
				protocolList(), nil)
	}
	if o.count <= 0 {
		return experiment.Observation{}, apperr.InvalidArgument("cli.experiment.plan",
			"-count must be positive", nil)
	}
	if o.size <= 0 {
		return experiment.Observation{}, apperr.InvalidArgument("cli.experiment.plan",
			"-size must be positive", nil)
	}
	if o.port > 65535 {
		return experiment.Observation{}, apperr.InvalidArgument("cli.experiment.plan",
			"-port "+strconv.FormatUint(uint64(o.port), 10)+" is not a port", nil)
	}
	return experiment.Observation{
		Namespace:  o.namespace,
		Target:     o.target,
		Port:       uint16(o.port),
		Protocol:   protocol,
		AfterFault: o.afterFault,
		Settle:     o.settle,
		Options: probe.Options{
			Count:    o.count,
			Size:     o.size,
			Timeout:  o.timeout,
			Deadline: o.deadline,
		},
	}, nil
}

func (o *experimentOptions) requests() ([]recovery.Request, error) {
	out := make([]recovery.Request, 0, len(o.recover))
	for _, entry := range o.recover {
		action, target, found := strings.Cut(entry, ":")
		if !found || strings.TrimSpace(target) == "" {
			return nil, apperr.InvalidArgument("cli.experiment.plan",
				"-recover "+quote(entry)+" is not action:target", nil)
		}
		out = append(out, recovery.Request{
			Action:  recovery.Action(strings.TrimSpace(action)),
			Target:  strings.TrimSpace(target),
			Reason:  o.reason,
			Consent: o.authorize,
		})
	}
	return out, nil
}

func recoverySubcommands() map[string]*Command {
	return map[string]*Command{
		"actions": recoveryActions(),
	}
}

func recoveryCommand() *Command {
	return &Command{
		Name:    "recovery",
		Summary: "Inspect the recovery actions NetForge will perform.",
		Usage:   "netforge recovery <actions>",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			subcommands := recoverySubcommands()
			if len(args) == 0 {
				writeGroupHelp(session.Env, session.Env.stderr(), commands()["recovery"], subcommands)
				return apperr.InvalidArgument("cli.recovery", "missing subcommand; run "+session.Env.program()+" help recovery", nil)
			}
			subcommand, ok := subcommands[args[0]]
			if !ok {
				return apperr.InvalidArgument("cli.recovery", "unknown subcommand "+quote(args[0])+"; run "+session.Env.program()+" help recovery", nil)
			}
			return runCommand(ctx, session, subcommand, args[1:])
		},
	}
}

func recoveryActions() *Command {
	return &Command{
		Name:    "actions",
		Parent:  "recovery",
		Summary: "Print the recovery actions NetForge will perform, and what each one requires.",
		Usage:   "netforge recovery actions",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			if err := rejectExtraArgs("recovery actions", args); err != nil {
				return err
			}
			out := session.Env.stdout()
			for _, action := range recovery.Actions() {
				kind := "additive"
				if action.Destructive() {
					kind = "destructive"
				}
				fmt.Fprintf(out, "%-20s %-12s %s\n", action, kind, actionTarget(action))
			}
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "a recovery requires: explicit authorization + scope validation + action validation")
			fmt.Fprintln(out, "                       + actual recovery + verification by measurement")
			return nil
		},
	}
}

func actionTarget(action recovery.Action) string {
	switch action {
	case recovery.ActionRemoveBackend, recovery.ActionRestoreBackend,
		recovery.ActionDrainConnection, recovery.ActionReEnableBackend:
		return "backend endpoint, inside scope.network"
	case recovery.ActionRestoreRoute:
		return "route destination, inside scope.network"
	default:
		return "namespace in the scope"
	}
}

func protocolList() string {
	names := make([]string, 0, len(probe.Protocols()))
	for _, protocol := range probe.Protocols() {
		names = append(names, string(protocol))
	}
	return strings.Join(names, ", ")
}
