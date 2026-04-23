// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/dice"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// addrCmp compares catalog.Addr values by identity for cmp.Diff.
var addrCmp = cmp.Comparer(func(a, b catalog.Addr) bool {
	return a.Identity() == b.Identity()
})

// moduleCmp compares catalog.ModulePath values by identity for cmp.Diff.
var moduleCmp = cmp.Comparer(func(a, b catalog.ModulePath) bool {
	return a.Identity() == b.Identity()
})

func newTestSession(t *testing.T) *dice.Session {
	t.Helper()
	s, _ := dice.NewSession(t.Context())
	return s
}

func newSolver(t *testing.T, root *catalog.Package, eval Evaluator, cfg Config) *Solver {
	t.Helper()
	return New(newTestSession(t), catalog.New(root), eval, cfg)
}

func TestSolver_TargetInstances(t *testing.T) {
	root := catalog.RootModule()

	tests := []struct {
		name      string
		setup     func() (*catalog.Package, Evaluator)
		addr      catalog.Addr
		wantAddrs []catalog.Addr
		wantErr   string
	}{
		{
			name: "expands count",
			setup: func() (*catalog.Package, Evaluator) {
				pkg := rootPkg(
					[]*catalog.TargetDecl{targetDecl(root, catalog.TargetKindResource, "test_resource", "build")},
					nil, nil,
				)
				eval := fakeEvaluator{
					targetInstancesFn: func(_ context.Context, req EvalTargetInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
						return InstanceResult{
							Keys:  []catalog.Key{catalog.IntKey(0), catalog.IntKey(1)},
							Shape: InstanceShapeList,
						}, nil
					},
				}
				return pkg, eval
			},
			addr: catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "build", catalog.NoKey()),
			wantAddrs: []catalog.Addr{
				catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "build", catalog.IntKey(0)),
				catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "build", catalog.IntKey(1)),
			},
		},
		{
			name: "rejects specific key",
			setup: func() (*catalog.Package, Evaluator) {
				return rootPkg(
					[]*catalog.TargetDecl{targetDecl(root, catalog.TargetKindResource, "test_resource", "build")},
					nil, nil,
				), fakeEvaluator{}
			},
			addr:    catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "build", catalog.StringKey("beta")),
			wantErr: "declaration address",
		},
		{
			name: "errors on deferred in inspection mode",
			setup: func() (*catalog.Package, Evaluator) {
				pkg := rootPkg(
					[]*catalog.TargetDecl{targetDecl(root, catalog.TargetKindResource, "test_resource", "build")},
					nil, nil,
				)
				eval := fakeEvaluator{
					targetInstancesFn: func(_ context.Context, req EvalTargetInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
						return InstanceResult{Deferred: true, Shape: InstanceShapeList}, nil
					},
				}
				return pkg, eval
			},
			addr:    catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "build", catalog.NoKey()),
			wantErr: "runtime values not available in inspection mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg, eval := tt.setup()
			solver := newSolver(t, pkg, eval, Config{})
			result, diags := solver.TargetInstances(t.Context(), tt.addr)

			if tt.wantErr != "" {
				if !diags.HasErrors() {
					t.Fatal("expected error")
				}
				if got := diags.Err().Error(); !strings.Contains(got, tt.wantErr) {
					t.Fatalf("error %q does not contain %q", got, tt.wantErr)
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %s", diags.Err())
			}
			if tt.wantAddrs != nil {
				if diff := cmp.Diff(tt.wantAddrs, result.Addrs, addrCmp); diff != "" {
					t.Fatalf("wrong addrs (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestSolver_PackageInstances(t *testing.T) {
	root := catalog.RootModule()

	tests := []struct {
		name        string
		setup       func() (*catalog.Package, Evaluator)
		module      catalog.ModulePath
		wantModules []catalog.ModulePath
		wantErr     string
	}{
		{
			name: "expands module count",
			setup: func() (*catalog.Package, Evaluator) {
				pkg := rootPkg(nil, nil, nil)
				childPkg(pkg, "child", nil, []*catalog.OutputDecl{outputDecl(root.Child("child", catalog.NoKey()), "image")}, nil)
				eval := fakeEvaluator{
					importInstancesFn: func(_ context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
						return InstanceResult{
							Keys:  []catalog.Key{catalog.IntKey(0), catalog.IntKey(1)},
							Shape: InstanceShapeList,
						}, nil
					},
				}
				return pkg, eval
			},
			module: root.Child("child", catalog.NoKey()),
			wantModules: []catalog.ModulePath{
				root.Child("child", catalog.IntKey(0)),
				root.Child("child", catalog.IntKey(1)),
			},
		},
		{
			name: "root module returns singleton",
			setup: func() (*catalog.Package, Evaluator) {
				return rootPkg(nil, nil, nil), fakeEvaluator{}
			},
			module:      catalog.RootModule(),
			wantModules: []catalog.ModulePath{catalog.RootModule()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg, eval := tt.setup()
			solver := newSolver(t, pkg, eval, Config{})
			result, diags := solver.PackageInstances(t.Context(), tt.module)

			if tt.wantErr != "" {
				if !diags.HasErrors() {
					t.Fatal("expected error")
				}
				if got := diags.Err().Error(); !strings.Contains(got, tt.wantErr) {
					t.Fatalf("error %q does not contain %q", got, tt.wantErr)
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %s", diags.Err())
			}
			if tt.wantModules != nil {
				if diff := cmp.Diff(tt.wantModules, result.Modules, moduleCmp); diff != "" {
					t.Fatalf("wrong modules (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestSolver_OutputValue(t *testing.T) {
	root := catalog.RootModule()

	tests := []struct {
		name      string
		setup     func() (*catalog.Package, Evaluator, Config)
		addr      catalog.Addr
		wantKnown bool
		wantDefer bool
		wantValue cty.Value
		wantRefs  []catalog.Addr
		wantErr   string
	}{
		{
			name: "returns static evaluated result",
			setup: func() (*catalog.Package, Evaluator, Config) {
				pkg := rootPkg(nil, []*catalog.OutputDecl{outputDecl(root, "tag")}, nil)
				eval := fakeEvaluator{
					outputFn: func(_ context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
						return EvalResult{
							Value:  cty.StringVal("busybox"),
							Digest: digest.FromStrings("output-tag"),
							Known:  true,
						}, nil
					},
				}
				return pkg, eval, Config{}
			},
			addr:      catalog.OutputAddr(root, "tag"),
			wantKnown: true,
			wantValue: cty.StringVal("busybox"),
		},
		{
			name: "defers on dynamic reference",
			setup: func() (*catalog.Package, Evaluator, Config) {
				baseAddr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "base", catalog.NoKey())
				pkg := rootPkg(
					[]*catalog.TargetDecl{targetDecl(root, catalog.TargetKindResource, "test_resource", "base")},
					[]*catalog.OutputDecl{outputDecl(root, "image")},
					nil,
				)
				eval := fakeEvaluator{
					outputFn: func(_ context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
						return EvalResult{
							Deferred: true,
							Refs:     []catalog.Addr{baseAddr},
						}, nil
					},
				}
				return pkg, eval, Config{}
			},
			addr:      catalog.OutputAddr(root, "image"),
			wantDefer: true,
			wantRefs:  []catalog.Addr{catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "base", catalog.NoKey())},
		},
		{
			name: "defers for deferred concrete module instance without hard error",
			setup: func() (*catalog.Package, Evaluator, Config) {
				pkg := rootPkg(
					[]*catalog.TargetDecl{targetDecl(root, catalog.TargetKindResource, "test_resource", "base")},
					[]*catalog.OutputDecl{outputDecl(root, "image")},
					nil,
				)
				child := root.Child("child", catalog.NoKey())
				childPkg(pkg, "child", nil, []*catalog.OutputDecl{outputDecl(child, "image")}, nil)

				eval := fakeEvaluator{
					importInstancesFn: func(_ context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
						if req.DeclAddr.Kind == catalog.TargetKindModule && req.Module.Identity() == root.Child("child", catalog.NoKey()).Identity() {
							return InstanceResult{Deferred: true, Shape: InstanceShapeList}, nil
						}
						return InstanceResult{Keys: []catalog.Key{catalog.NoKey()}, Shape: InstanceShapeSingle}, nil
					},
					outputFn: func(_ context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
						if req.Addr.Name == "image" && req.Addr.Module.Identity() == root.Identity() {
							return EvalResult{Deferred: true}, nil
						}
						return EvalResult{Value: cty.StringVal("hello"), Known: true, Digest: digest.FromStrings("child-out")}, nil
					},
				}
				return pkg, eval, Config{}
			},
			addr:      catalog.OutputAddr(root, "image"),
			wantDefer: true,
		},
		{
			name: "resolves child module output known through solver runtime resolver",
			setup: func() (*catalog.Package, Evaluator, Config) {
				child := root.Child("child", catalog.NoKey())
				pkg := rootPkg(nil, []*catalog.OutputDecl{outputDecl(root, "value")}, nil)
				childPkg(pkg, "child", nil, []*catalog.OutputDecl{outputDecl(child, "value")}, nil)

				eval := fakeEvaluator{
					importInstancesFn: func(_ context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
						if req.DeclAddr.Kind == catalog.TargetKindModule {
							return InstanceResult{
								Keys:  []catalog.Key{catalog.StringKey("amd64")},
								Shape: InstanceShapeMap,
							}, nil
						}
						return InstanceResult{Keys: []catalog.Key{catalog.NoKey()}, Shape: InstanceShapeSingle}, nil
					},
					outputFn: func(_ context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
						if req.Addr.Module.Identity() == root.Identity() {
							return EvalResult{
								Value:  cty.StringVal("hello"),
								Digest: digest.FromStrings("root-output"),
								Known:  true,
							}, nil
						}
						return EvalResult{
							Value:  cty.StringVal("hello"),
							Digest: digest.FromStrings("child-output"),
							Known:  true,
						}, nil
					},
				}
				return pkg, eval, Config{}
			},
			addr:      catalog.OutputAddr(root, "value"),
			wantKnown: true,
			wantValue: cty.StringVal("hello"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg, eval, cfg := tt.setup()
			solver := newSolver(t, pkg, eval, cfg)
			result, diags := solver.OutputValue(t.Context(), tt.addr)

			if tt.wantErr != "" {
				if !diags.HasErrors() {
					t.Fatal("expected error")
				}
				if got := diags.Err().Error(); !strings.Contains(got, tt.wantErr) {
					t.Fatalf("error %q does not contain %q", got, tt.wantErr)
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %s", diags.Err())
			}
			if got, want := result.Known, tt.wantKnown; got != want {
				t.Fatalf("Known = %v, want %v", got, want)
			}
			if got, want := result.Deferred, tt.wantDefer; got != want {
				t.Fatalf("Deferred = %v, want %v", got, want)
			}
			if tt.wantValue != cty.NilVal {
				if !result.Value.RawEquals(tt.wantValue) {
					t.Fatalf("wrong value: got %s want %s", result.Value.GoString(), tt.wantValue.GoString())
				}
			}
			if tt.wantRefs != nil {
				if diff := cmp.Diff(tt.wantRefs, result.Refs, addrCmp); diff != "" {
					t.Fatalf("wrong refs (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestSolver_ActionSpec(t *testing.T) {
	root := catalog.RootModule()

	tests := []struct {
		name  string
		setup func() (*catalog.Package, Evaluator, Config)
		addr  catalog.Addr
		check func(t *testing.T, spec engine.Spec, diags tfdiags.Diagnostics)
	}{
		{
			name: "lowers builtin run payload",
			setup: func() (*catalog.Package, Evaluator, Config) {
				addr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "run", catalog.NoKey())
				req, reqDiags := buildrun.BuildRequest(buildrun.RequestArgs{
					Argv:   []string{"/bin/echo", "hello"},
					Cwd:    t.TempDir(),
					Decode: buildrun.DecodeModeJSON,
				}, tfdiags.SourceRange{})
				if reqDiags.HasErrors() {
					t.Fatalf("unexpected request diagnostics: %s", reqDiags.Err())
				}

				pkg := rootPkg([]*catalog.TargetDecl{{
					Addr:        addr,
					Driver:      "test",
					ConfigKey:   digest.FromStrings("target"),
					ConfigValid: true,
				}}, nil, nil)

				eval := fakeEvaluator{
					targetFn: func(_ context.Context, r EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
						return EvalResult{
							Payload: &buildrun.Payload{
								Request:      *req,
								ValueAdapter: buildrun.ValueAdapterRawV1,
							},
							Digest: req.Key,
							Known:  true,
						}, nil
					},
				}
				return pkg, eval, Config{}
			},
			addr: catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "run", catalog.NoKey()),
			check: func(t *testing.T, spec engine.Spec, diags tfdiags.Diagnostics) {
				if diags.HasErrors() {
					t.Fatalf("unexpected diagnostics: %s", diags.Err())
				}
				if got, want := spec.Runner.Kind, string(catalog.RunnerKindBuiltinRun); got != want {
					t.Fatalf("wrong runner kind: got %q want %q", got, want)
				}
				if spec.Cacheable {
					t.Fatal("did not expect builtin.run to be cacheable")
				}
				if !spec.Volatile {
					t.Fatal("expected builtin.run to be volatile")
				}
				payload, ok := spec.Runner.Payload.(buildrun.Payload)
				if !ok {
					t.Fatalf("wrong payload type: got %T", spec.Runner.Payload)
				}
				if got, want := payload.ValueAdapter, buildrun.ValueAdapterRawV1; got != want {
					t.Fatalf("wrong value adapter: got %q want %q", got, want)
				}
			},
		},
		{
			name: "normalizes builtin run adapter before keying",
			setup: func() (*catalog.Package, Evaluator, Config) {
				addr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "run", catalog.NoKey())
				req, reqDiags := buildrun.BuildRequest(buildrun.RequestArgs{
					Argv: []string{"/bin/echo"},
					Cwd:  t.TempDir(),
				}, tfdiags.SourceRange{})
				if reqDiags.HasErrors() {
					t.Fatalf("unexpected request diagnostics: %s", reqDiags.Err())
				}

				pkg := rootPkg([]*catalog.TargetDecl{{
					Addr:        addr,
					Driver:      "test",
					ConfigKey:   digest.FromStrings("target"),
					ConfigValid: true,
				}}, nil, nil)

				emptyEval := fakeEvaluator{
					targetFn: func(_ context.Context, r EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
						return EvalResult{Payload: &buildrun.Payload{Request: *req}, Digest: req.Key, Known: true}, nil
					},
				}
				explicitEval := fakeEvaluator{
					targetFn: func(_ context.Context, r EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
						return EvalResult{Payload: &buildrun.Payload{Request: *req, ValueAdapter: buildrun.ValueAdapterRawV1}, Digest: req.Key, Known: true}, nil
					},
				}

				emptyS := New(newTestSession(t), catalog.New(pkg), emptyEval, Config{})
				explicitS := New(newTestSession(t), catalog.New(pkg), explicitEval, Config{})

				emptySpec, emptyDiags := emptyS.ActionSpec(t.Context(), addr)
				if emptyDiags.HasErrors() {
					t.Fatalf("unexpected empty diagnostics: %s", emptyDiags.Err())
				}
				explicitSpec, explicitDiags := explicitS.ActionSpec(t.Context(), addr)
				if explicitDiags.HasErrors() {
					t.Fatalf("unexpected explicit diagnostics: %s", explicitDiags.Err())
				}

				if emptySpec.Key != explicitSpec.Key {
					t.Fatalf("expected same keys: empty=%s explicit=%s", emptySpec.Key, explicitSpec.Key)
				}
				return nil, nil, Config{} // already tested inline
			},
			addr: catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "run", catalog.NoKey()),
			check: func(t *testing.T, _ engine.Spec, _ tfdiags.Diagnostics) {
				// assertions happen in setup
			},
		},
		{
			name: "builtin run value data affects key",
			setup: func() (*catalog.Package, Evaluator, Config) {
				addr := catalog.ResourceAddr(root, catalog.TargetKindResource, "null_resource", "run", catalog.NoKey())
				req, reqDiags := buildrun.BuildRequest(buildrun.RequestArgs{
					Argv: []string{"/bin/echo"},
					Cwd:  t.TempDir(),
				}, tfdiags.SourceRange{})
				if reqDiags.HasErrors() {
					t.Fatalf("unexpected request diagnostics: %s", reqDiags.Err())
				}

				pkg := rootPkg([]*catalog.TargetDecl{{
					Addr:        addr,
					Driver:      "test",
					ConfigKey:   digest.FromStrings("target"),
					ConfigValid: true,
				}}, nil, nil)

				firstEval := fakeEvaluator{
					targetFn: func(_ context.Context, r EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
						return EvalResult{
							Payload: &buildrun.Payload{Request: *req, ValueAdapter: buildrun.ValueAdapterNullV1, ValueData: []byte(`{"triggers":{"image":"one"}}`)},
							Digest:  req.Key, Known: true,
						}, nil
					},
				}
				secondEval := fakeEvaluator{
					targetFn: func(_ context.Context, r EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
						return EvalResult{
							Payload: &buildrun.Payload{Request: *req, ValueAdapter: buildrun.ValueAdapterNullV1, ValueData: []byte(`{"triggers":{"image":"two"}}`)},
							Digest:  req.Key, Known: true,
						}, nil
					},
				}

				firstS := New(newTestSession(t), catalog.New(pkg), firstEval, Config{})
				secondS := New(newTestSession(t), catalog.New(pkg), secondEval, Config{})
				firstSpec, _ := firstS.ActionSpec(t.Context(), addr)
				secondSpec, _ := secondS.ActionSpec(t.Context(), addr)

				if firstSpec.Key == secondSpec.Key {
					t.Fatal("expected distinct keys for distinct value data")
				}
				return nil, nil, Config{}
			},
			addr:  catalog.ResourceAddr(root, catalog.TargetKindResource, "null_resource", "run", catalog.NoKey()),
			check: func(t *testing.T, _ engine.Spec, _ tfdiags.Diagnostics) {},
		},
		{
			name: "rejects broken explicit dependency",
			setup: func() (*catalog.Package, Evaluator, Config) {
				addr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "build", catalog.NoKey())
				missing := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "missing", catalog.NoKey())
				pkg := rootPkg([]*catalog.TargetDecl{{
					Addr:          addr,
					Driver:        "test",
					ConfigKey:     digest.FromStrings("target"),
					ConfigValid:   true,
					ExplicitDeps:  []catalog.Addr{missing},
					ProviderLocal: addrs.LocalProviderConfig{LocalName: "test"},
				}}, nil, nil)
				return pkg, fakeEvaluator{}, Config{}
			},
			addr: catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "build", catalog.NoKey()),
			check: func(t *testing.T, spec engine.Spec, diags tfdiags.Diagnostics) {
				if !diags.HasErrors() {
					t.Fatal("expected error")
				}
				if got := diags.Err().Error(); !strings.Contains(got, "Unknown explicit dependency") {
					t.Fatalf("missing explicit-dependency diagnostic in:\n%s", got)
				}
				if spec.Key != (digest.Digest{}) {
					t.Fatal("expected zero spec on failure")
				}
			},
		},
		{
			name: "requires concrete repeated module instance",
			setup: func() (*catalog.Package, Evaluator, Config) {
				child := root.Child("child", catalog.NoKey())
				pkg := rootPkg(nil, nil, nil)
				childPkg(pkg, "child",
					[]*catalog.TargetDecl{targetDecl(child, catalog.TargetKindResource, "test_resource", "build")},
					nil, nil,
				)
				eval := fakeEvaluator{
					importInstancesFn: func(_ context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
						if req.DeclAddr.Kind == catalog.TargetKindModule {
							return InstanceResult{Keys: []catalog.Key{catalog.IntKey(0), catalog.IntKey(1)}, Shape: InstanceShapeList}, nil
						}
						return InstanceResult{Keys: []catalog.Key{catalog.NoKey()}, Shape: InstanceShapeSingle}, nil
					},
				}
				return pkg, eval, Config{}
			},
			addr: catalog.ResourceAddr(root.Child("child", catalog.NoKey()), catalog.TargetKindResource, "test_resource", "build", catalog.NoKey()),
			check: func(t *testing.T, _ engine.Spec, diags tfdiags.Diagnostics) {
				if !diags.HasErrors() {
					t.Fatal("expected error")
				}
				if got := diags.Err().Error(); !strings.Contains(got, "concrete module instance") {
					t.Fatalf("missing repeated-module diagnostic in:\n%s", got)
				}
			},
		},
		{
			name: "supports concrete repeated module instance with distinct keys",
			setup: func() (*catalog.Package, Evaluator, Config) {
				child := root.Child("child", catalog.NoKey())
				pkg := rootPkg(nil, nil, nil)
				childPkg(pkg, "child",
					[]*catalog.TargetDecl{targetDecl(child, catalog.TargetKindResource, "test_resource", "build")},
					nil, nil,
				)
				eval := fakeEvaluator{
					importInstancesFn: func(_ context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
						if req.DeclAddr.Kind == catalog.TargetKindModule {
							return InstanceResult{Keys: []catalog.Key{catalog.IntKey(0), catalog.IntKey(1)}, Shape: InstanceShapeList}, nil
						}
						return InstanceResult{Keys: []catalog.Key{catalog.NoKey()}, Shape: InstanceShapeSingle}, nil
					},
					targetFn: func(_ context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
						return knownTargetResult(req.Addr), nil
					},
				}

				s := New(newTestSession(t), catalog.New(pkg), eval, Config{})
				aAddr := catalog.ResourceAddr(root.Child("child", catalog.IntKey(0)), catalog.TargetKindResource, "test_resource", "build", catalog.NoKey())
				bAddr := catalog.ResourceAddr(root.Child("child", catalog.IntKey(1)), catalog.TargetKindResource, "test_resource", "build", catalog.NoKey())

				aSpec, aDiags := s.ActionSpec(t.Context(), aAddr)
				if aDiags.HasErrors() {
					t.Fatalf("unexpected a diagnostics: %s", aDiags.Err())
				}
				bSpec, bDiags := s.ActionSpec(t.Context(), bAddr)
				if bDiags.HasErrors() {
					t.Fatalf("unexpected b diagnostics: %s", bDiags.Err())
				}
				if aSpec.Key == bSpec.Key {
					t.Fatal("expected distinct keys for different module instances")
				}
				if got, want := bSpec.Name, bAddr.String(); got != want {
					t.Fatalf("wrong name: got %q want %q", got, want)
				}
				return nil, nil, Config{}
			},
			addr:  catalog.ResourceAddr(root.Child("child", catalog.IntKey(0)), catalog.TargetKindResource, "test_resource", "build", catalog.NoKey()),
			check: func(t *testing.T, _ engine.Spec, _ tfdiags.Diagnostics) {},
		},
		{
			name: "ignores known module output for exec deps",
			setup: func() (*catalog.Package, Evaluator, Config) {
				child := root.Child("child", catalog.NoKey())
				pkg := rootPkg(
					[]*catalog.TargetDecl{targetDecl(root, catalog.TargetKindResource, "test_resource", "consumer")},
					nil, nil,
				)
				childPkg(pkg, "child", nil, []*catalog.OutputDecl{outputDecl(child, "image")}, nil)

				eval := fakeEvaluator{
					targetFn: func(_ context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
						r := knownTargetResult(req.Addr)
						r.Refs = []catalog.Addr{catalog.ModuleAddr(child)}
						return r, nil
					},
					outputFn: func(_ context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
						return EvalResult{Value: cty.StringVal("stable"), Digest: digest.FromStrings("output"), Known: true}, nil
					},
				}
				return pkg, eval, Config{}
			},
			addr: catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "consumer", catalog.NoKey()),
			check: func(t *testing.T, spec engine.Spec, diags tfdiags.Diagnostics) {
				if diags.HasErrors() {
					t.Fatalf("unexpected diagnostics: %s", diags.Err())
				}
				if got := len(spec.ExecDeps); got != 0 {
					t.Fatalf("expected no exec deps for known module output, got %d", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg, eval, cfg := tt.setup()
			if pkg == nil {
				// Test completed inline in setup.
				return
			}
			solver := newSolver(t, pkg, eval, cfg)
			spec, diags := solver.ActionSpec(t.Context(), tt.addr)
			tt.check(t, spec, diags)
		})
	}
}

func TestSolver_ReverseDeps(t *testing.T) {
	root := catalog.RootModule()

	tests := []struct {
		name         string
		setup        func() (*catalog.Package, Evaluator, Config)
		addr         catalog.Addr
		wantAddrs    []catalog.Addr
		wantComplete bool
	}{
		{
			name: "finds reverse deps for target",
			setup: func() (*catalog.Package, Evaluator, Config) {
				baseAddr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "base", catalog.NoKey())
				pkg := rootPkg(
					[]*catalog.TargetDecl{
						targetDecl(root, catalog.TargetKindResource, "test_resource", "base"),
						targetDecl(root, catalog.TargetKindResource, "test_resource", "consumer"),
					},
					[]*catalog.OutputDecl{outputDecl(root, "value")},
					nil,
				)
				eval := fakeEvaluator{
					targetFn: func(_ context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
						r := knownTargetResult(req.Addr)
						if req.Addr.Name == "consumer" {
							r.Refs = []catalog.Addr{baseAddr}
						}
						return r, nil
					},
					outputFn: func(_ context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
						return EvalResult{
							Value:  cty.StringVal("hello"),
							Digest: digest.FromStrings("out"),
							Known:  true,
							Refs:   []catalog.Addr{baseAddr},
						}, nil
					},
				}
				return pkg, eval, Config{}
			},
			addr: catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "base", catalog.NoKey()),
			wantAddrs: []catalog.Addr{
				catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "consumer", catalog.NoKey()),
			},
			wantComplete: true,
		},
		{
			name: "finds reverse deps for output",
			setup: func() (*catalog.Package, Evaluator, Config) {
				child := root.Child("child", catalog.NoKey())
				childOutput := catalog.OutputAddr(child, "base")
				pkg := rootPkg(nil, []*catalog.OutputDecl{outputDecl(root, "consumer")}, nil)
				childPkg(pkg, "child", nil, []*catalog.OutputDecl{outputDecl(child, "base")}, nil)

				eval := fakeEvaluator{
					outputFn: func(_ context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
						if req.Addr.Name == "consumer" {
							return EvalResult{
								Value:  cty.StringVal("hello"),
								Digest: digest.FromStrings("consumer"),
								Known:  true,
								Refs:   []catalog.Addr{childOutput},
							}, nil
						}
						return EvalResult{Value: cty.StringVal("hello"), Digest: digest.FromStrings("base"), Known: true}, nil
					},
				}
				return pkg, eval, Config{}
			},
			addr: catalog.OutputAddr(root.Child("child", catalog.NoKey()), "base"),
			wantAddrs: []catalog.Addr{
				catalog.OutputAddr(root, "consumer"),
			},
			wantComplete: true,
		},
		{
			name: "marks partial when deferred subtrees omitted",
			setup: func() (*catalog.Package, Evaluator, Config) {
				baseAddr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "base", catalog.NoKey())
				pkg := rootPkg(
					[]*catalog.TargetDecl{
						targetDecl(root, catalog.TargetKindResource, "test_resource", "base"),
						targetDecl(root, catalog.TargetKindResource, "test_resource", "consumer"),
					},
					nil, nil,
				)
				child := root.Child("child", catalog.NoKey())
				childPkg(pkg, "child", nil, []*catalog.OutputDecl{outputDecl(child, "value")}, nil)

				eval := fakeEvaluator{
					importInstancesFn: func(_ context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
						if req.DeclAddr.Kind == catalog.TargetKindModule {
							return InstanceResult{Deferred: true, Shape: InstanceShapeMap}, nil
						}
						return InstanceResult{Keys: []catalog.Key{catalog.NoKey()}, Shape: InstanceShapeSingle}, nil
					},
					targetFn: func(_ context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
						if req.Addr.Name == "consumer" {
							r := knownTargetResult(req.Addr)
							r.Refs = []catalog.Addr{baseAddr}
							return r, nil
						}
						return knownTargetResult(req.Addr), nil
					},
				}
				return pkg, eval, Config{}
			},
			addr: catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "base", catalog.NoKey()),
			wantAddrs: []catalog.Addr{
				catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "consumer", catalog.NoKey()),
			},
			wantComplete: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg, eval, cfg := tt.setup()
			solver := newSolver(t, pkg, eval, cfg)
			got, diags := solver.ReverseDeps(t.Context(), tt.addr)
			if diags.HasErrors() {
				t.Fatalf("unexpected diagnostics: %s", diags.Err())
			}
			if got.Complete != tt.wantComplete {
				t.Fatalf("Complete = %v, want %v", got.Complete, tt.wantComplete)
			}
			if diff := cmp.Diff(tt.wantAddrs, got.Addrs, addrCmp); diff != "" {
				t.Fatalf("wrong addrs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSolver_TargetValue(t *testing.T) {
	root := catalog.RootModule()

	t.Run("decodes builtin run raw value", func(t *testing.T) {
		addr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "run", catalog.NoKey())
		req, reqDiags := buildrun.BuildRequest(buildrun.RequestArgs{
			Argv:   []string{"/bin/echo", "hello"},
			Cwd:    t.TempDir(),
			Decode: buildrun.DecodeModeJSON,
		}, tfdiags.SourceRange{})
		if reqDiags.HasErrors() {
			t.Fatalf("unexpected request diagnostics: %s", reqDiags.Err())
		}

		pkg := rootPkg([]*catalog.TargetDecl{{
			Addr:        addr,
			Driver:      "test",
			ConfigKey:   digest.FromStrings("target"),
			ConfigValid: true,
		}}, nil, nil)

		payload := buildrun.Payload{Request: *req, ValueAdapter: buildrun.ValueAdapterRawV1}
		eval := fakeEvaluator{
			targetFn: func(_ context.Context, r EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
				return EvalResult{Payload: &payload, Digest: req.Key, Known: true}, nil
			},
		}
		solver := newSolver(t, pkg, eval, Config{})

		spec, specDiags := solver.ActionSpec(t.Context(), addr)
		if specDiags.HasErrors() {
			t.Fatalf("unexpected action diagnostics: %s", specDiags.Err())
		}

		resultPayload, err := buildrun.EncodeResult(*req, buildrun.Result{
			ExitCode: 0,
			Stdout:   []byte("{\"value\":\"hello\"}\n"),
			Decoded: &buildrun.DecodedValue{
				Mode: buildrun.DecodeModeJSON,
				JSON: []byte("{\"value\":\"hello\"}\n"),
			},
		})
		if err != nil {
			t.Fatalf("encode result: %s", err)
		}

		runtimeSolver := New(newTestSession(t), catalog.New(pkg), eval, Config{
			Engine: newTestEngineWithRecords(t, map[digest.Digest]engine.Record{
				spec.Key: {
					ActionKey: spec.Key,
					OutputKey: digest.FromBytes(resultPayload),
					Payload:   resultPayload,
					Volatile:  true,
				},
			}),
		})

		result, resultDiags := runtimeSolver.TargetValue(t.Context(), addr)
		if resultDiags.HasErrors() {
			t.Fatalf("unexpected target value diagnostics: %s", resultDiags.Err())
		}
		if !result.Known {
			t.Fatal("expected known target value")
		}
		if got, want := result.Value.GetAttr("stdout"), cty.StringVal("{\"value\":\"hello\"}\n"); !got.RawEquals(want) {
			t.Fatalf("wrong stdout: got %s want %s", got.GoString(), want.GoString())
		}
		jsonVal := result.Value.GetAttr("json")
		if got, want := jsonVal.GetAttr("value"), cty.StringVal("hello"); !got.RawEquals(want) {
			t.Fatalf("wrong json value: got %s want %s", got.GoString(), want.GoString())
		}
	})

	t.Run("decodes builtin run null value", func(t *testing.T) {
		addr := catalog.ResourceAddr(root, catalog.TargetKindResource, "null_resource", "run", catalog.NoKey())
		req, reqDiags := buildrun.BuildRequest(buildrun.RequestArgs{
			Argv: []string{"/bin/echo", "hello"},
			Cwd:  t.TempDir(),
		}, tfdiags.SourceRange{})
		if reqDiags.HasErrors() {
			t.Fatalf("unexpected request diagnostics: %s", reqDiags.Err())
		}

		pkg := rootPkg([]*catalog.TargetDecl{{
			Addr:        addr,
			Driver:      "test",
			ConfigKey:   digest.FromStrings("target"),
			ConfigValid: true,
		}}, nil, nil)

		payload := buildrun.Payload{
			Request:      *req,
			ValueAdapter: buildrun.ValueAdapterNullV1,
			ValueData:    []byte(`{"triggers":{"image":"cgr.dev/example:latest"}}`),
		}
		eval := fakeEvaluator{
			targetFn: func(_ context.Context, r EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
				return EvalResult{Payload: &payload, Digest: req.Key, Known: true}, nil
			},
		}
		solver := newSolver(t, pkg, eval, Config{})

		spec, specDiags := solver.ActionSpec(t.Context(), addr)
		if specDiags.HasErrors() {
			t.Fatalf("unexpected action diagnostics: %s", specDiags.Err())
		}

		resultPayload, err := buildrun.EncodeResult(*req, buildrun.Result{
			ExitCode: 0,
			Stdout:   []byte("ok\n"),
		})
		if err != nil {
			t.Fatalf("encode result: %s", err)
		}

		runtimeSolver := New(newTestSession(t), catalog.New(pkg), eval, Config{
			Engine: newTestEngineWithRecords(t, map[digest.Digest]engine.Record{
				spec.Key: {
					ActionKey: spec.Key,
					OutputKey: digest.FromBytes(resultPayload),
					Payload:   resultPayload,
					Volatile:  true,
				},
			}),
		})

		result, resultDiags := runtimeSolver.TargetValue(t.Context(), addr)
		if resultDiags.HasErrors() {
			t.Fatalf("unexpected target value diagnostics: %s", resultDiags.Err())
		}
		if !result.Known {
			t.Fatal("expected known target value")
		}
		if result.Digest == (digest.Digest{}) {
			t.Fatal("expected non-zero value digest")
		}
		triggersVal := result.Value.GetAttr("triggers")
		if got, want := triggersVal.Index(cty.StringVal("image")), cty.StringVal("cgr.dev/example:latest"); !got.RawEquals(want) {
			t.Fatalf("wrong triggers: got %s want %s", got.GoString(), want.GoString())
		}
	})

	t.Run("defers when no runtime", func(t *testing.T) {
		addr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "run", catalog.NoKey())
		pkg := rootPkg([]*catalog.TargetDecl{{
			Addr:        addr,
			Driver:      "test",
			ConfigKey:   digest.FromStrings("target"),
			ConfigValid: true,
		}}, nil, nil)

		solver := newSolver(t, pkg, fakeEvaluator{}, Config{})
		_, diags := solver.TargetValue(t.Context(), addr)
		if !diags.HasErrors() {
			t.Fatal("expected error for target value without engine")
		}
	})
}

func TestSolver_ActionSpec_WithProviders(t *testing.T) {
	root := catalog.RootModule()

	t.Run("provider remap and module depends_on", func(t *testing.T) {
		child := root.Child("child", catalog.NoKey())
		testProv := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "foo", "test")

		childTarget := targetDecl(child, catalog.TargetKindResource, "test_resource", "child")
		childTarget.ProviderLocal = addrs.LocalProviderConfig{LocalName: "test", Alias: "alt"}

		afterTarget := targetDecl(root, catalog.TargetKindResource, "test_resource", "after")
		afterTarget.ProviderLocal = addrs.LocalProviderConfig{LocalName: "test", Alias: "src"}
		afterTarget.ExplicitDeps = []catalog.Addr{catalog.ModuleAddr(child)}

		pkg := rootPkg([]*catalog.TargetDecl{afterTarget}, nil, nil)
		pkg.Providers = []*catalog.ProviderDecl{{
			Module:      root,
			Local:       addrs.LocalProviderConfig{LocalName: "test", Alias: "src"},
			Provider:    testProv,
			ConfigKey:   digest.FromStrings("provider-src"),
			ConfigValid: true,
		}}

		childP := childPkg(pkg, "child", []*catalog.TargetDecl{childTarget}, nil, nil)
		childP.ParentImport.ProviderPass = []catalog.ProviderPass{{
			InChild:  addrs.LocalProviderConfig{LocalName: "test", Alias: "alt"},
			InParent: addrs.LocalProviderConfig{LocalName: "test", Alias: "src"},
		}}

		testResourceValue := cty.ObjectVal(map[string]cty.Value{
			"id":              cty.StringVal("test-id"),
			"value":           cty.StringVal("hello"),
			"interrupt_count": cty.NumberIntVal(0),
		})
		eval := fakeEvaluator{
			targetFn: func(_ context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
				return EvalResult{
					Value:  testResourceValue,
					Digest: digest.FromStrings("target-config", req.Addr.String()),
					Known:  true,
				}, nil
			},
		}

		solver := newSolver(t, pkg, eval, Config{Providers: newTestProviderSession(t)})

		childAddr := catalog.ResourceAddr(child, catalog.TargetKindResource, "test_resource", "child", catalog.NoKey())
		childSpec, childDiags := solver.ActionSpec(t.Context(), childAddr)
		if childDiags.HasErrors() {
			t.Fatalf("unexpected child diagnostics: %s", childDiags.Err())
		}
		if got, want := childSpec.Runner.Kind, string(catalog.RunnerKindProviderResource); got != want {
			t.Fatalf("wrong child runner kind: got %q want %q", got, want)
		}

		afterAddr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "after", catalog.NoKey())
		afterSpec, afterDiags := solver.ActionSpec(t.Context(), afterAddr)
		if afterDiags.HasErrors() {
			t.Fatalf("unexpected after diagnostics: %s", afterDiags.Err())
		}
		if got, want := len(afterSpec.After), 1; got != want {
			t.Fatalf("wrong after count: got %d want %d", got, want)
		}
		if got, want := afterSpec.After[0], childSpec.Key; got != want {
			t.Fatalf("wrong after key: got %s want %s", got, want)
		}
	})

	t.Run("repeated targets produce distinct action keys", func(t *testing.T) {
		pkg := rootPkg([]*catalog.TargetDecl{
			targetDecl(root, catalog.TargetKindResource, "test_resource", "build"),
		}, nil, nil)

		eval := fakeEvaluator{
			targetInstancesFn: func(_ context.Context, req EvalTargetInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
				return InstanceResult{
					Keys:  []catalog.Key{catalog.StringKey("alpha"), catalog.StringKey("beta")},
					Shape: InstanceShapeMap,
				}, nil
			},
			targetFn: func(_ context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
				return EvalResult{
					Value: cty.ObjectVal(map[string]cty.Value{
						"id":              cty.StringVal("test-" + req.Addr.Key.Str),
						"value":           cty.StringVal(req.Addr.Key.Str),
						"interrupt_count": cty.NumberIntVal(0),
					}),
					Digest: digest.FromStrings("target-config", req.Addr.String()),
					Known:  true,
				}, nil
			},
		}
		solver := newSolver(t, pkg, eval, Config{Providers: newTestProviderSession(t)})

		alphaAddr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "build", catalog.StringKey("alpha"))
		betaAddr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "build", catalog.StringKey("beta"))

		alphaSpec, alphaDiags := solver.ActionSpec(t.Context(), alphaAddr)
		if alphaDiags.HasErrors() {
			t.Fatalf("unexpected alpha diagnostics: %s", alphaDiags.Err())
		}
		betaSpec, betaDiags := solver.ActionSpec(t.Context(), betaAddr)
		if betaDiags.HasErrors() {
			t.Fatalf("unexpected beta diagnostics: %s", betaDiags.Err())
		}
		if alphaSpec.Key == betaSpec.Key {
			t.Fatal("expected distinct keys for repeated targets with different configs")
		}
		alphaPayload, ok := alphaSpec.Runner.Payload.(buildprovider.TargetPayload)
		if !ok {
			t.Fatalf("wrong alpha payload type: got %T", alphaSpec.Runner.Payload)
		}
		betaPayload, ok := betaSpec.Runner.Payload.(buildprovider.TargetPayload)
		if !ok {
			t.Fatalf("wrong beta payload type: got %T", betaSpec.Runner.Payload)
		}
		if alphaPayload.Request.Key == betaPayload.Request.Key {
			t.Fatal("expected distinct request keys")
		}
	})

	t.Run("rejects unresolved aliased provider", func(t *testing.T) {
		addr := catalog.ResourceAddr(root, catalog.TargetKindResource, "test_resource", "build", catalog.NoKey())
		target := targetDecl(root, catalog.TargetKindResource, "test_resource", "build")
		target.ProviderLocal = addrs.LocalProviderConfig{LocalName: "test", Alias: "alt"}
		pkg := rootPkg([]*catalog.TargetDecl{target}, nil, nil)

		solver := newSolver(t, pkg, fakeEvaluator{
			targetFn: func(_ context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
				return EvalResult{Value: cty.EmptyObjectVal, Digest: digest.FromStrings("t"), Known: true}, nil
			},
		}, Config{Providers: newTestProviderSession(t)})

		spec, diags := solver.ActionSpec(t.Context(), addr)
		if !diags.HasErrors() {
			t.Fatal("expected error")
		}
		if got := diags.Err().Error(); !strings.Contains(got, "Aliased provider configuration") {
			t.Fatalf("missing aliased-provider diagnostic in:\n%s", got)
		}
		if spec.Key != (digest.Digest{}) {
			t.Fatal("expected zero spec on failure")
		}
	})
}

func TestSolver_Binding(t *testing.T) {
	root := catalog.RootModule()
	testProv := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "foo", "test")

	t.Run("lowers provider config request", func(t *testing.T) {
		pkg := rootPkg(nil, nil, nil)
		pkg.Providers = []*catalog.ProviderDecl{{
			Module:      root,
			Local:       addrs.LocalProviderConfig{LocalName: "test"},
			Provider:    testProv,
			ConfigKey:   digest.FromStrings("provider-decl"),
			ConfigValid: true,
		}}

		eval := fakeEvaluator{
			providerFn: func(_ context.Context, req EvalProviderRequest) (EvalResult, tfdiags.Diagnostics) {
				config := req.Schema.Block.EmptyValue()
				values := config.AsValueMap()
				values["resource_prefix"] = cty.StringVal("images")
				return EvalResult{
					Value:  cty.ObjectVal(values),
					Digest: digest.FromStrings("provider-config"),
					Known:  true,
				}, nil
			},
		}
		solver := newSolver(t, pkg, eval, Config{Providers: newTestProviderSession(t)})

		binding, diags := solver.Binding(t.Context(), root, addrs.LocalProviderConfig{LocalName: "test"})
		if diags.HasErrors() {
			t.Fatalf("unexpected diagnostics: %s", diags.Err())
		}
		if binding.ConfigRequest == nil {
			t.Fatal("expected lowered config request")
		}
		config, configDiags := buildprovider.DecodeConfigRequest(*binding.ConfigRequest, tfdiags.SourceRange{})
		if configDiags.HasErrors() {
			t.Fatalf("unexpected config decode diagnostics: %s", configDiags.Err())
		}
		if got, want := config.GetAttr("resource_prefix"), cty.StringVal("images"); !got.RawEquals(want) {
			t.Fatalf("wrong config value: got %s want %s", got.GoString(), want.GoString())
		}
	})

	t.Run("binding keys differ across repeated module instances", func(t *testing.T) {
		child := root.Child("child", catalog.NoKey())
		pkg := rootPkg(nil, nil, nil)
		pkg.Imports = []catalog.Import{{
			Name:         "child",
			Module:       root,
			TargetModule: child,
		}}
		pkg.Children["child"] = &catalog.Package{
			Name:         "child",
			Module:       child,
			Parent:       pkg,
			ParentImport: &pkg.Imports[0],
			Providers: []*catalog.ProviderDecl{{
				Module:      child,
				Local:       addrs.LocalProviderConfig{LocalName: "test"},
				Provider:    testProv,
				ConfigKey:   digest.FromStrings("provider-decl"),
				ConfigValid: true,
			}},
			Children: map[string]*catalog.Package{},
		}
		pkg.ChildOrder = []string{"child"}

		eval := fakeEvaluator{
			importInstancesFn: func(_ context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
				if req.DeclAddr.Kind == catalog.TargetKindModule {
					return InstanceResult{
						Keys:  []catalog.Key{catalog.StringKey("amd64"), catalog.StringKey("arm64")},
						Shape: InstanceShapeMap,
					}, nil
				}
				return InstanceResult{Keys: []catalog.Key{catalog.NoKey()}, Shape: InstanceShapeSingle}, nil
			},
			providerFn: func(_ context.Context, req EvalProviderRequest) (EvalResult, tfdiags.Diagnostics) {
				prefix := "images-default"
				if step, ok := req.Module.LastStep(); ok && step.Key.Kind == catalog.KeyKindString {
					prefix = "images-" + step.Key.Str
				}
				config := req.Schema.Block.EmptyValue()
				values := config.AsValueMap()
				values["resource_prefix"] = cty.StringVal(prefix)
				return EvalResult{
					Value:  cty.ObjectVal(values),
					Digest: digest.FromStrings("provider-config", prefix),
					Known:  true,
				}, nil
			},
		}
		solver := newSolver(t, pkg, eval, Config{Providers: newTestProviderSession(t)})

		local := addrs.LocalProviderConfig{LocalName: "test"}
		amd64Binding, amd64Diags := solver.Binding(t.Context(), root.Child("child", catalog.StringKey("amd64")), local)
		if amd64Diags.HasErrors() {
			t.Fatalf("unexpected amd64 diagnostics: %s", amd64Diags.Err())
		}
		arm64Binding, arm64Diags := solver.Binding(t.Context(), root.Child("child", catalog.StringKey("arm64")), local)
		if arm64Diags.HasErrors() {
			t.Fatalf("unexpected arm64 diagnostics: %s", arm64Diags.Err())
		}

		if amd64Binding.ConfigKey == arm64Binding.ConfigKey {
			t.Fatal("expected distinct binding keys for different module instances")
		}
		if amd64Binding.ConfigRequest == nil || arm64Binding.ConfigRequest == nil {
			t.Fatal("expected config requests for both bindings")
		}
		if amd64Binding.ConfigRequest.Key == arm64Binding.ConfigRequest.Key {
			t.Fatal("expected distinct config request keys")
		}
	})
}

