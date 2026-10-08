package security

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var packetImportAllowlist = map[string]bool{
	"bytes":           true,
	"encoding/binary": true,
	"encoding/hex":    true,
	"math":            true,
	"net":             true,
	"net/netip":       true,
	"strconv":         true,
	"strings":         true,

	"github.com/codingseeker/netforge/internal/apperr": true,
}

func TestPacketLayerImportsOnlyTheAllowlist(t *testing.T) {
	for _, file := range packetProductSources(t) {
		relative := relative(t, file)
		for _, spec := range parse(t, file).Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Errorf("%s has an unparseable import %s", relative, spec.Path.Value)
				continue
			}
			if !packetImportAllowlist[path] {
				t.Errorf("%s imports %q, which is not on the packet layer allowlist", relative, path)
			}
		}
	}
}

func TestPacketLayerIsDeterministic(t *testing.T) {

	forbidden := []string{
		"rand.",
		"time.",
		"os.Getenv",
		"goroutine",
		"select {",
	}
	for _, file := range packetProductSources(t) {
		relative := relative(t, file)
		content := read(t, file)
		for _, pattern := range forbidden {
			if strings.Contains(content, pattern) {
				t.Errorf("%s contains %q; parsing must depend only on its input", relative, pattern)
			}
		}
	}
}

func TestPacketLayerDoesNotDependOnTheProcessBoundary(t *testing.T) {

	forbidden := []string{
		"panic(",
		"os.Exit(",
		"log.Fatal",
		"os.Open(",
		"os.ReadFile(",
		"net.Dial(",
		"net.Listen(",
		"http.",
	}
	for _, file := range packetProductSources(t) {
		relative := relative(t, file)
		content := read(t, file)
		for _, pattern := range forbidden {
			if strings.Contains(content, pattern) {
				t.Errorf("%s contains %q; a parser must not depend on the process", relative, pattern)
			}
		}
	}
}

func TestEveryAllocationIsBoundedBeforeItHappens(t *testing.T) {

	for _, file := range packetProductSources(t) {
		relative := relative(t, file)
		parsed := parse(t, file)
		for _, decl := range parsed.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			comparisons := boundComparisons(function.Body)
			for _, allocation := range allocations(function.Body) {
				size, ok := allocationSize(t, allocation.call)
				if !ok {
					continue
				}
				terms := sizeTerms(t, size)
				if len(terms) == 0 {
					continue
				}
				bounded := false
				for _, comparison := range comparisons {
					if comparison.op >= allocation.pos {
						continue
					}
					if intersects(terms, comparison.left) && mentionsBound(comparison.right) {
						bounded = true
						break
					}
					if intersects(terms, comparison.right) && mentionsBound(comparison.left) {
						bounded = true
						break
					}
				}
				if !bounded {
					t.Errorf("%s: %s allocates %s at offset %d with no earlier comparison "+
						"of that value against a limit", relative, function.Name.Name, renderExpr(size), allocation.pos)
				}
			}
		}
	}
}

type allocation struct {
	call *ast.CallExpr
	pos  token.Pos
}

type comparison struct {
	left, right ast.Expr
	op          token.Pos
}

func allocations(body *ast.BlockStmt) []allocation {
	var found []allocation
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "make" {
			found = append(found, allocation{call: call, pos: call.Pos()})
		}
		return true
	})
	return found
}

func allocationSize(t *testing.T, call *ast.CallExpr) (ast.Expr, bool) {
	t.Helper()
	if len(call.Args) == 0 {
		return nil, false
	}
	size := call.Args[len(call.Args)-1]
	if _, literal := size.(*ast.BasicLit); literal {
		return nil, false
	}
	return size, true
}

func sizeTerms(t *testing.T, size ast.Expr) map[string]bool {
	t.Helper()
	terms := map[string]bool{renderExpr(size): true}
	ast.Inspect(size, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if fun, ok := call.Fun.(*ast.Ident); ok && fun.Name == "len" {
			terms[renderExpr(call)] = true
		}
		return true
	})
	return terms
}

