// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package providers

import "time"

// ResourceRole describes the role a resource type plays in a build pipeline.
// This is used by the build execution mode for filtering (--skip/--only) and
// for UI presentation, but does not affect execution semantics.
type ResourceRole string

const (
	// RoleBuild indicates a resource that produces build artifacts (images, binaries, etc.)
	RoleBuild ResourceRole = "build"

	// RoleTest indicates a resource that validates build outputs.
	RoleTest ResourceRole = "test"

	// RolePublish indicates a resource that publishes or tags artifacts for distribution.
	RolePublish ResourceRole = "publish"

	// RoleDefault is the default role for resources that don't declare one.
	RoleDefault ResourceRole = ""
)

// CachePolicy controls how the build execution mode uses content-addressed
// caching for a resource type.
type CachePolicy struct {
	// Mode determines the caching strategy.
	Mode CacheMode

	// TTL is the maximum age of a cached result before it is considered stale.
	// Only used when Mode is CacheWithTTL.
	TTL time.Duration
}

// CacheMode enumerates the caching strategies for resource types.
type CacheMode int

const (
	// CacheByInputs caches resource outputs keyed by the content hash of their
	// evaluated inputs. Same inputs → cached outputs are reused without calling
	// the provider. Appropriate for deterministic operations like image builds.
	CacheByInputs CacheMode = iota

	// CacheWithTTL is like CacheByInputs but the cached result expires after
	// the configured TTL duration. Appropriate for operations that should be
	// periodically re-validated (e.g., integration tests).
	CacheWithTTL

	// NeverCache disables caching for this resource type. The provider is
	// always called regardless of whether inputs have changed. Appropriate for
	// idempotent side-effectful operations like tagging.
	NeverCache
)

// ResourceMeta describes build-system metadata for a single resource type.
type ResourceMeta struct {
	Role        ResourceRole
	CachePolicy CachePolicy
}

// BuildMetaProvider is an optional interface that providers can implement to
// declare build-system metadata for their resource types. Providers that don't
// implement this interface get default metadata (RoleDefault, CacheByInputs)
// for all resource types.
type BuildMetaProvider interface {
	// GetResourceMeta returns build metadata for each resource type the
	// provider manages. Resource types not present in the returned map
	// use the default metadata.
	GetResourceMeta() map[string]ResourceMeta
}

// DefaultResourceMeta returns the default metadata for a resource type that
// has no explicit metadata declared.
func DefaultResourceMeta() ResourceMeta {
	return ResourceMeta{
		Role:        RoleDefault,
		CachePolicy: CachePolicy{Mode: CacheByInputs},
	}
}

// GetResourceMetaFromProvider returns the ResourceMeta for a given resource
// type from a provider. If the provider does not implement BuildMetaProvider,
// or does not declare metadata for the given type, the default is returned.
func GetResourceMetaFromProvider(p Interface, typeName string) ResourceMeta {
	if bmp, ok := p.(BuildMetaProvider); ok {
		if meta, exists := bmp.GetResourceMeta()[typeName]; exists {
			return meta
		}
	}
	return DefaultResourceMeta()
}
