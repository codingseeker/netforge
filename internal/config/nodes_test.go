package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/netforge/netforge/internal/apperr"
)

func TestRequireScalarAcceptsString(t *testing.T) {
	node := scalarNode("10m")

	got, err := requireScalar("config", node, "limits.max_duration", `"10m"`)
	if err != nil {
		t.Fatalf("requireScalar() = %v, want nil", err)
	}
	if got != "10m" {
		t.Fatalf("requireScalar() = %q, want %q", got, "10m")
	}
}

func TestRequireScalarRejectsNonStringNodes(t *testing.T) {
	tests := []struct {
		name    string
		node    *yaml.Node
		wantMsg string
	}{
		{name: "missing", node: nil, wantMsg: "is missing"},
		{name: "list", node: &yaml.Node{Kind: yaml.SequenceNode}, wantMsg: "got a list"},
		{name: "mapping", node: &yaml.Node{Kind: yaml.MappingNode}, wantMsg: "got a mapping"},
		{name: "document", node: &yaml.Node{Kind: yaml.DocumentNode}, wantMsg: "got a document"},
		{name: "null", node: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}, wantMsg: "got null"},
		{name: "integer", node: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "600000000000"}, wantMsg: "got a non-string scalar"},
		{name: "boolean", node: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}, wantMsg: "got a non-string scalar"},
		{name: "unresolvable alias", node: &yaml.Node{Kind: yaml.AliasNode}, wantMsg: "got an alias"},
		{name: "empty", node: scalarNode(""), wantMsg: "must not be empty"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value, err := requireScalar("config", tc.node, "limits.max_duration", `"10m"`)
			if err == nil {
				t.Fatalf("requireScalar() = %q, want error", value)
			}
			if !apperr.IsKind(err, apperr.KindInvalidConfig) {
				t.Fatalf("requireScalar() kind = %v, want invalid_config", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("requireScalar() = %q, want it to contain %q", err, tc.wantMsg)
			}
		})
	}
}

func TestRequireScalarResolvesAlias(t *testing.T) {
	target := scalarNode("1Gbps")
	node := &yaml.Node{Kind: yaml.AliasNode, Alias: target}

	got, err := requireScalar("config", node, "limits.max_bandwidth", `"1Gbps"`)
	if err != nil {
		t.Fatalf("requireScalar(alias) = %v, want nil", err)
	}
	if got != "1Gbps" {
		t.Fatalf("requireScalar(alias) = %q, want %q", got, "1Gbps")
	}
}

func TestRequireScalarRejectsAliasToList(t *testing.T) {
	node := &yaml.Node{Kind: yaml.AliasNode, Alias: &yaml.Node{Kind: yaml.SequenceNode}}

	if _, err := requireScalar("config", node, "limits.max_bandwidth", `"1Gbps"`); err == nil {
		t.Fatal("requireScalar(alias to list) = nil error, want error")
	}
}

func TestNodeKind(t *testing.T) {
	tests := []struct {
		name string
		node *yaml.Node
		want string
	}{
		{name: "nil", node: nil, want: "nothing"},
		{name: "document", node: &yaml.Node{Kind: yaml.DocumentNode}, want: "a document"},
		{name: "sequence", node: &yaml.Node{Kind: yaml.SequenceNode}, want: "a list"},
		{name: "mapping", node: &yaml.Node{Kind: yaml.MappingNode}, want: "a mapping"},
		{name: "string", node: scalarNode("10m"), want: "a non-string scalar"},
		{name: "null", node: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}, want: "null"},
		{name: "alias", node: &yaml.Node{Kind: yaml.AliasNode}, want: "an alias"},
		{name: "unsupported", node: &yaml.Node{Kind: yaml.Kind(0)}, want: "an unsupported node"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeKind(tc.node); got != tc.want {
				t.Fatalf("nodeKind() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDurationRejectsNonScalarNode(t *testing.T) {
	if _, err := Parse([]byte("name: x\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\nlimits:\n  max_duration: [10m]\n")); err == nil {
		t.Fatal("Parse(max_duration list) = nil error, want error")
	}
}

func TestBandwidthRejectsNonScalarNode(t *testing.T) {
	if _, err := Parse([]byte("name: x\nscope:\n  network: 10.42.0.0/16\n  namespaces:\n    - client\nlimits:\n  max_bandwidth: {value: 1Gbps}\n")); err == nil {
		t.Fatal("Parse(max_bandwidth mapping) = nil error, want error")
	}
}

func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