func boundComparisons(body *ast.BlockStmt) []comparison {
	var found []comparison
	ast.Inspect(body, func(node ast.Node) bool {
		expression, ok := node.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		switch expression.Op {
		case token.LSS, token.GTR, token.LEQ, token.GEQ, token.NEQ, token.EQL:
		default:
			return true
		}
		if !mentionsBound(expression.X) && !mentionsBound(expression.Y) {
			return true
		}
		found = append(found, comparison{left: expression.X, right: expression.Y, op: expression.OpPos})
		return true
	})
	return found
}

func intersects(terms map[string]bool, expression ast.Expr) bool {
	return terms[renderExpr(expression)]
}

func mentionsBound(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.SelectorExpr:
			if strings.HasPrefix(typed.Sel.Name, "Max") || strings.HasPrefix(typed.Sel.Name, "Min") {
				found = true
			}
			if ident, ok := typed.X.(*ast.Ident); ok && ident.Name == "limits" {
				found = true
			}
		case *ast.Ident:
			if strings.HasPrefix(typed.Name, "Max") || strings.HasPrefix(typed.Name, "Min") {
				found = true
			}
			if typed.Name == "limits" {
				found = true
			}
		}
		return true
	})
	return found
}

func TestParserChecksInputLength(t *testing.T) {

	for _, file := range packetProductSources(t) {
		relative := relative(t, file)
		parsed := parse(t, file)
		for _, decl := range parsed.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil || !strings.HasPrefix(function.Name.Name, "Parse") {
				continue
			}
			parameter, ok := firstByteSliceParameter(function)
			if !ok {
				t.Errorf("%s: %s does not take a byte slice to parse", relative, function.Name.Name)
				continue
			}
			minimum, guard, accessed := lengthChecks(function.Body, parameter)

			if delegatesToParser(function.Body, parameter) {
				continue
			}
			switch {
			case !minimum:
				t.Errorf("%s: %s never rejects a %s that is shorter than the header it parses",
					relative, function.Name.Name, parameter)
			case guard == token.NoPos:
				t.Errorf("%s: %s never compares the length of %s", relative, function.Name.Name, parameter)
			case accessed != token.NoPos && accessed < guard:
				t.Errorf("%s: %s reads %s at offset %d before the length check at offset %d",
					relative, function.Name.Name, parameter, accessed, guard)
			}
		}
	}
}

func TestProtocolConstantsMatchTheStandards(t *testing.T) {

	for _, constant := range []struct {
		name  string
		value string
	}{
		{"EthernetHeaderSize", "= 14"},
		{"EthernetAddressSize", "= 6"},
		{"MinIPv4HeaderSize", "= 20"},
		{"MaxIPv4HeaderSize", "= 60"},
		{"MaxIPv4TotalLength", "= 65535"},
		{"MinICMPHeaderSize", "= 4"},
		{"ICMPErrorHeaderSize", "= 8"},
		{"ICMPEchoHeaderSize", "= 8"},
		{"UDPHeaderSize", "= 8"},
		{"MinTCPHeaderSize", "= 20"},
		{"MaxTCPHeaderSize", "= 60"},
		{"EtherTypeIPv4", "EtherType = 0x0800"},
		{"ProtocolICMP", "Protocol = 1"},
		{"ProtocolTCP", "Protocol = 6"},
		{"ProtocolUDP", "Protocol = 17"},
	} {
		declaration := constantValue(t, "internal/packet/limits.go", constant.name)
		if declaration == "" {
			t.Errorf("internal/packet/limits.go no longer declares %s", constant.name)
			continue
		}
		if !strings.Contains(declaration, constant.value) {
			t.Errorf("%s is declared as %q, want it to contain %q",
				constant.name, declaration, constant.value)
		}
	}
}

