// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package digest

import "testing"

func TestFromStringsStable(t *testing.T) {
	a := FromStrings("oci", "linux/amd64", "busybox")
	b := FromStrings("oci", "linux/amd64", "busybox")
	c := FromStrings("oci", "linux/arm64", "busybox")

	if a != b {
		t.Fatalf("expected stable digest, got %q and %q", a, b)
	}
	if a == c {
		t.Fatalf("expected distinct digests, got %q and %q", a, c)
	}
}

func TestFromValue(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	d, err := FromValue(payload{Name: "busybox"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if d == (Digest{}) {
		t.Fatal("expected non-empty digest")
	}
}
