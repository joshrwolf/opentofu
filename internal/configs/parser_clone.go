// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package configs

// cloneFiles creates independent copies of a []*File slice. The returned
// files share immutable HCL bodies and expressions with the originals,
// but all struct-level fields are independent — mutations by appendFile,
// mergeFile, and NewModule won't affect the source files.
//
// This enables safe reuse of a cached "template" []*File across multiple
// NewModule calls with different StaticModuleCall contexts.
func cloneFiles(files []*File) []*File {
	if len(files) == 0 {
		return files
	}
	out := make([]*File, len(files))
	for i, f := range files {
		out[i] = cloneFile(f)
	}
	return out
}

// cloneFile shallow-copies a File and all pointer targets that appendFile,
// mergeFile, or NewModule could mutate. HCL bodies and expressions (which
// are interface values pointing into the immutable parse tree) are shared.
func cloneFile(f *File) *File {
	c := *f // copies slice headers — elements still point to originals

	c.Backends = cloneSlice(f.Backends)
	c.CloudConfigs = cloneSlice(f.CloudConfigs)
	c.ProviderConfigs = cloneSlice(f.ProviderConfigs)
	c.ProviderMetas = cloneSlice(f.ProviderMetas)
	c.RequiredProviders = cloneSlice(f.RequiredProviders)
	c.Encryptions = cloneSlice(f.Encryptions)
	c.Variables = cloneSlice(f.Variables)
	c.Locals = cloneSlice(f.Locals)
	c.Outputs = cloneSlice(f.Outputs)
	c.ModuleCalls = cloneSlice(f.ModuleCalls)
	c.ManagedResources = cloneResources(f.ManagedResources)
	c.DataResources = cloneResources(f.DataResources)
	c.EphemeralResources = cloneResources(f.EphemeralResources)
	c.Import = cloneSlice(f.Import)
	c.Checks = cloneChecks(f.Checks)
	// Moved and Removed are only appended to Module slices, never mutated.

	return &c
}

// cloneSlice shallow-copies each pointed-to struct in a slice. This is
// sufficient because mutations (appendFile, mergeFile, NewModule) only
// write to direct fields of the struct, not through nested pointers.
func cloneSlice[T any](s []*T) []*T {
	if len(s) == 0 {
		return s
	}
	out := make([]*T, len(s))
	for i, v := range s {
		cp := *v
		out[i] = &cp
	}
	return out
}

// cloneResources copies Resource structs and their ManagedResource
// sub-structs. Resource.merge writes to Managed.Connection,
// Managed.IgnoreChanges, etc., so the nested struct must also be cloned.
func cloneResources(rs []*Resource) []*Resource {
	if len(rs) == 0 {
		return rs
	}
	out := make([]*Resource, len(rs))
	for i, r := range rs {
		rc := *r
		if r.Managed != nil {
			mc := *r.Managed
			rc.Managed = &mc
		}
		out[i] = &rc
	}
	return out
}

// cloneChecks copies Check structs and their DataResource sub-structs.
// appendFile sets DataResource.Provider, so the nested Resource must
// also be cloned.
func cloneChecks(cs []*Check) []*Check {
	if len(cs) == 0 {
		return cs
	}
	out := make([]*Check, len(cs))
	for i, c := range cs {
		cc := *c
		if c.DataResource != nil {
			dr := *c.DataResource
			cc.DataResource = &dr
		}
		out[i] = &cc
	}
	return out
}
