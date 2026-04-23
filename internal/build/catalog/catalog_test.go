// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package catalog

import "testing"

func TestAddrString(t *testing.T) {
	module := RootModule().Child("images", StringKey("amd64"))

	tests := map[string]struct {
		addr Addr
		want string
	}{
		"module": {
			addr: ModuleAddr(module),
			want: `module.images["amd64"]`,
		},
		"resource": {
			addr: ResourceAddr(module, TargetKindResource, "oci_image", "base", NoKey()),
			want: `module.images["amd64"].oci_image.base`,
		},
		"data": {
			addr: ResourceAddr(module, TargetKindData, "oci_repository", "base", IntKey(1)),
			want: `module.images["amd64"].data.oci_repository.base[1]`,
		},
		"output": {
			addr: OutputAddr(module, "digest"),
			want: `module.images["amd64"].output.digest`,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := test.addr.String(); got != test.want {
				t.Fatalf("wrong string\ngot:  %s\nwant: %s", got, test.want)
			}
		})
	}
}

func TestAddrIdentity(t *testing.T) {
	addr := OutputAddr(RootModule().Child("images", StringKey("amd64")), "digest")

	index := map[AddrKey]Addr{
		addr.Identity(): addr,
	}

	got, ok := index[addr.Identity()]
	if !ok {
		t.Fatal("missing address identity")
	}
	if got.String() != `module.images["amd64"].output.digest` {
		t.Fatalf("wrong address: %s", got.String())
	}
}
