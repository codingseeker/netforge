package namespace

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const MaxNameLength = 63

var ErrNotFound = errors.New("namespace not found")

func ValidateName(op, field, name string) error {
	if name == "" {
		return fmt.Errorf("%s: %s must not be empty", op, field)
	}
	if len(name) > MaxNameLength {
		return fmt.Errorf("%s: %s is %d characters, the maximum is %d", op, field, len(name), MaxNameLength)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return fmt.Errorf("%s: %s contains %q, only lowercase letters, digits and - are allowed",
				op, field, string(c))
		}
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("%s: %s must not start or end with -", op, field)
	}
	return nil
}

// Result is the outcome of a command that ran inside a namespace.
type Result struct {
	// Stdout and Stderr are what the command printed.
	Stdout string
	Stderr string
	// Code is the exit status. A command that ran but failed reports a
	// nonzero code here rather than an error, because a nonzero status is
	// often the answer the caller asked for: a probe that lost packets, a
	// route that does not exist.
	Code int
}

// Failed reports whether the command ran and reported a nonzero exit status.
func (r Result) Failed() bool { return r.Code != 0 }

type Lab struct {
	command  *exec.Cmd
	stdin    *os.File
	encoder  *json.Encoder
	reader   *bufio.Reader
	mu       sync.Mutex
	nextID   int
	root     *Space
	spacesMu sync.Mutex
	spaces   map[string]*Space
	closed   bool
}

func NewLab() (*Lab, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate the executable: %w", err)
	}
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("agent input pipe: %w", err)
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		_ = inRead.Close()
		_ = inWrite.Close()
		return nil, fmt.Errorf("agent output pipe: %w", err)
	}
	command := exec.Command(self, agentArgv)
	command.Env = append(os.Environ(), AgentEnv+"=1")
	command.Stdin = inRead
	command.Stdout = outWrite
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		GidMappingsEnableSetgroups: false,
	}
	if err := command.Start(); err != nil {
		_ = inRead.Close()
		_ = inWrite.Close()
		_ = outRead.Close()
		_ = outWrite.Close()
		return nil, fmt.Errorf("start the laboratory agent: %w", err)
	}
	_ = inRead.Close()
	_ = outWrite.Close()
	lab := &Lab{
		command: command,
		stdin:   inWrite,
		encoder: json.NewEncoder(inWrite),
		reader:  bufio.NewReaderSize(outRead, 64*1024),
		spaces:  map[string]*Space{},
	}
	reply, err := lab.call(request{Op: "ping"})
	if err != nil {
		_ = lab.Close()
		return nil, fmt.Errorf("the laboratory agent did not answer: %w", err)
	}
	lab.root = &Space{Name: "lab", pid: reply.PID, lab: lab}
	lab.spaces["lab"] = lab.root
	return lab, nil
}

func (l *Lab) Root() *Space { return l.root }

func (l *Lab) Spawn(name string) (*Space, error) {
	if err := ValidateName("namespace.Spawn", "name", name); err != nil {
		return nil, err
	}
	l.spacesMu.Lock()
	if _, exists := l.spaces[name]; exists {
		l.spacesMu.Unlock()
		return nil, fmt.Errorf("namespace %s already exists in this laboratory", name)
	}
	l.spacesMu.Unlock()
	reply, err := l.call(request{Op: "spawn"})
	if err != nil {
		return nil, err
	}
	space := &Space{Name: name, pid: reply.PID, lab: l}
	l.spacesMu.Lock()
	l.spaces[name] = space
	l.spacesMu.Unlock()
	return space, nil
}

func (l *Lab) Lookup(name string) (*Space, error) {
	l.spacesMu.Lock()
	space, ok := l.spaces[name]
	l.spacesMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if !space.Alive() {
		return nil, fmt.Errorf("%w: %s is no longer running", ErrNotFound, name)
	}
	return space, nil
}

func (l *Lab) Names() []string {
	l.spacesMu.Lock()
	defer l.spacesMu.Unlock()
	names := make([]string, 0, len(l.spaces))
	for name := range l.spaces {
		names = append(names, name)
	}
	return names
}

func (l *Lab) exec(pid int, argv ...string) (Result, error) {
	reply, err := l.call(request{Op: "exec", PID: pid, Argv: argv})
	if err != nil {
		return Result{}, err
	}
	return Result{Stdout: reply.Stdout, Stderr: reply.Stderr, Code: reply.Code}, nil
}

