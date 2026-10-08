package config

import (
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/codingseeker/netforge/internal/apperr"
)

func requireScalar(op string, node *yaml.Node, field, example string) (string, error) {
	if node == nil {
		return "", apperr.InvalidConfig(op, field+" is missing", nil)
	}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", apperr.InvalidConfig(op,
			field+" must be a quoted string such as "+example+", got "+nodeKind(node), nil)
	}
	if node.Value == "" {
		return "", apperr.InvalidConfig(op, field+" must not be empty", nil)
	}
	return node.Value, nil
}

func nodeKind(node *yaml.Node) string {
	if node == nil {
		return "nothing"
	}
	switch node.Kind {
	case yaml.DocumentNode:
		return "a document"
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return "null"
		}
		return "a non-string scalar"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "an unsupported node"
	}
}

func quote(value string) string {
	return strconv.Quote(value)
}
