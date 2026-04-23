// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package run

type Payload struct {
	Request      Request
	ValueAdapter ValueAdapter
	ValueData    []byte
}