func (l *Lab) call(req request) (response, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return response{}, errors.New("the laboratory is closed")
	}
	l.nextID++
	req.ID = l.nextID
	if err := l.encoder.Encode(req); err != nil {
		return response{}, fmt.Errorf("send %s: %w", req.Op, err)
	}
	line, err := l.reader.ReadBytes('\n')
	if err != nil {
		return response{}, fmt.Errorf("read the reply to %s: %w", req.Op, err)
	}
	var reply response
	if err := json.Unmarshal(line, &reply); err != nil {
		return response{}, fmt.Errorf("decode the reply to %s: %w", req.Op, err)
	}
	if reply.ID != req.ID {
		return response{}, fmt.Errorf("reply %d does not answer request %d", reply.ID, req.ID)
	}
	if reply.Err != "" {
		return response{}, errors.New(reply.Err)
	}
	return reply, nil
}

func (l *Lab) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()

	l.spacesMu.Lock()
	spaces := make([]*Space, 0, len(l.spaces))
	for _, space := range l.spaces {
		if space == l.root {
			continue
		}
		spaces = append(spaces, space)
	}
	l.spaces = map[string]*Space{}
	l.root = nil
	l.spacesMu.Unlock()

	var failure error
	for _, space := range spaces {
		if err := space.Close(); err != nil && failure == nil {
			failure = err
		}
	}
	_ = l.stdin.Close()
	if l.command.Process != nil {
		_ = l.command.Process.Kill()
		_, _ = l.command.Process.Wait()
	}
	return failure
}

type Space struct {
	Name string
	pid  int
	lab  *Lab
}

func (s *Space) PID() int {
	if s == nil {
		return 0
	}
	return s.pid
}

func (s *Space) ID() (string, error) {
	if s == nil || s.pid == 0 {
		return "", ErrNotFound
	}
	target, err := os.Readlink("/proc/" + strconv.Itoa(s.pid) + "/ns/net")
	if err != nil {
		return "", fmt.Errorf("read the namespace of %s: %w", s.Name, err)
	}
	return target, nil
}

func (s *Space) Alive() bool {
	if s == nil || s.pid == 0 {
		return false
	}
	return syscall.Kill(s.pid, 0) == nil
}

func (s *Space) Exec(argv ...string) (string, string, error) {
	result, err := s.Run(argv...)
	if err != nil {
		return "", "", err
	}
	return result.Stdout, result.Stderr, nil
}

// Run executes a command in the namespace and reports its exit status. A
// caller that has to know whether the command succeeded uses this; a caller
// that only reads the output of a command that may fail harmlessly uses Exec.
func (s *Space) Run(argv ...string) (Result, error) {
	if s == nil || s.pid == 0 {
		return Result{}, ErrNotFound
	}
	return s.lab.exec(s.pid, argv...)
}

func (s *Space) Start(argv ...string) (int, error) {
	if s == nil || s.pid == 0 {
		return 0, ErrNotFound
	}
	if len(argv) == 0 {
		return 0, errors.New("no command given")
	}
	reply, err := s.lab.call(request{Op: "daemon", PID: s.pid, Argv: argv})
	if err != nil {
		return 0, err
	}
	return reply.PID, nil
}

func (s *Space) Write(path, value string) (string, error) {
	if s == nil || s.pid == 0 {
		return "", ErrNotFound
	}
	clean, err := procSysNetPath(path)
	if err != nil {
		return "", err
	}
	reply, err := s.lab.call(request{Op: "write", PID: s.pid, Argv: []string{clean, value}})
	if err != nil {
		return "", err
	}
	return reply.Stderr, nil
}

func (s *Space) Read(path string) (string, error) {
	if s == nil || s.pid == 0 {
		return "", ErrNotFound
	}
	clean, err := procSysNetPath(path)
	if err != nil {
		return "", err
	}
	stdout, _, err := s.Exec("cat", clean)
	if err != nil {
		return "", err
	}
	return stdout, nil
}

func procSysNetPath(name string) (string, error) {
	const root = "/proc/sys/net"
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	if clean == root || !strings.HasPrefix(clean, root+"/") {
		return "", fmt.Errorf("refusing %s: only %s is reachable inside a laboratory", name, root)
	}
	if strings.ContainsRune(clean, 0) {
		return "", fmt.Errorf("refusing %s: the path holds a zero byte", name)
	}
	return clean, nil
}

func processGone(pid int) (bool, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true, nil
	}
	start := strings.Index(string(data), "(")
	end := strings.LastIndex(string(data), ")")
	if start < 0 || end <= start {
		return false, nil
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) == 0 {
		return false, nil
	}
	return fields[0] == "Z", nil
}

func (s *Space) Close() error {
	if s == nil || s.pid == 0 {
		return nil
	}
	pid := s.pid
	s.pid = 0
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("destroy namespace %s: %w", s.Name, err)
	}
	for i := 0; i < 2000; i++ {
		gone, err := processGone(pid)
		if err != nil {
			return nil
		}
		if gone {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("namespace %s did not exit", s.Name)
}
