package namespace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	agentBuffer = 1 << 20
	// holderBuffer bounds the read on a holder's control pipe. The agent never
	// writes to it, so anything that arrives is discarded; the bound exists so
	// that a peer which found the pipe cannot keep a holder reading, because a
	// holder that never returns is a namespace that is never released.
	holderBuffer = 1 << 16
)

const AgentEnv = "NETFORGE_LAB_AGENT"

const (
	agentArgv  = "netforge-lab-agent"
	holderArgv = "netforge-namespace-holder"
)

func Dispatch() (bool, int) {
	if os.Getenv(AgentEnv) == "1" {
		return true, AgentMain()
	}
	for _, arg := range os.Args[1:] {
		if arg == holderArgv {
			return true, HolderMain()
		}
	}
	return false, 0
}

// HolderMain holds a network namespace open for as long as the laboratory agent
// that created it is alive.
//
// The agent holds the write end of a pipe whose read end is this process's
// standard input. The kernel closes that end when the agent exits, however it
// exits, so the read below returns and this process leaves. That matters
// because the kernel destroys a namespace when the last process inside it is
// gone: a holder that waits only for a signal is an orphan after a run that was
// killed rather than stopped, and it would hold its namespace, and its veths,
// open for as long as the machine stayed up.
//
// A terminal is not a pipe and is not watched, so a holder started by hand keeps
// waiting for a signal instead of returning at once.
func HolderMain() int {
	end := make(chan struct{})
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0 {
		go func() {
			defer close(end)
			_, _ = io.Copy(io.Discard, io.LimitReader(os.Stdin, holderBuffer))
		}()
	} else {
		close(end)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-end:
	case <-stop:
	}
	return 0
}

type request struct {
	ID   int      `json:"id"`
	Op   string   `json:"op"`
	Argv []string `json:"argv,omitempty"`
	PID  int      `json:"pid,omitempty"`
}

