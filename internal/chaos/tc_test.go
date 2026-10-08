package chaos

import (
	"strings"
	"testing"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

const (
	kernelNetemDelay = `[{"kind":"netem","handle":"800c:","root":true,"refcnt":13,` +
		`"options":{"limit":1000,"delay":{"delay":0.05,"jitter":0,"correlation":0},` +
		`"seed":15287240990339982236,"ecn":false,"gap":0}}]`

	kernelNetemJitter = `[{"kind":"netem","handle":"1:","root":true,"refcnt":13,` +
		`"options":{"limit":1000,"delay":{"delay":0.05,"jitter":0.008,"correlation":0.25},` +
		`"seed":9152703592454320130,"ecn":false,"gap":0}}]`

	kernelNetemLoss = `[{"kind":"netem","handle":"8004:","root":true,"refcnt":13,` +
		`"options":{"limit":1000,"loss-random":{"loss":0.1,"correlation":0.3},` +
		`"seed":6476391252298570975,"ecn":false,"gap":0}}]`

	kernelNetemDuplicate = `[{"kind":"netem","handle":"1:","root":true,"refcnt":13,` +
		`"options":{"limit":1000,"duplicate":{"duplicate":0.2,"correlation":0},` +
		`"seed":16183979352400195304,"ecn":false,"gap":0}}]`

	kernelNetemReorder = `[{"kind":"netem","handle":"8004:","root":true,"refcnt":13,` +
		`"options":{"limit":1000,"delay":{"delay":0.02,"jitter":0,"correlation":0},` +
		`"reorder":{"reorder":0.25,"correlation":0.01},"seed":1,"ecn":false,"gap":3}}]`

	kernelTBF = `[{"kind":"tbf","handle":"800d:","root":true,"refcnt":13,` +
		`"options":{"rate":25000,"burst":8192,"lat":1000000}}]`

	kernelEmpty = `[{"kind":"noqueue","handle":"0:","root":true,"refcnt":2,"options":{}}]`
)

func TestBuildPlanRendersEveryKind(t *testing.T) {
	cases := []struct {
		name     string
		params   Parameters
		want     []string
		wantKind string
	}{
		{
			name:     "latency",
			wantKind: "netem",
			params:   Parameters{Latency: Duration(50 * time.Millisecond), Queue: 1000},
			want:     []string{"tc", "qdisc", "replace", "dev", "nfc0", "root", "netem", "limit", "1000", "delay", "50ms"},
		},
		{
			name:     "jitter with correlation",
			wantKind: "netem",
			params:   Parameters{Latency: Duration(50 * time.Millisecond), Jitter: Duration(8 * time.Millisecond), Correlation: 25, Queue: 1000},
			want: []string{"tc", "qdisc", "replace", "dev", "nfc0", "root", "netem", "limit", "1000",
				"delay", "50ms", "8ms", "25%"},
		},
		{
			name:     "loss with correlation",
			wantKind: "netem",
			params:   Parameters{Loss: 10, Correlation: 30, Queue: 1000},
			want: []string{"tc", "qdisc", "replace", "dev", "nfc0", "root", "netem", "limit", "1000",
				"loss", "10%", "30%"},
		},
		{
			name:     "duplication",
			wantKind: "netem",
			params:   Parameters{Duplication: 20, Queue: 1000},
			want:     []string{"tc", "qdisc", "replace", "dev", "nfc0", "root", "netem", "limit", "1000", "duplicate", "20%"},
		},
		{
			name:     "reordering names the gap after the correlation",
			wantKind: "netem",
			params:   Parameters{Reordering: 25, Latency: Duration(20 * time.Millisecond), Correlation: 1, Gap: 3, Queue: 1000},

			want: []string{"tc", "qdisc", "replace", "dev", "nfc0", "root", "netem", "limit", "1000",
				"delay", "20ms", "reorder", "25%", "1%", "gap", "3"},
		},
		{
			name:     "reordering keeps the correlation the caller asked for, which may be none",
			wantKind: "netem",
			params:   Parameters{Reordering: 25, Latency: Duration(20 * time.Millisecond), Gap: 3, Queue: 1000},

			want: []string{"tc", "qdisc", "replace", "dev", "nfc0", "root", "netem", "limit", "1000",
				"delay", "20ms", "reorder", "25%", "0%", "gap", "3"},
		},
		{
			name:     "bandwidth",
			wantKind: "tbf",
			params:   Parameters{Rate: 200_000},
			want: []string{"tc", "qdisc", "replace", "dev", "nfc0", "root", "tbf",
				"rate", "200000bit", "burst", "3000b", "latency", "1000ms"},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			built, err := buildPlan("nfc0", test.params)
			if err != nil {
				t.Fatalf("buildPlan: %v", err)
			}
			if got := strings.Join(built.root, " "); got != strings.Join(test.want, " ") {
				t.Errorf("root\n got %q\nwant %q", got, strings.Join(test.want, " "))
			}
			wantClear := "tc qdisc del dev nfc0 root"
			if got := strings.Join(built.clear, " "); got != wantClear {
				t.Errorf("clear\n got %q\nwant %q", got, wantClear)
			}
			if test.wantKind != "" && built.kind != test.wantKind {
				t.Errorf("got kind %q, want %q", built.kind, test.wantKind)
			}
		})
	}
}

