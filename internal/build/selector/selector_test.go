// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package selector

import (
	"testing"

	"github.com/opentofu/opentofu/internal/build/catalog"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		kind      catalog.TargetKind
		recursive bool
		wantErr   bool
	}{
		{name: "global wildcard", raw: "**", recursive: true},
		{name: "module", raw: `module.images`, kind: catalog.TargetKindModule},
		{name: "keyed module", raw: `module.images["amd64"]`, kind: catalog.TargetKindModule},
		{name: "recursive module", raw: `module.images.**`, recursive: true},
		{name: "resource", raw: `oci_image.base`, kind: catalog.TargetKindResource},
		{name: "resource with key", raw: `oci_image.base["release"]`, kind: catalog.TargetKindResource},
		{name: "resource with wildcard key", raw: `oci_image.base[*]`, kind: catalog.TargetKindResource},
		{name: "resource under module", raw: `module.images.oci_image.base`, kind: catalog.TargetKindResource},
		{name: "data source", raw: `data.oci_repository.base`, kind: catalog.TargetKindData},
		{name: "output", raw: `output.digest`, kind: catalog.TargetKindOutput},
		{name: "output under module", raw: `module.images.output.digest`, kind: catalog.TargetKindOutput},
		{name: "run", raw: `run.validate`, kind: catalog.TargetKindRun},
		{name: "nested modules", raw: `module.a.module.b.output.x`, kind: catalog.TargetKindOutput},
		{name: "wildcard type and name", raw: `*.*`, kind: catalog.TargetKindResource},

		{name: "empty", raw: "", wantErr: true},
		{name: "double star in middle", raw: "module.**.output.foo", wantErr: true},
		{name: "module without name", raw: "module", wantErr: true},
		{name: "resource missing name", raw: "oci_image", wantErr: true},
		{name: "output missing name", raw: "module.x.output", wantErr: true},
		{name: "data missing name", raw: "module.x.data.type", wantErr: true},
		{name: "unbalanced bracket", raw: `oci_image.base["x"`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pattern, err := Parse(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if pattern.Raw != tt.raw {
				t.Fatalf("Raw = %q, want %q", pattern.Raw, tt.raw)
			}
			if pattern.Kind != tt.kind {
				t.Fatalf("Kind = %s, want %s", pattern.Kind, tt.kind)
			}
			if pattern.Recursive != tt.recursive {
				t.Fatalf("Recursive = %v, want %v", pattern.Recursive, tt.recursive)
			}
		})
	}
}

func TestParseAll(t *testing.T) {
	tests := []struct {
		name    string
		raw     []string
		wantN   int
		wantErr bool
	}{
		{name: "single valid", raw: []string{"**"}, wantN: 1},
		{name: "multiple valid", raw: []string{"**", "output.digest"}, wantN: 2},
		{name: "all invalid", raw: []string{"", "module"}, wantN: 0, wantErr: true},
		{name: "mixed", raw: []string{"**", ""}, wantN: 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, err := ParseAll(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseAll() error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(set) != tt.wantN {
				t.Fatalf("len(set) = %d, want %d", len(set), tt.wantN)
			}
		})
	}
}

var (
	root     = catalog.RootModule()
	images   = root.Child("images", catalog.NoKey())
	imagesK  = root.Child("images", catalog.StringKey("amd64"))
	platform = images.Child("platform", catalog.StringKey("amd64"))
	other    = root.Child("other", catalog.NoKey())
)

