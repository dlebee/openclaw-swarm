package apply_test

// This file is a build-time guard (a "grep-guard" implemented as a Go test)
// that enforces the cache-purity rules documented in
// openclaw-swarm/.cursor/rules/caches-are-optimizations.mdc and
// openclaw-swarm/AGENTS.md.
//
// Concretely: no method named Check or Applicable (the scaffold.Step
// predicates) anywhere under internal/claws/plans/apply/ may
//
//   1. Call any scaffold.PlanCache*Set / RecordPlanMachine* helper
//      (direct plan-cache writes), or
//   2. Call common.RegisterHostResolver / RecordPlan* equivalents, or
//   3. Assign to a field of the Target.Payload's concrete type (typical
//      payload mutation: `mt.Instance = ...`, `at.Foo = ...`).
//
// If this test fails you are almost certainly reinventing the exact bug the
// cache-purity audit (cache-purity-audit_94646afe) was written to eliminate
// — the probe UI will lie about what will execute, parallel probing will
// stop being safe, and the next engineer will re-run this whole audit.
//
// Fix shape: move the side effect into Execute (which is allowed to mutate),
// or introduce / use a Resolve* helper (see provisioning.ResolveMachineStatus,
// mesh.ResolveMeshIP, mesh.getOrResolveControlURL) that Check and Execute
// both consult.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenCallSuffixes are function names (just the Sel part of a
// selector expression like `scaffold.PlanCacheSet`) that must not appear
// inside Check/Applicable bodies. We match on the last identifier so
// aliases like `sc.PlanCacheSet` still trip.
var forbiddenCallSuffixes = []string{
	"PlanCacheSet",
	"RecordPlanMachineHost",
	"RecordPlanMachineExists",
	"RecordPlanMachineMeshIP",
	"RecordPlanMachineControlURL",
	"RecordPlanMachinePreauthKey",
	"RecordMachineStatus",
	"RegisterHostResolver",
}

// payloadReceiverPrefixes are local variable names conventionally used for
// an unwrapped target payload in apply steps. An assignment whose LHS is
// `<prefix>.<Field>` inside Check/Applicable is treated as payload
// mutation. Keep this list in sync with the receiver names used in
// apply/** (mt = MachineTarget, at = AgentTarget, gt = GatewayTarget, nt
// = NodeTarget, ct = ChannelTarget, mesht = MeshTarget ... etc.).
var payloadReceiverPrefixes = []string{
	"mt", "at", "gt", "nt", "ct",
}

func TestCachePurity_CheckApplicableHaveNoMutations(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "internal", "claws", "plans", "apply")

	type violation struct {
		file   string
		method string
		reason string
		line   int
	}
	var violations []violation

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			if fn.Name.Name != "Check" && fn.Name.Name != "Applicable" {
				continue
			}
			// Walk the body for disallowed patterns.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.CallExpr:
					sel, ok := v.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					for _, bad := range forbiddenCallSuffixes {
						if sel.Sel.Name == bad {
							violations = append(violations, violation{
								file:   path,
								method: fn.Name.Name,
								reason: "calls " + bad + " (cache write)",
								line:   fset.Position(v.Pos()).Line,
							})
						}
					}
				case *ast.AssignStmt:
					// Only flag plain `x.F = ...` (single LHS, selector,
					// receiver name in the known-payload set).
					if len(v.Lhs) != 1 {
						return true
					}
					sel, ok := v.Lhs[0].(*ast.SelectorExpr)
					if !ok {
						return true
					}
					xIdent, ok := sel.X.(*ast.Ident)
					if !ok {
						return true
					}
					for _, pre := range payloadReceiverPrefixes {
						if xIdent.Name == pre {
							violations = append(violations, violation{
								file:   path,
								method: fn.Name.Name,
								reason: "assigns " + xIdent.Name + "." + sel.Sel.Name + " (payload mutation)",
								line:   fset.Position(v.Pos()).Line,
							})
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(violations) > 0 {
		var b strings.Builder
		b.WriteString("cache-purity violation(s) — see .cursor/rules/caches-are-optimizations.mdc:\n")
		for _, v := range violations {
			b.WriteString("  ")
			b.WriteString(v.file)
			b.WriteString(":")
			b.WriteString(itoa(v.line))
			b.WriteString(" ")
			b.WriteString(v.method)
			b.WriteString(": ")
			b.WriteString(v.reason)
			b.WriteString("\n")
		}
		t.Fatal(b.String())
	}
}

// TestCachePurity_DetectorFlagsViolations is a self-test for the guard:
// it feeds an in-memory fixture with known-bad Check/Applicable methods
// and asserts every bad pattern is detected. Without this, a typo in
// forbiddenCallSuffixes or payloadReceiverPrefixes would silently make
// TestCachePurity_CheckApplicableHaveNoMutations a no-op green.
func TestCachePurity_DetectorFlagsViolations(t *testing.T) {
	src := `package fixture

type payload struct{ Instance int }
type scaffold struct{}

func (s *scaffold) PlanCacheSet(_, _ int)            {}
func (s *scaffold) RecordPlanMachineHost(_, _ int)   {}
func (s *scaffold) RecordPlanMachineMeshIP(_, _ int) {}

type Step struct{ sc *scaffold }

func (s *Step) Check() bool {
	s.sc.PlanCacheSet(1, 2)
	mt := &payload{}
	mt.Instance = 7
	return true
}

func (s *Step) Applicable() bool {
	s.sc.RecordPlanMachineHost(1, 2)
	return false
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	var hits []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		if fn.Name.Name != "Check" && fn.Name.Name != "Applicable" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
					for _, bad := range forbiddenCallSuffixes {
						if sel.Sel.Name == bad {
							hits = append(hits, fn.Name.Name+":"+bad)
						}
					}
				}
			case *ast.AssignStmt:
				if len(v.Lhs) != 1 {
					return true
				}
				sel, ok := v.Lhs[0].(*ast.SelectorExpr)
				if !ok {
					return true
				}
				xIdent, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				for _, pre := range payloadReceiverPrefixes {
					if xIdent.Name == pre {
						hits = append(hits, fn.Name.Name+":"+xIdent.Name+"."+sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
	want := map[string]bool{
		"Check:PlanCacheSet":              true,
		"Check:mt.Instance":               true,
		"Applicable:RecordPlanMachineHost": true,
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("detector missed %q; got %v", k, hits)
		}
	}
}

// itoa avoids pulling strconv just for the error formatter.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