func TestBuildPlanRejectsBadDevices(t *testing.T) {
	for _, device := range []string{"", "eth0/1", strings.Repeat("e", 16), ".."} {
		if _, err := buildPlan(device, Parameters{Latency: Duration(time.Millisecond)}); err == nil {
			t.Errorf("the device %q was accepted", device)
		}
	}
	if _, err := buildPlan("nfc0", Parameters{}); err == nil {
		t.Error("a fault without parameters was accepted")
	}
}

func TestParseStateRejectsUnreadableOutput(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"blank":       "   \n",
		"not json":    "Cannot find device \"nfc0\"",
		"json null":   "null",
		"empty array": "[]",
		"object":      `{"kind":"netem"}`,
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseState(output); err == nil {
				t.Error("unreadable state was accepted")
			} else if !apperr.IsKind(err, apperr.KindIO) {
				t.Errorf("got %v, want an input error", err)
			}
		})
	}
}

func TestInstalledRecognisesTheKernelDefault(t *testing.T) {
	states, err := parseState(kernelEmpty)
	if err != nil {
		t.Fatalf("parseState: %v", err)
	}
	if installed(states) {
		t.Error("the empty kernel default must not count as an installed fault")
	}
	for _, output := range []string{kernelNetemDelay, kernelTBF} {
		states, err := parseState(output)
		if err != nil {
			t.Fatalf("parseState: %v", err)
		}
		if !installed(states) {
			t.Errorf("a fault discipline in %s was not recognised", output)
		}
	}
}

