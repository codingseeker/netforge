package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/reconcile"
)

// The reconcile commands are the only ones here that delete anything. Listing is
// safe: it reads a directory and prints what it can positively identify, and it
// identifies nothing it cannot prove. Reclaiming is not, which is why it needs
// the same three things a recovery needs — a name, an explicit authorization and
// a written reason — and why it will not act on a directory whose owning process
// is still running.

// reconcileOptions is what the reclaim subcommand takes. The names are names and
// not paths, so a request cannot carry a way out of the state root.
type reconcileOptions struct {
	stateRoot string
	names     stringList
	authorize bool
	reason    string
	older     time.Duration
}

func reconcileSubcommands() map[string]*Command {
	return map[string]*Command{
		"list":    reconcileList(),
		"reclaim": reconcileReclaim(),
	}
}

func reconcileCommand() *Command {
	return &Command{
		Name:    "reconcile",
		Summary: "List and reclaim the state a killed run left behind.",
		Usage:   "netforge reconcile <list|reclaim> [flags]",
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			subcommands := reconcileSubcommands()
			if len(args) == 0 {
				writeGroupHelp(session.Env, session.Env.stderr(), commands()["reconcile"], subcommands)
				return apperr.InvalidArgument("cli.reconcile", "missing subcommand; run "+session.Env.program()+" help reconcile", nil)
			}
			subcommand, ok := subcommands[args[0]]
			if !ok {
				return apperr.InvalidArgument("cli.reconcile", "unknown subcommand "+quote(args[0])+"; run "+session.Env.program()+" help reconcile", nil)
			}
			return runCommand(ctx, session, subcommand, args[1:])
		},
	}
}

// stateRootFlags registers the flags both subcommands take. The state root is a
// flag rather than a fixed path so a host whose temporary directory is shared
// with software NetForge does not control can be reconciled in isolation, and so
// a test can reconcile a directory of its own.
func stateRootFlags(fs *flag.FlagSet, opts *reconcileOptions) {
	fs.StringVar(&opts.stateRoot, "state-root", "",
		"the directory NetForge keeps its state in; default "+reconcile.StateRootEnv+" or the system temporary directory")
}

func reconcileList() *Command {
	return &Command{
		Name:    "list",
		Parent:  "reconcile",
		Summary: "List what NetForge believes it left behind, and identify nothing it cannot prove.",
		Usage:   "netforge reconcile list [flags]",
		Setup: func(fs *flag.FlagSet) any {
			opts := &reconcileOptions{}
			stateRootFlags(fs, opts)
			return opts
		},
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			if err := rejectExtraArgs("reconcile list", args); err != nil {
				return err
			}
			opts, _ := options.(*reconcileOptions)
			if opts == nil {
				opts = &reconcileOptions{}
			}
			root, err := resolveRoot(opts.stateRoot)
			if err != nil {
				return err
			}
			entries, err := reconcile.Scan(root, time.Now())
			if err != nil {
				return err
			}
			return writeListing(session.Env.stdout(), root, entries)
		},
	}
}

