// The stored shape of an IAVL node: the 12-byte key it lives under and the
// value iavl writes, mirrored here so damaged records can be read raw.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

const (
	rootPrefix = "s/k:" // every store lives under rootPrefix + name + "/"
	nodeTag    = 's'    // then nodes under one more byte, then 12 bytes of key
	nodeKeyLen = 12
)

// nodeKey identifies one IAVL node: the version that created it and its
// sequence within that version.
type nodeKey struct {
	version int64
	nonce   int32
}

func (nk nodeKey) String() string { return fmt.Sprintf("(v%d,n%d)", nk.version, nk.nonce) }

// bytes returns the 12 bytes as stored.
func (nk nodeKey) bytes() [nodeKeyLen]byte {
	var b [nodeKeyLen]byte
	binary.BigEndian.PutUint64(b[:8], uint64(nk.version)) //nolint:gosec // round-trips the stored encoding
	binary.BigEndian.PutUint32(b[8:], uint32(nk.nonce))   //nolint:gosec // round-trips the stored encoding
	return b
}

// find is the command that shows this node's record: `pebble find`, which also
// prints a DEL tombstone, or iavlscan's own lookup on the other backends.
func (nk nodeKey) find() string {
	if dbBackend != backendPebble {
		return fmt.Sprintf("iavlscan -db <db> -nodekey %x%x", nodeTag, nk.bytes())
	}
	return fmt.Sprintf("pebble find <db> hex:<s/k:STORE/>%x%x", nodeTag, nk.bytes())
}

// parseNodeKey accepts the nodeKey as printed in the iavl error, with or
// without the 's' tag and an optional 0x.
func parseNodeKey(s string) (nodeKey, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	b, err := hex.DecodeString(s)
	if err != nil {
		return nodeKey{}, fmt.Errorf("parse nodekey %q: %w", s, err)
	}
	if len(b) == nodeKeyLen+1 && b[0] == nodeTag {
		b = b[1:]
	}
	if len(b) != nodeKeyLen {
		return nodeKey{}, fmt.Errorf("parse nodekey %q: want %d bytes (or one more with the 's' tag), got %d",
			s, nodeKeyLen, len(b))
	}
	return decodeNodeKey(b), nil
}

// decodeNodeKey reads the 12 stored bytes of a node key.
func decodeNodeKey(b []byte) nodeKey {
	return nodeKey{
		version: int64(binary.BigEndian.Uint64(b[:8])), //nolint:gosec // written as 8-byte BE int64
		nonce:   int32(binary.BigEndian.Uint32(b[8:])), //nolint:gosec // written as 4-byte BE int32
	}
}

// nodePrefix is the key prefix under which one store's nodes live.
func nodePrefix(store string) []byte { return []byte(rootPrefix + store + "/" + string(nodeTag)) }

// nodeDBKey is the stored key of one node: its store's prefix then the node key.
func nodeDBKey(store string, nk nodeKey) []byte {
	prefix, b := nodePrefix(store), nk.bytes()
	key := make([]byte, len(prefix)+len(b))
	copy(key, prefix)
	copy(key[len(prefix):], b[:])
	return key
}

// indexBlock is how many packed node keys one block holds. Smaller blocks keep
// tails in cache and search faster, larger ones waste less on the part-filled
// block; 512 is where both curves flatten.
const indexBlock = 1 << 9

// packNodeKey folds a node key into one uint64, preserving the stored order.
// Both fields are written unsigned and big-endian, so this is lossless for any
// version below 2^32 -- far above any real chain height.
func packNodeKey(nk nodeKey) (uint64, bool) {
	if nk.version < 0 || nk.version > math.MaxUint32 || nk.nonce < 0 {
		return 0, false
	}
	return uint64(nk.version)<<32 | uint64(uint32(nk.nonce)), true //nolint:gosec // nonce is checked non-negative
}

func unpackNodeKey(packed uint64) nodeKey {
	return nodeKey{version: int64(packed >> 32), nonce: int32(packed)} //nolint:gosec // inverts packNodeKey
}

// child is one of an inner node's two links, either a node key or, for a node
// carried over from the legacy format, a hash.
type child struct {
	nk     nodeKey
	hash   []byte
	legacy bool
}

func (c child) String() string {
	if c.legacy {
		return fmt.Sprintf("legacy hash %x", c.hash)
	}
	return fmt.Sprintf("%-16v %s", c.nk, c.nk.find())
}

// reference is one outgoing link, named by which side it came from.
type reference struct {
	child
	side string
}

