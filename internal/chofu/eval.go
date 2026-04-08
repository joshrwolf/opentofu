// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"fmt"
	"unique"

	"github.com/zclconf/go-cty/cty"

	"github.com/zclconf/go-cty/cty/function"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/instances"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// instanceShapeKind describes how a set of graph instances is keyed.
type instanceShapeKind int

const (
	shapeSingleton instanceShapeKind = iota // all NoKey
	shapeCount                              // at least one IntKey
	shapeForEach                            // at least one StringKey
)

// instanceShape inspects the actual instance keys in the graph to determine
// the collection shape. This is authoritative over the config's count/for_each
// declaration — when expansion falls back to a singleton, the graph has NoKey
// instances regardless of what the config says.
func instanceShape(g *Graph, ids []ID) instanceShapeKind {
	for _, id := range ids {
		switch g.Verts[id].ResourceAddr.Resource.Key.(type) {
		case addrs.IntKey:
			return shapeCount
		case addrs.StringKey:
			return shapeForEach
		}
	}
	return shapeSingleton
}

// instOutputs groups a module instance's output values by instance key.
type instOutputs struct {
	key  addrs.InstanceKey
	vals map[string]cty.Value
}

// moduleInstanceShape inspects output vertex instance keys to determine the
// collection shape for a module call.
func moduleInstanceShape(instances []instOutputs) instanceShapeKind {
	for _, inst := range instances {
		switch inst.key.(type) {
		case addrs.IntKey:
			return shapeCount
		case addrs.StringKey:
			return shapeForEach
		}
	}
	return shapeSingleton
}

// resolveShape returns the graph-derived shape, falling back to the config's
// declared intent when all instances are NoKey (singleton expansion fallback).
func resolveShape(shape instanceShapeKind, hasCount bool, hasForEach bool) instanceShapeKind {
	if shape == shapeSingleton {
		if hasCount {
			return shapeCount
		}
		if hasForEach {
			return shapeForEach
		}
	}
	return shape
}

// evalData implements lang.Data backed by the flat results array and
// graph lookup maps. It resolves references by indexing into Results.Values
// using vertex IDs from the Graph's lookup tables.
type evalData struct {
	graph     *Graph
	results   *Results
	config    *configs.Config          // full config tree for navigation
	module    addrs.ModuleInstance     // current evaluation scope
	repData   instances.RepetitionData // count.index / each.key / each.value
	rootDir   string                   // path.root
	workDir   string                   // path.cwd
	workspace string
}

var _ lang.Data = (*evalData)(nil)

func (d *evalData) StaticValidateReferences(_ context.Context, _ []*addrs.Reference, _ addrs.Referenceable, _ addrs.Referenceable) tfdiags.Diagnostics {
	return nil
}

func (d *evalData) GetCountAttr(_ context.Context, addr addrs.CountAttr, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	switch addr.Name {
	case "index":
		if d.repData.CountIndex == cty.NilVal {
			// No repetition data — this vertex was expanded as a singleton
			// fallback because count depended on a runtime value. Return
			// unknown so expressions propagate rather than cascade errors.
			return cty.UnknownVal(cty.Number), nil
		}
		return d.repData.CountIndex, nil
	default:
		return cty.DynamicVal, evalErr(fmt.Sprintf("unsupported count attribute %q", addr.Name))
	}
}

func (d *evalData) GetForEachAttr(_ context.Context, addr addrs.ForEachAttr, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	switch addr.Name {
	case "key":
		if d.repData.EachKey == cty.NilVal {
			// No repetition data — singleton fallback. Return unknown so
			// the HCL type system propagates unknowns through attribute
			// access rather than erroring.
			return cty.UnknownVal(cty.String), nil
		}
		return d.repData.EachKey, nil
	case "value":
		if d.repData.EachValue == cty.NilVal {
			return cty.DynamicVal, nil
		}
		return d.repData.EachValue, nil
	default:
		return cty.DynamicVal, evalErr(fmt.Sprintf("unsupported each attribute %q", addr.Name))
	}
}

func (d *evalData) GetInputVariable(_ context.Context, addr addrs.InputVariable, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	key := addrKey{unique.Make(d.module.String()), unique.Make(addr.Name)}
	id, ok := d.graph.Vars[key]
	if !ok {
		return cty.DynamicVal, evalErr(fmt.Sprintf("reference to undeclared input variable %q", addr.Name))
	}
	val := d.results.Values[id]
	if val == cty.NilVal {
		return cty.DynamicVal, nil
	}
	if cfg := d.graph.Verts[id].VariableCfg; cfg != nil && cfg.Sensitive {
		val = val.WithMarks(markSensitive)
	}
	return val, nil
}

