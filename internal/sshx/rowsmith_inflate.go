// Rowsmith: a zlib decoder that takes one SSH packet at a time.
//
// compress/flate can't be used: when its input runs out it stops for good,
// and it hands out what it decoded only at a sync flush, a full window or the
// end of the stream. OpenSSH ends each packet with a partial flush instead,
// whose last few bits arrive with the next packet. This decoder works in
// steps (a header, a symbol, a match) and, when a step runs out of bits,
// rewinds to the step's start and waits for the next packet. It follows
// RFC 1950 and RFC 1951, and the reference decoder puff.c by Mark Adler.

package ssh

import (
	"errors"
	"fmt"
	"math/bits"
)

const (
	maxCodeBits = 15
	fastBits    = 9 // codes this short are decoded by table lookup
	windowSize  = 32 << 10
)

var (
	lengthBase  = [29]uint16{3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31, 35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258}
	lengthExtra = [29]uint8{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}
	distBase    = [30]uint16{1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193, 257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577}
	distExtra   = [30]uint8{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}
	// The order code length code lengths are sent in (RFC 1951, section 3.2.7).
	clOrder = [19]uint8{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

	fixedLit, fixedDist huffman
)

func init() {
	var l [288]uint8
	for i := range l {
		switch {
		case i < 144:
			l[i] = 8
		case i < 256:
			l[i] = 9
		case i < 280:
			l[i] = 7
		default:
			l[i] = 8
		}
	}
	fixedLit.build(l[:])
	var d [30]uint8
	for i := range d {
		d[i] = 5
	}
	fixedDist.build(d[:])
}

func inflateError(msg string) error { return fmt.Errorf("ssh: corrupt compressed data: %s", msg) }

// huffman is a canonical Huffman code: how many codes there are of each
// length, and the symbols ordered by code. fast maps the next fastBits input
// bits to the symbol<<4 | length of a code that short, or 0.
type huffman struct {
	count  [maxCodeBits + 1]uint16
	symbol []uint16
	fast   [1 << fastBits]uint16
}

// build makes the code for the given code lengths, returning how many codes
// of the longest possible length are left unused: 0 for a complete code, more
// for an incomplete one, and less than 0 when the lengths are over-subscribed.
func (h *huffman) build(lengths []uint8) int {
	h.count = [maxCodeBits + 1]uint16{}
	h.fast = [1 << fastBits]uint16{}
	for _, l := range lengths {
		h.count[l]++
	}
	h.symbol = make([]uint16, len(lengths))
	if int(h.count[0]) == len(lengths) {
		return 0 // no codes: complete, but decoding with it fails
	}
	left := 1
	for l := 1; l <= maxCodeBits; l++ {
		left = left<<1 - int(h.count[l])
		if left < 0 {
			return left
		}
	}
	var offs [maxCodeBits + 1]uint16
	for l := 1; l < maxCodeBits; l++ {
		offs[l+1] = offs[l] + h.count[l]
	}
	for sym, l := range lengths {
		if l != 0 {
			h.symbol[offs[l]] = uint16(sym)
			offs[l]++
		}
	}
	code, idx := 0, 0
	for l := 1; l <= fastBits; l++ {
		for range h.count[l] {
			// Input bits come lowest first, so the code appears reversed.
			r := int(bits.Reverse16(uint16(code)) >> (16 - l))
			for j := r; j < len(h.fast); j += 1 << l {
				h.fast[j] = h.symbol[idx]<<4 | uint16(l)
			}
			code++
			idx++
		}
		code <<= 1
	}
	return left
}

// oneBit reports whether a code for n symbols is a single code of length 1.
func (h *huffman) oneBit(n int) bool { return h.count[1] == 1 && int(h.count[0]) == n-1 }

// inflater decodes one direction's zlib stream. The zero value is ready.
type inflater struct {
	in    []byte // input not yet consumed, from pos on
	pos   int
	bits  uint64 // bits taken from in but not used yet, lowest first
	nbits uint

	win    []byte // the output, of which the last 32 KiB stay between packets
	err    error
	header bool // the zlib header was read
	final  bool // the last block was read
	block  byte // the block being read: 0 none, 1 stored, 2 Huffman
	stored int  // bytes left in a stored block
	lit    *huffman
	dist   *huffman
	dyn    [2]huffman // a dynamic block's codes
}

// errMore means a step needs more input than this packet holds.
var errMore = errors.New("more input needed")

// inflate decodes one packet, returning all the output it completes.
func (z *inflater) inflate(p []byte) ([]byte, error) {
	if z.err != nil {
		return nil, z.err
	}
	z.in = append(z.in[z.pos:], p...)
	z.pos = 0
	start := len(z.win)
	for {
		pos, bits, nbits := z.pos, z.bits, z.nbits
		err := z.step()
		if err == errMore {
			z.pos, z.bits, z.nbits = pos, bits, nbits
			break
		}
		if err == nil && len(z.win)-start > maxPacket {
			err = inflateError("a packet expands beyond the packet size limit")
		}
		if err != nil {
			z.err = err
			return nil, err
		}
	}
	out := append([]byte(nil), z.win[start:]...)
	if len(z.win) > windowSize {
		n := copy(z.win, z.win[len(z.win)-windowSize:])
		z.win = z.win[:n]
	}
	return out, nil
}

// need loads at least n bits (at most 32), or returns false.
func (z *inflater) need(n uint) bool {
	for z.nbits < n {
		if z.pos == len(z.in) {
			return false
		}
		z.bits |= uint64(z.in[z.pos]) << z.nbits
		z.pos++
		z.nbits += 8
	}
	return true
}

// take returns the next n bits, which need has loaded.
func (z *inflater) take(n uint) int {
	v := int(z.bits & (1<<n - 1))
	z.bits >>= n
	z.nbits -= n
	return v
}

func (z *inflater) read(n uint) (int, error) {
	if !z.need(n) {
		return 0, errMore
	}
	return z.take(n), nil
}

// decode reads one symbol: a short code by table, a longer one a bit at a
// time (Huffman codes are packed starting with their most significant bit).
func (z *inflater) decode(h *huffman) (int, error) {
	z.need(fastBits) // as many as there are; bits past the input read as 0
	if e := h.fast[z.bits&(1<<fastBits-1)]; e != 0 && uint(e&15) <= z.nbits {
		z.take(uint(e & 15))
		return int(e >> 4), nil
	}
	code, first, index := 0, 0, 0
	for l := 1; l <= maxCodeBits; l++ {
		if !z.need(1) {
			return 0, errMore
		}
		code |= z.take(1)
		count := int(h.count[l])
		if code-count < first {
			return int(h.symbol[index+code-first]), nil
		}
		index += count
		first = (first + count) << 1
		code <<= 1
	}
	return 0, inflateError("invalid Huffman code")
}

// step decodes the next header, symbol or run of stored bytes. On errMore
// the caller rewinds the input, so a step adds output only once it has all
// its bits.
func (z *inflater) step() error {
	switch {
	case !z.header:
		if !z.need(16) {
			return errMore
		}
		cmf, flg := z.take(8), z.take(8)
		if cmf&0x0f != 8 || cmf>>4 > 7 || (cmf<<8|flg)%31 != 0 || flg&0x20 != 0 {
			return inflateError("bad zlib header")
		}
		z.header = true
	case z.block == 0 && z.final:
		if z.pos < len(z.in) {
			return inflateError("data after the last block")
		}
		return errMore
	case z.block == 0:
		if !z.need(3) {
			return errMore
		}
		final, kind := z.take(1) == 1, z.take(2)
		switch kind {
		case 0:
			z.take(z.nbits % 8) // to a byte boundary
			n, err := z.read(16)
			if err != nil {
				return err
			}
			nn, err := z.read(16)
			if err != nil {
				return err
			}
			if n != ^nn&0xffff {
				return inflateError("stored block length mismatch")
			}
			z.block, z.stored = 1, n
		case 1:
			z.block, z.lit, z.dist = 2, &fixedLit, &fixedDist
		case 2:
			if err := z.dynamic(); err != nil {
				return err
			}
			z.block, z.lit, z.dist = 2, &z.dyn[0], &z.dyn[1]
		default:
			return inflateError("invalid block type")
		}
		z.final = final
	case z.block == 1:
		if z.stored == 0 {
			z.block = 0
			return nil
		}
		n := 0
		for ; n < z.stored && z.nbits >= 8; n++ {
			z.win = append(z.win, byte(z.take(8)))
		}
		m := min(z.stored-n, len(z.in)-z.pos)
		z.win = append(z.win, z.in[z.pos:z.pos+m]...)
		z.pos += m
		n += m
		if n == 0 {
			return errMore
		}
		z.stored -= n
	default:
		sym, err := z.decode(z.lit)
		if err != nil {
			return err
		}
		switch {
		case sym < 256:
			z.win = append(z.win, byte(sym))
		case sym == 256:
			z.block = 0
		default:
			return z.match(sym - 257)
		}
	}
	return nil
}

// match copies a length and distance pair's bytes from earlier output.
func (z *inflater) match(sym int) error {
	if sym >= len(lengthBase) {
		return inflateError("invalid length symbol")
	}
	extra, err := z.read(uint(lengthExtra[sym]))
	if err != nil {
		return err
	}
	length := int(lengthBase[sym]) + extra
	dsym, err := z.decode(z.dist)
	if err != nil {
		return err
	}
	if dsym >= len(distBase) {
		return inflateError("invalid distance symbol")
	}
	extra, err = z.read(uint(distExtra[dsym]))
	if err != nil {
		return err
	}
	dist := int(distBase[dsym]) + extra
	if dist > len(z.win) {
		return inflateError("distance too far back")
	}
	// A copy longer than its distance repeats its own output, dist bytes at a time.
	for length > 0 {
		n := min(length, dist)
		from := len(z.win) - dist
		z.win = append(z.win, z.win[from:from+n]...)
		length -= n
	}
	return nil
}

// dynamic reads a dynamic block's code descriptions (RFC 1951, section 3.2.7).
func (z *inflater) dynamic() error {
	if !z.need(14) {
		return errMore
	}
	nlen, ndist, ncode := z.take(5)+257, z.take(5)+1, z.take(4)+4
	if nlen > 286 || ndist > 30 {
		return inflateError("too many length or distance symbols")
	}
	var lengths [286 + 30]uint8
	for i := range ncode {
		v, err := z.read(3)
		if err != nil {
			return err
		}
		lengths[clOrder[i]] = uint8(v)
	}
	var cl huffman
	if cl.build(lengths[:19]) != 0 {
		return inflateError("incomplete code length code")
	}
	lengths = [286 + 30]uint8{}
	for i := 0; i < nlen+ndist; {
		sym, err := z.decode(&cl)
		if err != nil {
			return err
		}
		if sym < 16 {
			lengths[i] = uint8(sym)
			i++
			continue
		}
		var v uint8
		var n int
		switch sym {
		case 16:
			if i == 0 {
				return inflateError("repeat with no first length")
			}
			v = lengths[i-1]
			n, err = z.read(2)
			n += 3
		case 17:
			n, err = z.read(3)
			n += 3
		default:
			n, err = z.read(7)
			n += 11
		}
		if err != nil {
			return err
		}
		if i+n > nlen+ndist {
			return inflateError("too many code lengths")
		}
		for ; n > 0; n-- {
			lengths[i] = v
			i++
		}
	}
	if lengths[256] == 0 {
		return inflateError("no end-of-block code")
	}
	// An incomplete code is allowed only when it is a single code of length 1.
	if left := z.dyn[0].build(lengths[:nlen]); left < 0 || left > 0 && !z.dyn[0].oneBit(nlen) {
		return inflateError("bad literal/length code")
	}
	if left := z.dyn[1].build(lengths[nlen : nlen+ndist]); left < 0 || left > 0 && !z.dyn[1].oneBit(ndist) {
		return inflateError("bad distance code")
	}
	return nil
}
