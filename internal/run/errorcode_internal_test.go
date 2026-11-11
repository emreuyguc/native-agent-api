package run

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestEveryErrorCodeHasAMeaning: the API maps a run error code through ErrorCodes, so a constant
// that is missing there would reach clients as an empty code.
func TestEveryErrorCodeHasAMeaning(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if ident, ok := spec.Type.(*ast.Ident); !ok || ident.Name != "ErrorCode" {
			return true
		}
		for _, value := range spec.Values {
			literal, ok := value.(*ast.BasicLit)
			if !ok {
				t.Errorf("ErrorCode constant is not a string literal")
				continue
			}
			code, _ := strconv.Unquote(literal.Value)
			found++
			if ErrorCode(code).Meaning() == "" {
				t.Errorf("ErrorCode %q has no meaning", code)
			}
		}

		return true
	})
	if found != len(ErrorCodes()) {
		t.Errorf("%d ErrorCode constants, %d meanings", found, len(ErrorCodes()))
	}
}
