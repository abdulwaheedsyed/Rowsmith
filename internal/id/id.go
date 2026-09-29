// Package id generates sortable, URL-safe identifiers (48-bit millisecond
// timestamp followed by 80 random bits, Crockford base32, lowercase).
package id

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

func New() string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixMilli())<<16)
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err)
	}
	// 128 bits -> 26 base32 characters (130 bits, top 2 bits zero).
	out := make([]byte, 26)
	var acc uint64
	var bits uint
	j := 0
	// Prepend two zero bits so the output is exactly 26 chars.
	bits = 2
	for _, c := range b {
		acc = acc<<8 | uint64(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[j] = alphabet[(acc>>bits)&31]
			j++
		}
	}
	return string(out[:j])
}

// Token returns n random bytes; callers encode as needed.
func Token(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