func TestObserveReadsTheKernelState(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		params  Parameters
		want    Observed
		wantErr bool
	}{
		{
			name:   "latency",
			output: kernelNetemDelay,
			params: Parameters{Latency: Duration(50 * time.Millisecond), Queue: 1000},
			want: Observed{
				Kind: "netem", Handle: "800c:", Queue: 1000,
				Latency: Duration(50 * time.Millisecond),
			},
		},
		{
			name:   "jitter",
			output: kernelNetemJitter,
			params: Parameters{Latency: Duration(50 * time.Millisecond), Jitter: Duration(8 * time.Millisecond), Correlation: 25, Queue: 1000},
			want: Observed{
				Kind: "netem", Handle: "1:", Queue: 1000,
				Latency: Duration(50 * time.Millisecond), Jitter: Duration(8 * time.Millisecond),
				Correlation: 25,
			},
		},
		{
			name:   "loss",
			output: kernelNetemLoss,
			params: Parameters{Loss: 10, Correlation: 30, Queue: 1000},
			want: Observed{
				Kind: "netem", Handle: "8004:", Queue: 1000, Loss: 10, Correlation: 30,
			},
		},
		{
			name:   "duplication",
			output: kernelNetemDuplicate,
			params: Parameters{Duplication: 20, Queue: 1000},
			want: Observed{
				Kind: "netem", Handle: "1:", Queue: 1000, Duplication: 20,
			},
		},
		{
			name:   "reordering",
			output: kernelNetemReorder,
			params: Parameters{Reordering: 25, Latency: Duration(20 * time.Millisecond), Correlation: 1, Gap: 3, Queue: 1000},
			want: Observed{
				Kind: "netem", Handle: "8004:", Queue: 1000, Latency: Duration(20 * time.Millisecond),
				Reordering: 25, Correlation: 1, Gap: 3,
			},
		},
		{
			name:   "a token bucket",
			output: kernelTBF,
			params: Parameters{Rate: 200_000},
			want: Observed{
				Kind: "tbf", Handle: "800d:", Rate: 200_000, Burst: 8192,
			},
		},
		{
			name:    "a bandwidth fault that the kernel did not install",
			output:  kernelNetemDelay,
			params:  Parameters{Rate: 200_000},
			wantErr: true,
		},
		{
			name:    "a fault on a device with no discipline",
			output:  kernelEmpty,
			params:  Parameters{Latency: Duration(30 * time.Millisecond), Queue: 1000},
			wantErr: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			states, err := parseState(test.output)
			if err != nil {
				t.Fatalf("parseState: %v", err)
			}
			seen, err := observe(states, "nfc0", test.params)
			if test.wantErr {
				if err == nil {
					t.Fatalf("observed %+v, want an error", seen)
				}
				return
			}
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			if seen != test.want {
				t.Errorf("observed\n got %+v\nwant %+v", seen, test.want)
			}
			if err := verify(seen, test.params); err != nil {
				t.Errorf("verify: %v", err)
			}
		})
	}
}

func TestVerifyCatchesAKernelThatDisagrees(t *testing.T) {
	cases := map[string]struct {
		seen   Observed
		params Parameters
	}{
		"a delay the kernel rounded away": {
			seen:   Observed{Kind: "netem", Latency: Duration(10 * time.Millisecond)},
			params: Parameters{Latency: Duration(50 * time.Millisecond)},
		},
		"a loss the kernel halved": {
			seen:   Observed{Kind: "netem", Loss: 5},
			params: Parameters{Loss: 10},
		},
		"a rate the kernel doubled": {
			seen:   Observed{Kind: "tbf", Rate: 400_000},
			params: Parameters{Rate: 200_000},
		},
		"a queue the kernel shrank": {
			seen:   Observed{Kind: "netem", Queue: 100},
			params: Parameters{Latency: Duration(time.Millisecond), Queue: 500},
		},
		"a reordering the kernel dropped": {
			seen:   Observed{Kind: "netem", Reordering: 0},
			params: Parameters{Reordering: 25, Latency: Duration(time.Millisecond)},
		},
		"a duplication the kernel dropped": {
			seen:   Observed{Kind: "netem", Duplication: 0},
			params: Parameters{Duplication: 20},
		},
		"a jitter the kernel dropped": {
			seen:   Observed{Kind: "netem", Jitter: 0},
			params: Parameters{Latency: Duration(50 * time.Millisecond), Jitter: Duration(5 * time.Millisecond)},
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			if err := verify(test.seen, test.params); err == nil {
				t.Error("a kernel that disagrees with the request was accepted")
			} else if !apperr.IsKind(err, apperr.KindValidation) {
				t.Errorf("got %v, want a validation error", err)
			}
		})
	}
}

func TestObserveIgnoresTheKernelSeed(t *testing.T) {

	states, err := parseState(kernelNetemDelay)
	if err != nil {
		t.Fatalf("parseState: %v", err)
	}
	if _, err := observe(states, "nfc0", Parameters{Latency: Duration(50 * time.Millisecond), Queue: 1000}); err != nil {
		t.Fatalf("observe: %v", err)
	}
}

func TestShowAndDelArgs(t *testing.T) {
	if got, want := strings.Join(showArgs("nfc0"), " "), "tc -j qdisc show dev nfc0"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := strings.Join(delArgs("nfc0"), " "), "tc qdisc del dev nfc0 root"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