// node is one decoded IAVL record. Leaves carry key and value, inner nodes key,
// hash and two children. Two shapes are not nodes at all: an empty root, and a
// reference root, which points at an earlier version's root.
type node struct {
	height      int64
	size        int64
	key         []byte // the tree key, i.e. the store key this node sits under
	leaf        bool
	value       []byte // leaves only
	hash        []byte // inner only
	left, right child  // inner only
	empty       bool
	ref         bool
	refTo       nodeKey // set when ref
	trailing    int     // bytes left over; non-zero means the decode is suspect
}

// outgoing lists the links this record holds by node key into buf: two children
// for an inner node, one pointer for a reference root, none otherwise. A legacy
// child is addressed by hash and so holds no node key to follow.
func (n node) outgoing(buf []reference) []reference {
	buf = buf[:0]
	switch {
	case n.ref:
		buf = append(buf, reference{child{nk: n.refTo}, "reference to"})
	case !n.leaf && !n.empty:
		if !n.left.legacy {
			buf = append(buf, reference{n.left, "left child"})
		}
		if !n.right.legacy {
			buf = append(buf, reference{n.right, "right child"})
		}
	}
	return buf
}

// decodeNode mirrors iavl.MakeNode: varint height, varint size, bytes key,
// then for a leaf bytes value, and for an inner node bytes hash, varint mode
// and per child either a legacy bytes(hash) or varint version + varint nonce.
// It reports ok=false for anything it cannot parse.
func decodeNode(buf []byte) (node, bool) {
	var n node

	// nodeDB.SaveEmptyRoot writes a zero-length value.
	if len(buf) == 0 {
		n.empty = true
		return n, true
	}
	// nodeDB.isReferenceRoot: a value starting with the node tag points at an
	// earlier version's root, written when nothing changed. It comes in two
	// lengths, the shorter from before lazy pruning.
	if buf[0] == nodeTag {
		switch len(buf) {
		case nodeKeyLen + 1: // 's' + version + nonce
			n.ref, n.refTo = true, decodeNodeKey(buf[1:])
		case 9: // 's' + version, with nonce 1 implied
			n.ref, n.refTo = true, nodeKey{version: int64(binary.BigEndian.Uint64(buf[1:])), nonce: 1} //nolint:gosec // written as 8-byte BE int64
		default:
			return n, false
		}
		return n, true
	}

	height, adv := binary.Varint(buf)
	if adv <= 0 {
		return n, false
	}
	n.height, buf = height, buf[adv:]

	size, adv := binary.Varint(buf)
	if adv <= 0 {
		return n, false
	}
	n.size, buf = size, buf[adv:]

	var ok bool
	if n.key, buf, ok = decodeBytes(buf); !ok {
		return n, false
	}

	if n.height == 0 {
		n.leaf = true
		if n.value, buf, ok = decodeBytes(buf); !ok {
			return n, false
		}
		n.trailing = len(buf)
		return n, true
	}

	if n.hash, buf, ok = decodeBytes(buf); !ok {
		return n, false
	}
	mode, adv := binary.Varint(buf)
	if adv <= 0 || mode < 0 || mode > 3 {
		return n, false
	}
	buf = buf[adv:]

	// mode bit 0/1 mark a child still referenced by its legacy hash.
	if n.left, buf, ok = decodeChild(buf, mode&0x01 != 0); !ok {
		return n, false
	}
	if n.right, buf, ok = decodeChild(buf, mode&0x02 != 0); !ok {
		return n, false
	}
	n.trailing = len(buf)
	return n, true
}

func decodeChild(buf []byte, legacy bool) (child, []byte, bool) {
	if legacy { // a 32-byte hash, carrying no (version, nonce)
		hash, rest, ok := decodeBytes(buf)
		return child{hash: hash, legacy: true}, rest, ok
	}
	version, adv := binary.Varint(buf)
	if adv <= 0 {
		return child{}, nil, false
	}
	buf = buf[adv:]
	nonce, adv := binary.Varint(buf)
	if adv <= 0 {
		return child{}, nil, false
	}
	return child{nk: nodeKey{version: version, nonce: int32(nonce)}}, buf[adv:], true //nolint:gosec // nonce is an int32 in the encoding
}

func decodeBytes(buf []byte) (val, rest []byte, ok bool) {
	l, n := binary.Uvarint(buf)
	if n <= 0 || uint64(len(buf)-n) < l {
		return nil, nil, false
	}
	return buf[n : n+int(l)], buf[n+int(l):], true
}
