package apitypes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestNullableFieldsAreTyped keeps the generated TypeScript honest: a pointer
// field without omitempty is serialised as null, so it must carry a
// `tstype:"T | null,required"` tag; one with omitempty is an optional
// request field. Every field needs an explicit json name.
func TestNullableFieldsAreTyped(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, f := range st.Fields.List {
				name := ts.Name.Name + "." + f.Names[0].Name
				if f.Tag == nil {
					t.Errorf("%s: missing struct tag", name)
					continue
				}
				raw, _ := strconv.Unquote(f.Tag.Value)
				tag := reflect.StructTag(raw)
				js := tag.Get("json")
				if js == "" || strings.HasPrefix(js, ",") {
					t.Errorf("%s: missing json name", name)
				}
				if _, isPtr := f.Type.(*ast.StarExpr); isPtr {
					omit := strings.Contains(js, ",omitempty")
					if !omit && !strings.Contains(tag.Get("tstype"), "| null,required") {
						t.Errorf("%s: nullable pointer needs tstype:\"T | null,required\"", name)
					}
				}
			}
			return true
		})
	}
}