type response struct {
	ID     int    `json:"id"`
	Op     string `json:"op,omitempty"`
	Err    string `json:"error,omitempty"`
	Code   int    `json:"code"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	PID    int    `json:"pid,omitempty"`
}

type outcome struct {
	code   int
	stdout string
	stderr string
}

type daemonProcess struct {
	pid  int
	feed *os.File
}

var (
	daemonMu sync.Mutex
	daemons  []*daemonProcess
	// holders are the control pipes that keep the namespace holders alive. They
	// are held here rather than in the goroutine that made them so the garbage
	// collector cannot close a pipe this agent is still relying on.
	holders []*os.File
)

func trackDaemon(entry *daemonProcess) {
	daemonMu.Lock()
	defer daemonMu.Unlock()
	daemons = append(daemons, entry)
}

func holdPipe(feed *os.File) {
	daemonMu.Lock()
	defer daemonMu.Unlock()
	holders = append(holders, feed)
}

func releasePipes() {
	daemonMu.Lock()
	pipes := holders
	holders = nil
	daemonMu.Unlock()
	for _, feed := range pipes {
		_ = feed.Close()
	}
}

func stopDaemons() {
	daemonMu.Lock()
	list := daemons
	daemons = nil
	daemonMu.Unlock()
	// Closing the holders' pipes first is what tells them the laboratory is
	// over; a holder that exits releases its namespace without waiting for a
	// signal from a process that is on its way out anyway.
	releasePipes()
	for _, entry := range list {
		if entry.feed != nil {
			_ = entry.feed.Close()
		}
		_ = syscall.Kill(entry.pid, syscall.SIGKILL)
	}
}

func startDaemon(pid int, argv []string) (int, error) {
	if len(argv) == 0 {
		return 0, errors.New("no command given")
	}
	read, feed, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("create the control pipe: %w", err)
	}
	child := 0
	err = inNamespace(pid, func() error {
		command := exec.Command(argv[0], argv[1:]...)
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
		command.Stdin = read
		command.Stdout = nil
		command.Stderr = nil
		if err := command.Start(); err != nil {
			return err
		}
		child = command.Process.Pid
		go func() { _ = command.Wait() }()
		return nil
	})
	_ = read.Close()
	if err != nil {
		_ = feed.Close()
		return 0, fmt.Errorf("start %s: %w", argv[0], err)
	}
	trackDaemon(&daemonProcess{pid: child, feed: feed})
	return child, nil
}

func AgentMain() int {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		fmt.Fprintln(os.Stderr, "lab agent: stdin is not a pipe; refusing to run attached to a terminal")
		return 1
	}
	defer stopDaemons()
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, agentBuffer))
	encoder := json.NewEncoder(os.Stdout)
	self := os.Getpid()
	for {
		var req request
		if err := decoder.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			return 1
		}
		if err := encoder.Encode(serve(req, self)); err != nil {
			return 1
		}
	}
}

func serve(req request, self int) response {
	switch req.Op {
	case "ping":
		return response{ID: req.ID, Op: req.Op, PID: self}
	case "spawn":
		return spawn(req)
	case "exec":
		reply := response{ID: req.ID, Op: req.Op}
		result, err := execInNamespace(req.PID, req.Argv)
		if err != nil {
			reply.Err = err.Error()
			reply.Code = -1
			return reply
		}
		reply.Code = result.code
		reply.Stdout = result.stdout
		reply.Stderr = result.stderr
		return reply
	case "write":
		return write(req)
	case "daemon":
		reply := response{ID: req.ID, Op: req.Op}
		child, err := startDaemon(req.PID, req.Argv)
		if err != nil {
			reply.Err = err.Error()
			reply.Code = -1
			return reply
		}
		reply.Code = 0
		reply.PID = child
		return reply
	case "shutdown":
		return response{ID: req.ID, Op: req.Op, PID: self}
	}
	return response{ID: req.ID, Op: req.Op, Err: "unknown operation " + strconv.Quote(req.Op), Code: -1}
}

func write(req request) response {
	reply := response{ID: req.ID, Op: req.Op}
	if len(req.Argv) != 2 || !strings.HasPrefix(req.Argv[0], "/proc/sys/net/") {
		reply.Err = "only a /proc/sys/net path with a value may be written"
		reply.Code = -1
		return reply
	}
	err := inNamespace(req.PID, func() error {
		return os.WriteFile(req.Argv[0], []byte(req.Argv[1]), 0o644)
	})
	if err != nil {
		reply.Err = err.Error()
		reply.Code = -1
		return reply
	}
	reply.Code = 0
	return reply
}

func spawn(req request) response {
	self, err := os.Executable()
	if err != nil {
		return response{ID: req.ID, Op: req.Op, Err: err.Error(), Code: -1}
	}
	read, feed, err := os.Pipe()
	if err != nil {
		return response{ID: req.ID, Op: req.Op, Err: fmt.Sprintf("create the holder pipe: %v", err), Code: -1}
	}
	command := exec.Command(self, holderArgv)
	command.Env = append(os.Environ(), AgentEnv+"=")
	// The holder watches this pipe rather than waiting for a signal, because a
	// signal is only ever sent by a process that is still alive to send it.
	command.Stdin = read
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	if err := command.Start(); err != nil {
		_ = read.Close()
		_ = feed.Close()
		return response{ID: req.ID, Op: req.Op, Err: err.Error(), Code: -1}
	}
	// The holder has its own copy of the read end now. Closing this one is what
	// lets the holder see the end of the pipe when this agent goes away.
	_ = read.Close()
	holdPipe(feed)
	pid := command.Process.Pid
	go func() { _ = command.Wait() }()
	return response{ID: req.ID, Op: req.Op, PID: pid}
}

func execInNamespace(pid int, argv []string) (outcome, error) {
	if len(argv) == 0 {
		return outcome{}, errors.New("no command given")
	}
	var result outcome
	err := inNamespace(pid, func() error {
		command := exec.Command(argv[0], argv[1:]...)
		command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
		var stdout, stderr strings.Builder
		command.Stdout = &stdout
		command.Stderr = &stderr
		runErr := command.Run()
		result.code = 0
		if runErr != nil {
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) {
				return fmt.Errorf("run %s: %w", argv[0], runErr)
			}
			result.code = exit.ExitCode()
		}
		result.stdout = stdout.String()
		result.stderr = stderr.String()
		return nil
	})
	if err != nil {
		return outcome{}, err
	}
	return result, nil
}

func inNamespace(pid int, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	home, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("open the current network namespace: %w", err)
	}
	defer home.Close()
	target, err := os.Open("/proc/" + strconv.Itoa(pid) + "/ns/net")
	if err != nil {
		return fmt.Errorf("open the network namespace of pid %d: %w", pid, err)
	}
	defer target.Close()
	if err := setns(int(target.Fd()), targetNSNet); err != nil {
		return fmt.Errorf("enter the network namespace of pid %d: %w", pid, err)
	}
	defer func() { _ = setns(int(home.Fd()), targetNSNet) }()
	return fn()
}
