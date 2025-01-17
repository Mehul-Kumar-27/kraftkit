// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2017, Ryan Armstrong.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package cpio

import (
	"encoding/binary"
	"hash"
)

type digest struct {
	sum uint32
}

// NewHash returns a new hash.Hash32 for computing SVR4 checksums.
func NewHash() hash.Hash32 {
	return &digest{}
}

func (d *digest) Write(p []byte) (n int, err error) {
	for _, b := range p {
		d.sum += uint32(b & 0xFF)
	}

	return len(p), nil
}

func (d *digest) Sum(b []byte) []byte {
	out := [4]byte{}
	binary.LittleEndian.PutUint32(out[:], d.sum)
	return append(b, out[:]...)
}

func (d *digest) Sum32() uint32 {
	return d.sum
}

func (d *digest) Reset() {
	d.sum = 0
}

func (d *digest) Size() int {
	return 4
}

func (d *digest) BlockSize() int {
	return 1
}