func TestPatternMatch(t *testing.T) {
	tests := []struct {
		name         string
		selector     string
		addr         catalog.Addr
		wantExact    bool
		wantDecl     bool
		wantPossible bool
	}{
		// ** matches everything
		{
			name:     "global matches resource",
			selector: "**", addr: catalog.ResourceAddr(imagesK, catalog.TargetKindResource, "oci_image", "base", catalog.StringKey("release")),
			wantExact: true, wantDecl: true, wantPossible: true,
		},
		{
			name:     "global matches root module",
			selector: "**", addr: catalog.ModuleAddr(root),
			wantExact: true, wantDecl: true, wantPossible: true,
		},

		// Recursive module patterns
		{
			name:     "recursive matches descendant resource",
			selector: "module.images.**", addr: catalog.ResourceAddr(platform, catalog.TargetKindResource, "oci_image", "base", catalog.NoKey()),
			wantExact: true, wantDecl: true, wantPossible: true,
		},
		{
			name:     "recursive matches child module",
			selector: "module.images.**", addr: catalog.ModuleAddr(platform),
			wantExact: true, wantDecl: true, wantPossible: true,
		},
		{
			name:     "recursive matches module itself",
			selector: "module.images.**", addr: catalog.ModuleAddr(images),
			wantExact: true, wantDecl: true, wantPossible: true,
		},
		{
			name:     "recursive rejects unrelated subtree",
			selector: "module.images.**", addr: catalog.ModuleAddr(other),
			wantExact: false, wantDecl: false, wantPossible: false,
		},

		// Module patterns (exact vs declaration)
		{
			name:     "keyed module exact match",
			selector: `module.images["amd64"]`, addr: catalog.ModuleAddr(imagesK),
			wantExact: true, wantDecl: true, wantPossible: true,
		},
		{
			name:     "keyed module declaration match",
			selector: `module.images["amd64"]`, addr: catalog.ModuleAddr(images),
			wantExact: false, wantDecl: true, wantPossible: true,
		},
		{
			name:     "keyed module wrong key",
			selector: `module.images["arm64"]`, addr: catalog.ModuleAddr(imagesK),
			wantExact: false, wantDecl: true, wantPossible: true,
		},
		{
			name:     "module rejects wrong name",
			selector: `module.images`, addr: catalog.ModuleAddr(other),
			wantExact: false, wantDecl: false, wantPossible: false,
		},

		// Subtree matching via module addrs
		{
			name:     "resource pattern yields subtree for parent module",
			selector: "module.images.output.digest", addr: catalog.ModuleAddr(root),
			wantExact: false, wantDecl: false, wantPossible: true,
		},
		{
			name:     "resource pattern yields subtree for matching module",
			selector: "module.images.output.digest", addr: catalog.ModuleAddr(images),
			wantExact: false, wantDecl: false, wantPossible: true,
		},
		{
			name:     "resource pattern prunes deeper non-recursive module",
			selector: "module.images.output.digest", addr: catalog.ModuleAddr(platform),
			wantExact: false, wantDecl: false, wantPossible: false,
		},

		// Output patterns
		{
			name:     "output exact match",
			selector: "output.digest", addr: catalog.OutputAddr(root, "digest"),
			wantExact: true, wantDecl: true, wantPossible: true,
		},
		{
			name:     "output wrong name",
			selector: "output.digest", addr: catalog.OutputAddr(root, "other"),
			wantExact: false, wantDecl: false, wantPossible: false,
		},
		{
			name:     "output under keyed module exact",
			selector: `module.images["amd64"].output.digest`, addr: catalog.OutputAddr(imagesK, "digest"),
			wantExact: true, wantDecl: true, wantPossible: true,
		},
		{
			name:     "output under keyed module declaration",
			selector: `module.images["amd64"].output.digest`, addr: catalog.OutputAddr(images, "digest"),
			wantExact: false, wantDecl: true, wantPossible: true,
		},

		// Resource patterns with keys
		{
			name:      "resource exact match with key",
			selector:  `module.images["amd64"].oci_image.base["release"]`,
			addr:      catalog.ResourceAddr(imagesK, catalog.TargetKindResource, "oci_image", "base", catalog.StringKey("release")),
			wantExact: true, wantDecl: true, wantPossible: true,
		},
		{
			name:      "resource declaration match ignores keys",
			selector:  `module.images["amd64"].oci_image.base["release"]`,
			addr:      catalog.ResourceAddr(images, catalog.TargetKindResource, "oci_image", "base", catalog.NoKey()),
			wantExact: false, wantDecl: true, wantPossible: true,
		},
		{
			name:     "resource wrong type",
			selector: `oci_image.base`, addr: catalog.ResourceAddr(root, catalog.TargetKindResource, "null_resource", "base", catalog.NoKey()),
			wantExact: false, wantDecl: false, wantPossible: false,
		},
		{
			name:     "resource wildcard key matches any key",
			selector: `oci_image.base[*]`, addr: catalog.ResourceAddr(root, catalog.TargetKindResource, "oci_image", "base", catalog.StringKey("anything")),
			wantExact: true, wantDecl: true, wantPossible: true,
		},

		// Data source patterns
		{
			name:     "data source match",
			selector: "data.oci_repository.base", addr: catalog.ResourceAddr(root, catalog.TargetKindData, "oci_repository", "base", catalog.NoKey()),
			wantExact: true, wantDecl: true, wantPossible: true,
		},

		// Cross-kind mismatch
		{
			name:     "output pattern does not match resource",
			selector: "output.digest", addr: catalog.ResourceAddr(root, catalog.TargetKindResource, "oci_image", "digest", catalog.NoKey()),
			wantExact: false, wantDecl: false, wantPossible: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pattern, err := Parse(tt.selector)
			if err != nil {
				t.Fatalf("Parse(%q): %s", tt.selector, err)
			}
			m := pattern.Match(tt.addr)
			if m.Exact() != tt.wantExact {
				t.Errorf("Exact() = %v, want %v", m.Exact(), tt.wantExact)
			}
			if m.Declaration() != tt.wantDecl {
				t.Errorf("Declaration() = %v, want %v", m.Declaration(), tt.wantDecl)
			}
			if m.Possible() != tt.wantPossible {
				t.Errorf("Possible() = %v, want %v", m.Possible(), tt.wantPossible)
			}
		})
	}
}

func TestSetMatch(t *testing.T) {
	tests := []struct {
		name         string
		selectors    []string
		addr         catalog.Addr
		wantExact    bool
		wantDecl     bool
		wantPossible bool
	}{
		{
			name:      "best match wins across patterns",
			selectors: []string{"module.images", `module.images["amd64"]`},
			addr:      catalog.ModuleAddr(imagesK),
			wantExact: true, wantDecl: true, wantPossible: true,
		},
		{
			name:      "subtree from one pattern declaration from another",
			selectors: []string{"module.images.output.digest", "module.images"},
			addr:      catalog.ModuleAddr(images),
			wantExact: true, wantDecl: true, wantPossible: true,
		},
		{
			name:      "no match across all patterns",
			selectors: []string{"module.images.**", "output.digest"},
			addr:      catalog.ResourceAddr(other, catalog.TargetKindResource, "null_resource", "x", catalog.NoKey()),
			wantExact: false, wantDecl: false, wantPossible: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, err := ParseAll(tt.selectors)
			if err != nil {
				t.Fatalf("ParseAll: %s", err)
			}
			m := set.Match(tt.addr)
			if m.Exact() != tt.wantExact {
				t.Errorf("Exact() = %v, want %v", m.Exact(), tt.wantExact)
			}
			if m.Declaration() != tt.wantDecl {
				t.Errorf("Declaration() = %v, want %v", m.Declaration(), tt.wantDecl)
			}
			if m.Possible() != tt.wantPossible {
				t.Errorf("Possible() = %v, want %v", m.Possible(), tt.wantPossible)
			}
		})
	}
}