func (d *evalData) GetLocalValue(_ context.Context, addr addrs.LocalValue, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	key := addrKey{unique.Make(d.module.String()), unique.Make(addr.Name)}

	// Check inlined locals first — these were compacted out of the graph
	// because their values were wholly known at expansion time.
	if val, ok := d.graph.InlinedLocals[key]; ok {
		return val, nil
	}

	id, ok := d.graph.Locals[key]
	if !ok {
		return cty.DynamicVal, evalErr(fmt.Sprintf("reference to undeclared local value %q", addr.Name))
	}
	val := d.results.Values[id]
	if val == cty.NilVal {
		return cty.DynamicVal, nil
	}
	return val, nil
}

func (d *evalData) GetOutput(_ context.Context, addr addrs.OutputValue, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	key := addrKey{unique.Make(d.module.String()), unique.Make(addr.Name)}
	id, ok := d.graph.Outputs[key]
	if !ok {
		return cty.DynamicVal, evalErr(fmt.Sprintf("reference to undeclared output value %q", addr.Name))
	}
	val := d.results.Values[id]
	if val == cty.NilVal {
		return cty.DynamicVal, nil
	}
	return val, nil
}

func (d *evalData) GetResource(_ context.Context, addr addrs.Resource, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	absRes := addr.Absolute(d.module)
	instIDs, ok := d.graph.ResInst[absRes.String()]

	modCfg := d.config.DescendentForInstance(d.module)
	if modCfg == nil {
		return cty.DynamicVal, evalErr(fmt.Sprintf("no configuration for module %s", d.module))
	}
	rc := modCfg.Module.ResourceByAddr(addr)
	if rc == nil {
		return cty.DynamicVal, evalErr(fmt.Sprintf("reference to undeclared resource %s", addr))
	}

	if !ok || len(instIDs) == 0 {
		switch {
		case rc.Count != nil:
			return cty.EmptyTupleVal, nil
		case rc.ForEach != nil:
			return cty.EmptyObjectVal, nil
		default:
			return cty.DynamicVal, nil
		}
	}

	shape := resolveShape(instanceShape(d.graph, instIDs), rc.Count != nil, rc.ForEach != nil)

	switch shape {
	case shapeCount:
		length := 0
		for _, id := range instIDs {
			if ik, ok := d.graph.Verts[id].ResourceAddr.Resource.Key.(addrs.IntKey); ok {
				if int(ik)+1 > length {
					length = int(ik) + 1
				}
			} else if length < 1 {
				length = 1 // NoKey singleton promoted to count
			}
		}
		vals := make([]cty.Value, length)
		for i := range vals {
			vals[i] = cty.DynamicVal
		}
		for _, id := range instIDs {
			v := d.results.Values[id]
			if v == cty.NilVal {
				continue
			}
			if ik, ok := d.graph.Verts[id].ResourceAddr.Resource.Key.(addrs.IntKey); ok {
				vals[int(ik)] = v
			} else {
				vals[0] = v
			}
		}
		return cty.TupleVal(vals), nil

	case shapeForEach:
		vals := make(map[string]cty.Value, len(instIDs))
		for _, id := range instIDs {
			v := d.results.Values[id]
			if v == cty.NilVal {
				continue
			}
			switch sk := d.graph.Verts[id].ResourceAddr.Resource.Key.(type) {
			case addrs.StringKey:
				vals[string(sk)] = v
			default:
				vals[""] = v // NoKey singleton → empty string key
			}
		}
		if len(vals) == 0 {
			return cty.EmptyObjectVal, nil
		}
		return cty.ObjectVal(vals), nil

	default: // shapeSingleton
		if v := d.results.Values[instIDs[0]]; v != cty.NilVal {
			return v, nil
		}
		return cty.DynamicVal, nil
	}
}

