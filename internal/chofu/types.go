// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"unique"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/instances"
)

// ID is a dense vertex identifier for O(1) array indexing.
// All vertex data is stored in flat arrays indexed by ID.
type ID int32

// VertexKind classifies what a vertex represents in the evaluation DAG.
type VertexKind uint8

const (
	KindVariable       VertexKind = iota // module input variable
	KindLocal                            // local value
	KindOutput                           // module output value
	KindResource                         // managed resource instance
	KindDataSource                       // data source instance
	KindProviderConfig                   // provider configuration block
	KindModuleExpand                     // module instance entry gate (all child content depends on this)
	KindModuleClose                      // module instance exit gate (depends on all child content)
)

// Vertex is a node in the flat evaluation DAG. Exactly one of the
// kind-specific config fields is non-nil, determined by Kind.
type Vertex struct {
	Kind    VertexKind
	Module  addrs.ModuleInstance     // which module instance this belongs to
	RepData instances.RepetitionData // count.index / each.key / each.value
	Name    string                   // variable/local/output name, or resource type.name

	// Kind-specific configuration. Exactly one is set per Kind.
	VariableCfg *configs.Variable // KindVariable
	VariableVal cty.Value         // KindVariable: resolved value from parent module call
	LocalExpr   hcl.Expression    // KindLocal
	LocalVal    cty.Value         // KindLocal: expansion-time value (skip walk re-eval if known)
	OutputCfg    *configs.Output   // KindOutput
	ResourceCfg  *configs.Resource // KindResource, KindDataSource
	ProviderBody hcl.Body          // KindProviderConfig

	// Module expand/close fields.
	ModuleCallExprs []hcl.Expression         // KindModuleExpand: parent module call arg expressions (for DAG edges)
	ModuleCallAttrs map[string]hcl.Expression // KindModuleExpand: variable name → parent expression (for walk re-evaluation)
	ModuleChildVars map[string]*configs.Variable // KindModuleExpand: child variable configs (for type conversion)
	ModuleDependsOn []hcl.Traversal           // KindModuleExpand: parent module call depends_on

	// Resource/DataSource fields.
	ResourceAddr addrs.AbsResourceInstance // fully-qualified instance address
	ProviderAddr addrs.Provider            // which provider type
	Schema       *configschema.Block       // resource/data/provider schema (set after schema fetch)

	// Provider config fields.
	ProviderAlias string // provider alias ("" for default)
}

// addrKey is a composite key for looking up vertices by module instance + name.
// Both fields are unique.Handle so map lookups compare pointers instead of
// hashing full strings. With 66k vertices sharing ~200 distinct names across
// 1,881 modules, this eliminates significant hashing overhead.
type addrKey struct {
	Module unique.Handle[string]
	Name   unique.Handle[string]
}

// Graph is a flat, integer-indexed dependency DAG. All lookups are O(1)
// via maps or array indexing. No interface{} boxing.
type Graph struct {
	Verts   []Vertex // dense, indexed by ID
	Deps    [][]ID   // Deps[v] = predecessors of v
	RevDeps [][]ID   // RevDeps[v] = successors of v (built from Deps)

	// Lookup tables for resolving references to vertex IDs.
	Vars      map[addrKey]ID // variable vertices
	Locals    map[addrKey]ID // local vertices
	Outputs   map[addrKey]ID // output vertices
	Providers map[string]ID  // provider config key → ID

	// Resource instances grouped by AbsResource for GetResource aggregation.
	// Key is AbsResource.String(), value is the list of instance vertex IDs.
	ResInst map[string][]ID

	// Module expand/close vertices keyed by module instance string.
	// Used to wire child→expand and close→child edges.
	Expands map[string]ID // module instance → expand vertex ID
	Closes  map[string]ID // module instance → close vertex ID

	// ChildCloses maps (parent module, call name) → close vertex IDs for
	// any instance of that child module. Used by childModuleDeps for O(1) lookup.
	ChildCloses map[addrKey][]ID

	// ChildOutputs maps (parent module, call name) → output vertex IDs for
	// any instance of that child module. Built during Pass 1 so that
	// childModuleOutputIDs and collectChildOutputs are O(1) lookups
	// instead of scanning all outputs.
	ChildOutputs map[addrKey][]ID

	// InlinedLocals stores values for local vertices that were compacted
	// out of the graph during CompactGraph. These are locals whose values
	// were wholly known at expansion time (LocalVal != NilVal) — they
	// don't need walk-time evaluation.
	//
	// GetLocalValue checks this table first before falling back to the
	// Locals → Results lookup for uncached locals that remain as vertices.
	InlinedLocals map[addrKey]cty.Value
}

// EdgeStats tracks how many edges come from each source in the DAG.
// Built during BuildGraph for observability (logged + emitted as trace attributes).
type EdgeStats struct {
	Total      int // sum of all edge categories
	ExpandGate int // child → expand (structural gate)
	CloseGate  int // close → children (structural gate)
	HCLRefs    int // expression-derived references
	DependsOn  int // explicit depends_on
	Provider   int // resource → provider config
	ExpandRefs int // expand → parent call expressions
	ParentExp  int // expand → parent expand (nesting)
}

// Results stores the evaluated value for each vertex. The DAG walk order
// guarantees that when a vertex V is evaluated, all of V's dependencies
// have already written their values — so reads don't need locks.
type Results struct {
	Values []cty.Value
}

// NewResults creates a results array for the given number of vertices.
func NewResults(n int) *Results {
	return &Results{
		Values: make([]cty.Value, n),
	}
}