func TestSolver_OutputValue_WithRuntime(t *testing.T) {
	root := catalog.RootModule()

	t.Run("uses cached runtime value when conditional skips absent counted module", func(t *testing.T) {
		child := root.Child("child", catalog.NoKey())
		cacheAddr := catalog.ResourceAddr(root, catalog.TargetKindData, "external", "cache_lookup", catalog.NoKey())

		pkg := rootPkg(
			[]*catalog.TargetDecl{targetDecl(root, catalog.TargetKindData, "external", "cache_lookup")},
			[]*catalog.OutputDecl{outputDecl(root, "dev_ref")},
			nil,
		)
		childPkg(pkg, "child", nil, []*catalog.OutputDecl{outputDecl(child, "image")}, nil)

		cacheReq, cacheReqDiags := buildrun.BuildRequest(buildrun.RequestArgs{
			Argv:   []string{"/bin/sh", "-c", "echo '{\"image_ref\":\"cached\"}'"},
			Cwd:    t.TempDir(),
			Decode: buildrun.DecodeModeJSON,
		}, tfdiags.SourceRange{})
		if cacheReqDiags.HasErrors() {
			t.Fatalf("unexpected cache request diagnostics: %s", cacheReqDiags.Err())
		}
		cachePayload := buildrun.Payload{Request: *cacheReq, ValueAdapter: buildrun.ValueAdapterRawV1}

		eval := fakeEvaluator{
			importInstancesFn: func(_ context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
				if req.DeclAddr.Kind == catalog.TargetKindModule {
					return InstanceResult{Keys: nil, Shape: InstanceShapeList}, nil
				}
				return InstanceResult{Keys: []catalog.Key{catalog.NoKey()}, Shape: InstanceShapeSingle}, nil
			},
			targetFn: func(_ context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
				return EvalResult{Payload: &cachePayload, Digest: cacheReq.Key, Known: true}, nil
			},
			outputFn: func(_ context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
				if req.Runtime != nil {
					return EvalResult{
						Value:  cty.StringVal("cached"),
						Digest: digest.FromStrings("cached-output"),
						Known:  true,
					}, nil
				}
				return EvalResult{Deferred: true, Refs: []catalog.Addr{cacheAddr}}, nil
			},
		}

		staticSolver := newSolver(t, pkg, eval, Config{})
		cacheSpec, diags := staticSolver.ActionSpec(t.Context(), cacheAddr)
		if diags.HasErrors() {
			t.Fatalf("unexpected cache spec diagnostics: %s", diags.Err())
		}

		recordPayload, err := buildrun.EncodeResult(*cacheReq, buildrun.Result{
			ExitCode: 0,
			Stdout:   []byte(`{"image_ref":"cached"}`),
			Decoded:  &buildrun.DecodedValue{Mode: buildrun.DecodeModeJSON, JSON: []byte(`{"image_ref":"cached"}`)},
		})
		if err != nil {
			t.Fatalf("encode result: %s", err)
		}

		runtimeSolver := New(newTestSession(t), catalog.New(pkg), eval, Config{
			Engine: newTestEngineWithRecords(t, map[digest.Digest]engine.Record{
				cacheSpec.Key: {
					ActionKey: cacheSpec.Key,
					OutputKey: cacheSpec.Key,
					Payload:   recordPayload,
				},
			}),
		})

		result, resultDiags := runtimeSolver.OutputValue(t.Context(), catalog.OutputAddr(root, "dev_ref"))
		if resultDiags.HasErrors() {
			t.Fatalf("unexpected output diagnostics: %s", resultDiags.Err())
		}
		if !result.Known || result.Deferred {
			t.Fatalf("expected known cached output, got known=%t deferred=%t", result.Known, result.Deferred)
		}
		if !result.Value.RawEquals(cty.StringVal("cached")) {
			t.Fatalf("wrong output value: got %s", result.Value.GoString())
		}
	})
}