func (d *evalData) GetModule(_ context.Context, addr addrs.ModuleCall, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	childPath := d.module.Module().Child(addr.Name)
	childCfg := d.config.Descendent(childPath)
	if childCfg == nil {
		return cty.DynamicVal, evalErr(fmt.Sprintf("reference to undeclared module %q", addr.Name))
	}

	parentCfg := d.config.DescendentForInstance(d.module)
	if parentCfg == nil {
		return cty.DynamicVal, evalErr(fmt.Sprintf("no configuration for module %s", d.module))
	}
	callCfg := parentCfg.Module.ModuleCalls[addr.Name]

	outputNames := make([]string, 0, len(childCfg.Module.Outputs))
	for name := range childCfg.Module.Outputs {
		outputNames = append(outputNames, name)
	}

	var instances []instOutputs
	keyIndex := make(map[addrs.InstanceKey]int) // instance key → index into instances
	for _, outName := range outputNames {
		for _, id := range d.collectChildOutputs(d.module, addr.Name, outName) {
			vert := &d.graph.Verts[id]
			_, instStep := vert.Module.CallInstance()
			val := d.results.Values[id]
			if val == cty.NilVal {
				val = cty.DynamicVal
			}
			if idx, ok := keyIndex[instStep.Key]; ok {
				instances[idx].vals[outName] = val
			} else {
				keyIndex[instStep.Key] = len(instances)
				instances = append(instances, instOutputs{
					key:  instStep.Key,
					vals: map[string]cty.Value{outName: val},
				})
			}
		}
	}

	if len(instances) == 0 {
		switch {
		case callCfg != nil && callCfg.Count != nil:
			return cty.EmptyTupleVal, nil
		case callCfg != nil && callCfg.ForEach != nil:
			return cty.EmptyObjectVal, nil
		default:
			return cty.EmptyObjectVal, nil
		}
	}

	shape := resolveShape(moduleInstanceShape(instances),
		callCfg != nil && callCfg.Count != nil,
		callCfg != nil && callCfg.ForEach != nil)

	switch shape {
	case shapeCount:
		length := 0
		for _, inst := range instances {
			if ik, ok := inst.key.(addrs.IntKey); ok {
				if int(ik)+1 > length {
					length = int(ik) + 1
				}
			} else if length < 1 {
				length = 1
			}
		}
		vals := make([]cty.Value, length)
		for i := range vals {
			vals[i] = cty.EmptyObjectVal
		}
		for _, inst := range instances {
			if ik, ok := inst.key.(addrs.IntKey); ok {
				vals[int(ik)] = cty.ObjectVal(inst.vals)
			} else {
				vals[0] = cty.ObjectVal(inst.vals)
			}
		}
		return cty.TupleVal(vals), nil

	case shapeForEach:
		vals := make(map[string]cty.Value, len(instances))
		for _, inst := range instances {
			switch sk := inst.key.(type) {
			case addrs.StringKey:
				vals[string(sk)] = cty.ObjectVal(inst.vals)
			default:
				vals[""] = cty.ObjectVal(inst.vals)
			}
		}
		if len(vals) == 0 {
			return cty.EmptyObjectVal, nil
		}
		return cty.ObjectVal(vals), nil

	default: // shapeSingleton
		return cty.ObjectVal(instances[0].vals), nil
	}
}

// collectChildOutputs returns output vertex IDs for a specific output name
// across all instances of the named child module call. Uses the pre-built
// ChildOutputs index — O(children) instead of O(all outputs).
func (d *evalData) collectChildOutputs(parent addrs.ModuleInstance, callName string, outputName string) []ID {
	all := d.graph.ChildOutputs[addrKey{unique.Make(parent.String()), unique.Make(callName)}]
	if len(all) == 0 {
		return nil
	}
	result := make([]ID, 0, len(all)/4)
	for _, id := range all {
		if d.graph.Verts[id].Name == outputName {
			result = append(result, id)
		}
	}
	return result
}

func (d *evalData) GetPathAttr(_ context.Context, addr addrs.PathAttr, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	switch addr.Name {
	case "cwd":
		return cty.StringVal(d.workDir), nil
	case "root":
		return cty.StringVal(d.rootDir), nil
	case "module":
		if modCfg := d.config.DescendentForInstance(d.module); modCfg != nil {
			return cty.StringVal(modCfg.Module.SourceDir), nil
		}
		return cty.StringVal(d.rootDir), nil
	default:
		return cty.DynamicVal, evalErr(fmt.Sprintf("unsupported path attribute %q", addr.Name))
	}
}

func (d *evalData) GetTerraformAttr(_ context.Context, addr addrs.TerraformAttr, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	switch addr.Name {
	case "workspace":
		return cty.StringVal(d.workspace), nil
	case "applying":
		return cty.False, nil
	case "env":
		return cty.StringVal(d.workspace), nil
	default:
		return cty.DynamicVal, evalErr(fmt.Sprintf("unsupported terraform attribute %q", addr.Name))
	}
}

func (d *evalData) GetCheckBlock(_ context.Context, _ addrs.Check, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return cty.DynamicVal, evalErr("check blocks are not supported in build mode")
}

// stubProviderFunction returns a placeholder function that accepts any
// arguments and returns cty.DynamicVal. This lets expressions that call
// provider functions (e.g. provider::oci::parse) propagate unknowns
// through the graph instead of producing errors that cascade to every
// dependent vertex.
func stubProviderFunction(_ context.Context, fn addrs.ProviderFunction, rng tfdiags.SourceRange) (*function.Function, tfdiags.Diagnostics) {
	f := function.New(&function.Spec{
		VarParam: &function.Parameter{
			Name: "args",
			Type: cty.DynamicPseudoType,
		},
		Type: function.StaticReturnType(cty.DynamicPseudoType),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			return cty.DynamicVal, nil
		},
	})
	return &f, nil
}

// markSensitive is the value mark for sensitive data.
var markSensitive = cty.NewValueMarks("sensitive")

// evalErr creates a sourceless error diagnostic.
func evalErr(summary string) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	return diags.Append(tfdiags.Sourceless(tfdiags.Error, summary, ""))
}
