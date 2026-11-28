package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The heat control and the API must hold devices through one
// drivercontrol.Holds. If main handed the API none, the API would build its
// own, and an operator's hold and a planned value would sit on two locks:
// each would think the driver free and could expire into a default that
// undoes the other. main.go is read rather than run because building the
// whole process here would test everything but this.
func TestMainHandsTheHeatControlAndTheAPIOneSetOfHolds(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var made []string // identifiers assigned from drivercontrol.New
	heatHolds, apiHolds := "", ""
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range n.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok || !isSelector(call.Fun, "drivercontrol", "New") || i >= len(n.Lhs) {
					continue
				}
				if id, ok := n.Lhs[i].(*ast.Ident); ok {
					made = append(made, id.Name)
				}
			}
		case *ast.CompositeLit:
			switch {
			case isSelector(n.Type, "heatrun", "HoldsWriter"):
				heatHolds = fieldIdent(n, "Holds")
			case isSelector(n.Type, "api", "Deps"):
				apiHolds = fieldIdent(n, "ControlHolds")
			}
		}
		return true
	})

	if len(made) != 1 {
		t.Fatalf("main.go makes %d sets of control holds (%v), want 1", len(made), made)
	}
	if heatHolds != made[0] {
		t.Errorf("heatrun.HoldsWriter{Holds: %q}, want %q", heatHolds, made[0])
	}
	if apiHolds != made[0] {
		t.Errorf("api.Deps{ControlHolds: %q}, want %q; without it the API builds its own", apiHolds, made[0])
	}
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// fieldIdent is the identifier a keyed composite literal sets field to, or "".
func fieldIdent(lit *ast.CompositeLit, field string) string {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
			if v, ok := kv.Value.(*ast.Ident); ok {
				return v.Name
			}
		}
	}
	return ""
}