// writeListing prints the state root, its entries, and what was and was not
// identified.
//
// The unidentified are listed rather than hidden, and with no identifier: a
// reader has to be able to see that something is there, and this listing is not
// allowed to be the thing that claims it.
func writeListing(out io.Writer, root string, entries []reconcile.Entry) error {
	if _, err := fmt.Fprintf(out, "state root: %s\n", root); err != nil {
		return apperr.IO("cli.reconcile.list", "the listing could not be written", err)
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintln(out, "nothing is there: every run that used this state root cleaned up after itself")
		if err != nil {
			return apperr.IO("cli.reconcile.list", "the listing could not be written", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(out, "%s%s%s%s%s\n",
		header("NAME", 32), header("KIND", 14), header("ID", 20),
		header("AGE", 16), "STATE"); err != nil {
		return apperr.IO("cli.reconcile.list", "the listing could not be written", err)
	}
	owned, foreign := 0, 0
	for _, entry := range entries {
		if entry.Identified {
			owned++
		} else {
			foreign++
		}
		if _, err := fmt.Fprintln(out, entry.Describe()); err != nil {
			return apperr.IO("cli.reconcile.list", "the listing could not be written", err)
		}
	}
	if _, err := fmt.Fprintf(out, "\n%d identified as NetForge's, %d not identified and left alone\n",
		owned, foreign); err != nil {
		return apperr.IO("cli.reconcile.list", "the listing could not be written", err)
	}
	if foreign > 0 {
		// The state root is shared, so an unidentified entry is normal rather
		// than alarming, and the listing says what it did about them instead of
		// leaving it to be inferred from the word "unidentified".
		if _, err := fmt.Fprintln(out,
			"the rest belong to something else and are never candidates for removal: this state root is shared"); err != nil {
			return apperr.IO("cli.reconcile.list", "the listing could not be written", err)
		}
	}
	if owned > 0 {
		if _, err := fmt.Fprintln(out,
			"reclaim one with: netforge reconcile reclaim -id <name> -authorize -why \"<reason>\""); err != nil {
			return apperr.IO("cli.reconcile.list", "the listing could not be written", err)
		}
	}
	return nil
}

func reconcileReclaim() *Command {
	return &Command{
		Name:    "reclaim",
		Parent:  "reconcile",
		Summary: "Remove the state directories a killed run left, once they are named, authorized and justified.",
		Usage:   "netforge reconcile reclaim [flags]",
		Setup: func(fs *flag.FlagSet) any {
			opts := &reconcileOptions{}
			stateRootFlags(fs, opts)
			fs.Var(&opts.names, "id", "the name of a state directory to reclaim; may be given more than once")
			fs.BoolVar(&opts.authorize, "authorize", false,
				"authorize the removal; without it the reclaim is refused")
			fs.StringVar(&opts.reason, "why", "", "why the removal was authorized")
			fs.DurationVar(&opts.older, "older-than", 0,
				"only reclaim directories at least this old; a directory younger than this is refused")
			return opts
		},
		Run: func(ctx context.Context, session *Session, options any, args []string) error {
			if err := rejectExtraArgs("reconcile reclaim", args); err != nil {
				return err
			}
			opts, _ := options.(*reconcileOptions)
			if opts == nil {
				opts = &reconcileOptions{}
			}
			root, err := resolveRoot(opts.stateRoot)
			if err != nil {
				return err
			}
			// The authorization is built before anything is read from disk, so a
			// reclaim that is not permitted costs no filesystem call and cannot
			// half-happen.
			authorization, err := reconcile.Authorize(root, reconcile.Request{
				Names:   opts.names,
				Reason:  opts.reason,
				Consent: opts.authorize,
			}, reconcile.DefaultLimits(), time.Now())
			if err != nil {
				return err
			}
			if opts.older > 0 {
				if err := ageGate(root, opts.names, opts.older, time.Now()); err != nil {
					return err
				}
			}
			report, err := reconcile.Reclaim(ctx, root, authorization, time.Now())
			if err != nil {
				return err
			}
			if err := writeReclaim(session.Env.stdout(), report); err != nil {
				return err
			}
			session.Logger.Info("state reconciled",
				"removed", len(report.Names()),
				"refused", report.Refused,
				"reason", report.Reason)
			if report.Refused > 0 {
				// A reclaim that refused something has not done what it was asked
				// to do, and saying so is better than a report a script reads as
				// success.
				return apperr.Conflict("cli.reconcile.reclaim",
					"nothing was reclaimed for "+plural(report.Refused, "name", "names")+
						" that was refused; nothing was removed that is still in use or is not NetForge's", nil)
			}
			return nil
		},
	}
}

// ageGate refuses a directory younger than the operator's threshold. It is a
// separate check because an operator who says "older than an hour" is expressing
// a fear about a run that is still starting, and that fear is worth honouring
// even though the marker already refuses a live owner.
func ageGate(root string, names []string, older time.Duration, now time.Time) error {
	const op = "cli.reconcile.reclaim"
	entries, err := reconcile.Scan(root, now)
	if err != nil {
		return err
	}
	byName := make(map[string]reconcile.Entry, len(entries))
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	for _, name := range names {
		entry, found := byName[strings.TrimSpace(name)]
		if !found {
			// It is not there, which is the same answer as having been removed
			// already, and the reclaim says so rather than failing here.
			continue
		}
		if entry.Age < older {
			return apperr.Conflict(op, "-id "+quote(entry.Name)+" is only "+
				entry.Age.Round(time.Second).String()+" old, and -older-than asks for "+
				older.String(), nil)
		}
	}
	return nil
}

func writeReclaim(out io.Writer, report reconcile.Report) error {
	lines := []string{
		"state root: " + report.Root,
		"reason:     " + report.Reason,
		"authorized: " + report.GrantedAt.Format(time.RFC3339),
		"",
	}
	for _, outcome := range report.Outcomes {
		lines = append(lines, outcome.Name+"  "+outcome.Reason)
	}
	lines = append(lines, "",
		"removed "+strconv.Itoa(report.Removed)+", refused "+strconv.Itoa(report.Refused)+
			", already gone "+strconv.Itoa(report.Absent))
	if _, err := fmt.Fprintln(out, strings.Join(lines, "\n")); err != nil {
		return apperr.IO("cli.reconcile.reclaim", "the report could not be written", err)
	}
	return nil
}

// resolveRoot decides which state root to act on. The environment variable is
// read here rather than inside the package so that a command line flag always
// wins over the environment, and so the value a run acted on is the value its
// output names.
func resolveRoot(flagValue string) (string, error) {
	const op = "cli.reconcile"
	root := strings.TrimSpace(flagValue)
	if root == "" {
		root = reconcile.StateRoot()
	}
	if strings.TrimSpace(root) == "" {
		return "", apperr.InvalidArgument(op,
			"the state root could not be determined; pass -state-root or set "+reconcile.StateRootEnv, nil)
	}
	return root, nil
}

func plural(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return strconv.Itoa(count) + " " + many
}

// header renders a listing column heading at the width its entries use, so the
// heading and the values below it line up without either being hard-coded twice.
func header(name string, width int) string {
	if len(name) >= width {
		return name + " "
	}
	return name + strings.Repeat(" ", width-len(name))
}