func TestChecksumFieldOffsetsOnesRFCsDefine(t *testing.T) {

	if declaration := constantValue(t, "internal/packet/udp.go", "UDPChecksumOffset"); !strings.Contains(declaration, "6") {
		t.Errorf("UDPChecksumOffset is declared as %q, want the RFC 768 offset 6", declaration)
	}
	if declaration := constantValue(t, "internal/packet/tcp.go", "TCPChecksumOffset"); !strings.Contains(declaration, "16") {
		t.Errorf("TCPChecksumOffset is declared as %q, want the RFC 793 offset 16", declaration)
	}
}

func firstByteSliceParameter(function *ast.FuncDecl) (string, bool) {
	if function.Type.Params == nil {
		return "", false
	}
	for _, field := range function.Type.Params.List {
		array, ok := field.Type.(*ast.ArrayType)
		if !ok {
			continue
		}
		ident, ok := array.Elt.(*ast.Ident)
		if !ok || ident.Name != "byte" {
			continue
		}
		if len(field.Names) == 0 {
			return "", false
		}
		return field.Names[0].Name, true
	}
	return "", false
}

func lengthChecks(body *ast.BlockStmt, name string) (minimum bool, guard, access token.Pos) {
	guard, access = token.NoPos, token.NoPos
	ast.Inspect(body, func(node ast.Node) bool {
		if comparison, ok := node.(*ast.BinaryExpr); ok {
			if call, ok := comparison.X.(*ast.CallExpr); ok {
				if fun, ok := call.Fun.(*ast.Ident); ok && fun.Name == "len" {
					if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == name {
						if guard == token.NoPos || comparison.OpPos < guard {
							guard = comparison.OpPos
						}
						switch comparison.Op {
						case token.LSS, token.NEQ:
							minimum = true
						}
					}
				}
			}
		}
		if index, ok := node.(*ast.IndexExpr); ok {
			if ident, ok := index.X.(*ast.Ident); ok && ident.Name == name {
				if access == token.NoPos || index.Lbrack < access {
					access = index.Lbrack
				}
			}
		}
		if slice, ok := node.(*ast.SliceExpr); ok {
			if ident, ok := slice.X.(*ast.Ident); ok && ident.Name == name {
				if access == token.NoPos || slice.Lbrack < access {
					access = slice.Lbrack
				}
			}
		}
		return true
	})
	return minimum, guard, access
}

func delegatesToParser(body *ast.BlockStmt, name string) bool {
	delegates := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun, ok := call.Fun.(*ast.Ident)
		if !ok || !strings.HasPrefix(fun.Name, "Parse") {
			return true
		}
		for _, argument := range call.Args {
			if ident, ok := argument.(*ast.Ident); ok && ident.Name == name {
				delegates = true
			}
		}
		return true
	})
	return delegates
}

func constantValue(t *testing.T, relativePath, name string) string {
	t.Helper()
	declared := parse(t, repoFile(t, relativePath))
	for _, decl := range declared.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range values.Names {
				if ident.Name != name || i >= len(values.Values) {
					continue
				}
				var builder strings.Builder
				builder.WriteString(ident.Name)
				if values.Type != nil {
					builder.WriteString(" ")
					builder.WriteString(render(t, values.Type))
				}
				builder.WriteString(" = ")
				builder.WriteString(render(t, values.Values[i]))
				return builder.String()
			}
		}
	}
	return ""
}

func render(t *testing.T, expression ast.Expr) string {
	t.Helper()
	return renderExpr(expression)
}

func renderExpr(expression ast.Expr) string {
	var builder strings.Builder
	if err := printer.Fprint(&builder, token.NewFileSet(), expression); err != nil {
		return ""
	}
	return builder.String()
}

func packetProductSources(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, file := range packetSources(t) {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		files = append(files, file)
	}
	return files
}

func packetSources(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, file := range collect(t, filepath.Join(repoRoot(t), "internal", "packet"), ".go") {
		files = append(files, file)
	}
	return files
}

func parse(t *testing.T, path string) *ast.File {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("ParseFile(%q) = %v, want nil", path, err)
	}
	return parsed
}
