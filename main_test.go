package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/iavl"
	iavldb "github.com/cosmos/iavl/db"
)

// A real inner node read off a running chain: the acc store's root at
// version 1, as `pebble find` printed it.
const accRoot = "0c421501f1829676db577682e944fc3493d451b67ff3e29f20" +
	"2d8f346f5fb182037ba090b1524bad5512a9147c958b096d79649709b0daf9070002040242"

// buildFixture writes two real IAVL stores into one pebble application.db,
// laid out the way rootmulti does, so the tests run against bytes iavl wrote
// rather than bytes this package's decoder produced. Each store is 300 leaves
// under 299 inner nodes, all at version 1, root (v1,n1).
func buildFixture(t *testing.T, dir string, damage ...nodeKey) {
	t.Helper()
	db, err := dbm.NewPebbleDB("application", dir, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	for _, store := range []string{"evm", "bank"} {
		pdb := dbm.NewPrefixDB(db, []byte(rootPrefix+store+"/"))
		tree := iavl.NewMutableTree(iavldb.NewWrapper(pdb), 0, false, iavl.NewNopLogger())
		for i := range 300 {
			if _, err := tree.Set([]byte(fmt.Sprintf("%s/key%04d", store, i)), []byte{byte(i)}); err != nil {
				t.Fatalf("set: %v", err)
			}
		}
		if _, _, err := tree.SaveVersion(); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	// Remove evm nodes the tree still references, the way a bad prune would.
	for _, nk := range damage {
		if err := db.DeleteSync(append(nodePrefix("evm"), nk.bytes()...)); err != nil {
			t.Fatalf("damage %v: %v", nk, err)
		}
	}
}

// buildVersioned writes one evm store that grew over many versions, the way
// a real chain's does: each version updates `updates` keys out of `keyspace`.
func buildVersioned(t testing.TB, dir string, versions, updates, keyspace int, damage ...nodeKey) {
	t.Helper()
	db, err := dbm.NewPebbleDB("application", dir, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	pdb := dbm.NewPrefixDB(db, []byte(rootPrefix+"evm/"))
	tree := iavl.NewMutableTree(iavldb.NewWrapper(pdb), 0, false, iavl.NewNopLogger())
	for v := range versions {
		for i := range updates {
			k := fmt.Sprintf("key%06d", (v*7919+i*104729)%keyspace)
			if _, err := tree.Set([]byte(k), []byte{byte(v), byte(i)}); err != nil {
				t.Fatalf("set: %v", err)
			}
		}
		if _, _, err := tree.SaveVersion(); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	for _, nk := range damage {
		if err := db.DeleteSync(append(nodePrefix("evm"), nk.bytes()...)); err != nil {
			t.Fatalf("damage %v: %v", nk, err)
		}
	}
}

func openFixture(t testing.TB, dir string) (*pebble.DB, []string) {
	t.Helper()
	db, err := pebble.Open(filepath.Join(dir, "application.db"), &pebble.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("pebble open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	names, err := storeNames(db)
	if err != nil {
		t.Fatal(err)
	}
	return db, names
}

func TestParseNodeKey(t *testing.T) {
	want := nodeKey{version: 16678423, nonce: 242} // 0x00fe7e17, 0xf2
	for _, in := range []string{
		"730000000000fe7e17000000f2",   // as the error prints it, with the 's' tag
		"0000000000fe7e17000000f2",     // without
		"0x730000000000fe7e17000000f2", // pasted from a hex viewer
		"  730000000000fe7e17000000f2 ",
	} {
		got, err := parseNodeKey(in)
		if err != nil {
			t.Fatalf("parseNodeKey(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("parseNodeKey(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"", "zz", "0000", "730000000000fe7e17000000f2ff"} {
		if _, err := parseNodeKey(in); err == nil {
			t.Fatalf("parseNodeKey(%q) succeeded, want error", in)
		}
	}
}

func TestStoreNames(t *testing.T) {
	dir := t.TempDir()
	buildFixture(t, dir)
	_, names := openFixture(t, dir)
	if len(names) != 2 || names[0] != "bank" || names[1] != "evm" {
		t.Fatalf("got %v, want [bank evm]", names)
	}
}

// TestDecodeRebuildsTree checks the decoder against the whole tree iavl wrote:
// every node must be referenced exactly once except the root, and nothing may
// dangle.
func TestDecodeRebuildsTree(t *testing.T) {
	dir := t.TempDir()
	buildFixture(t, dir)
	db, _ := openFixture(t, dir)

	refs := map[nodeKey]int{}
	nodes := map[nodeKey]bool{}
	err := eachNode(db, "evm", nodeKey{}, func(nk nodeKey, val []byte) error {
		nodes[nk] = true
		n, ok := decodeNode(val)
		if !ok {
			t.Fatalf("failed to decode node %v", nk)
		}
		if n.trailing != 0 {
			t.Fatalf("node %v left %d trailing bytes", nk, n.trailing)
		}
		for _, r := range n.outgoing(nil) {
			refs[r.nk]++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(nodes) != 599 {
		t.Fatalf("got %d nodes, want 599", len(nodes))
	}
	if len(refs) != 598 {
		t.Fatalf("got %d referenced nodes, want 598", len(refs))
	}
	for nk, count := range refs {
		if count != 1 {
			t.Fatalf("node %v referenced %d times", nk, count)
		}
		if !nodes[nk] {
			t.Fatalf("dangling reference to %v in a healthy tree", nk)
		}
	}
}

func TestDecodeNodeGolden(t *testing.T) {
	buf, err := hex.DecodeString(accRoot)
	if err != nil {
		t.Fatal(err)
	}
	n, ok := decodeNode(buf)
	if !ok {
		t.Fatal("decodeNode reported failure")
	}
	if n.leaf || n.height != 6 || n.size != 33 {
		t.Fatalf("got leaf=%v height=%d size=%d, want inner/6/33", n.leaf, n.height, n.size)
	}
	// x/auth AddressStoreKeyPrefix (0x01) then a 20-byte address.
	if want := "01f1829676db577682e944fc3493d451b67ff3e29f"; hex.EncodeToString(n.key) != want {
		t.Fatalf("tree key = %x, want %s", n.key, want)
	}
	if n.left.nk != (nodeKey{version: 1, nonce: 2}) || n.right.nk != (nodeKey{version: 1, nonce: 33}) {
		t.Fatalf("children = %v %v, want (v1,n2) (v1,n33)", n.left.nk, n.right.nk)
	}
	if n.left.legacy || n.right.legacy || n.trailing != 0 {
		t.Fatalf("legacy=%v/%v trailing=%d, want none", n.left.legacy, n.right.legacy, n.trailing)
	}
}

// TestDecodeNodeReferenceRoot pins the two shapes that are not nodes. The
// 13-byte case is a real value read from a chain: acc's root at version 8,
// unchanged since version 7 and therefore stored as a pointer.
func TestDecodeNodeReferenceRoot(t *testing.T) {
	for _, raw := range []string{"73000000000000000700000001", "730000000000000007"} {
		buf, err := hex.DecodeString(raw)
		if err != nil {
			t.Fatal(err)
		}
		n, ok := decodeNode(buf)
		if !ok || !n.ref || n.refTo != (nodeKey{version: 7, nonce: 1}) {
			t.Fatalf("%s: got ok=%v ref=%v refTo=%v, want (v7,n1)", raw, ok, n.ref, n.refTo)
		}
		// Decoded as a node instead this would read as height -58 with
		// nonsense children -- the bug this case exists to prevent.
		if n.leaf || n.left.nk != (nodeKey{}) || n.right.nk != (nodeKey{}) {
			t.Fatalf("%s: reference root leaked node fields: %+v", raw, n)
		}
	}

	// Reports must say what a root is rather than show an empty tree key.
	if got := where("evm", node{ref: true, refTo: nodeKey{version: 7, nonce: 1}}); got != "reference root -> (v7,n1)" {
		t.Fatalf("where(reference root) = %q", got)
	}
	if got := where("evm", node{empty: true}); got != "empty root" {
		t.Fatalf("where(empty root) = %q", got)
	}
	if got := where("evm", node{key: []byte("params")}); got != "tree key=706172616d73 (\"params\")" {
		t.Fatalf("where(node) = %q", got)
	}

	// SaveEmptyRoot writes nothing at all.
	if n, ok := decodeNode(nil); !ok || !n.empty || n.ref {
		t.Fatalf("empty value: got ok=%v empty=%v ref=%v", ok, n.empty, n.ref)
	}
	// The tag with any other length is not a shape iavl writes.
	if _, ok := decodeNode([]byte("s12345")); ok {
		t.Fatal("a malformed reference root was accepted")
	}
}

func TestDecodeValue(t *testing.T) {
	// pebble prints the value in brackets; 0x is what a hex viewer adds.
	for _, in := range []string{accRoot, "[" + accRoot + "]", "0x" + accRoot, "  " + accRoot + " "} {
		out := captureStdout(t, func() {
			if err := decodeValue("", in); err != nil {
				t.Fatalf("decodeValue(%q): %v", in, err)
			}
		})
		for _, want := range []string{"height    6", "(inner)", "(v1,n2)", "(v1,n33)"} {
			if !strings.Contains(out, want) {
				t.Fatalf("decodeValue(%q) output missing %q:\n%s", in, want, out)
			}
		}
		if strings.Contains(out, "trailing") {
			t.Fatalf("decodeValue(%q) reported trailing bytes:\n%s", in, out)
		}
	}
	for _, in := range []string{"nothex", "ff"} {
		if err := decodeValue("", in); err == nil {
			t.Fatalf("decodeValue(%q) succeeded, want error", in)
		}
	}
}

// TestDecodeValueLeaf checks the leaf branch against real leaves from the
// fixture rather than a hand-built value.
func TestDecodeValueLeaf(t *testing.T) {
	dir := t.TempDir()
	buildFixture(t, dir)
	db, _ := openFixture(t, dir)

	var leaves int
	err := eachNode(db, "evm", nodeKey{}, func(_ nodeKey, val []byte) error {
		n, ok := decodeNode(val)
		if !ok || !n.leaf {
			return nil
		}
		leaves++
		if n.height != 0 || n.size != 1 || !strings.HasPrefix(string(n.key), "evm/key") {
			t.Fatalf("leaf height=%d size=%d key=%q, want 0/1/evm/key*", n.height, n.size, n.key)
		}
		if leaves > 1 {
			return nil // one printed sample is enough
		}
		out := captureStdout(t, func() {
			if err := decodeValue("", hex.EncodeToString(val)); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, "(leaf)") || !strings.Contains(out, "value  ") || strings.Contains(out, "trailing") {
			t.Fatalf("unexpected leaf output:\n%s", out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if leaves != 300 {
		t.Fatalf("decoded %d leaves, want 300", leaves)
	}
}

func TestFindParents(t *testing.T) {
	dir := t.TempDir()
	buildFixture(t, dir)
	db, names := openFixture(t, dir)

	// Every nonce but the root's is some node's child, once per store.
	out := captureStdout(t, func() {
		if err := findParents(db, names, nodeKey{version: 1, nonce: 7}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"present in bank", "present in evm", "  evm: ", "  bank: ", "2 to (v1,n7), 0 dangling"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	// A root is referenced by nobody.
	out = captureStdout(t, func() {
		if err := findParents(db, names, nodeKey{version: 1, nonce: 1}); err != nil {
			t.Fatal(err)
		}
	})
	if want := "0 to (v1,n1), 0 dangling"; !strings.Contains(out, want) {
		t.Fatalf("output missing %q:\n%s", want, out)
	}
}

func TestPackNodeKey(t *testing.T) {
	// Packing must preserve the stored byte order, which is what lets the
	// audit binary-search the array the iteration produced.
	ordered := []nodeKey{
		{version: 0, nonce: 0},
		{version: 1, nonce: 1},
		{version: 1, nonce: 2},
		{version: 1, nonce: math.MaxInt32},
		{version: 2, nonce: 0},
		{version: 16678423, nonce: 242},
		{version: math.MaxUint32, nonce: math.MaxInt32},
	}
	var prev uint64
	for i, nk := range ordered {
		got, ok := packNodeKey(nk)
		if !ok {
			t.Fatalf("packNodeKey(%v) rejected a valid key", nk)
		}
		if i > 0 && got <= prev {
			t.Fatalf("packNodeKey(%v) = %d, not greater than the previous %d", nk, got, prev)
		}
		prev = got
	}
	for _, nk := range []nodeKey{{version: -1}, {version: math.MaxUint32 + 1}, {version: 1, nonce: -1}} {
		if _, ok := packNodeKey(nk); ok {
			t.Fatalf("packNodeKey(%v) accepted an out-of-range key", nk)
		}
	}
}

// TestResolves covers nodeDB.GetNode's fallback: pruning rewrites a root from
// (v,1) to (v,0), and references to it must still resolve.
func TestResolves(t *testing.T) {
	pack := func(v int64, n int32) uint64 {
		p, ok := packNodeKey(nodeKey{version: v, nonce: n})
		if !ok {
			t.Fatalf("packNodeKey(v%d,n%d) failed", v, n)
		}
		return p
	}
	// Version 5's root was reformatted to nonce 0; version 9 was never stored.
	keys := []uint64{pack(5, 0), pack(7, 1), pack(7, 42)}

	for _, tc := range []struct {
		nk   nodeKey
		want bool
		why  string
	}{
		{nodeKey{version: 7, nonce: 1}, true, "present as stored"},
		{nodeKey{version: 7, nonce: 42}, true, "present as stored"},
		{nodeKey{version: 5, nonce: 1}, true, "falls back to the reformatted (v5,n0)"},
		{nodeKey{version: 9, nonce: 1}, false, "neither (v9,n1) nor (v9,n0) exists"},
		{nodeKey{version: 5, nonce: 2}, false, "only nonce 1 gets the fallback"},
		{nodeKey{version: -1, nonce: 1}, false, "an impossible key matches nothing"},
	} {
		if got := resolves(keys, tc.nk); got != tc.want {
			t.Fatalf("resolves(%v) = %v, want %v (%s)", tc.nk, got, tc.want, tc.why)
		}
	}
}

func TestAuditHealthy(t *testing.T) {
	dir := t.TempDir()
	buildFixture(t, dir)
	db, names := openFixture(t, dir)

	out := captureStdout(t, func() {
		if err := auditAll(db, names, 20); err != nil {
			t.Fatal(err)
		}
	})
	// 2 stores x 299 inner nodes x 2 children.
	for _, want := range []string{
		"checked 1196 references: 0 dangling, 0 missing node(s), 0 store(s) affected",
		"internally consistent",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestAuditFindsDamage(t *testing.T) {
	dir := t.TempDir()
	buildFixture(t, dir, nodeKey{version: 1, nonce: 7})
	db, names := openFixture(t, dir)

	out := captureStdout(t, func() {
		if err := auditAll(db, names, 20); err != nil {
			t.Fatal(err)
		}
	})
	// The removed node was referenced once, by its parent in evm only; bank
	// holds a node with the same key and must stay clean.
	for _, want := range []string{
		"(v1,n7) is missing: 1 reference(s) from parents v1..v1; ",
		"1 dangling, 1 missing node(s), 1 store(s) affected",
		"affected stores: evm",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	// The targeted scan must reach the same verdict per store.
	out = captureStdout(t, func() {
		if err := findParents(db, names, nodeKey{version: 1, nonce: 7}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"present in bank", "(v1,n7) (dangling)", "2 to (v1,n7), 1 dangling"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "present in evm") {
		t.Fatalf("deleted node reported present in evm:\n%s", out)
	}
}

// TestAuditMultiVersion exercises the single pass on a tree that grew over
// many versions: references to older versions resolve as they are met, and
// same-version references wait for the version to end. One node of each kind
// is then removed and both must be reported.
func TestAuditMultiVersion(t *testing.T) {
	const versions, updates, keyspace = 40, 25, 500

	clean := t.TempDir()
	buildVersioned(t, clean, versions, updates, keyspace)
	db, names := openFixture(t, clean)

	out := captureStdout(t, func() {
		if err := auditAll(db, names, 20); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "0 dangling") {
		t.Fatalf("clean tree audited dirty:\n%s", out)
	}

	// Pick one child of each kind and count how many parents reference each,
	// since an unchanged old node is referenced again by every later version.
	var older, same nodeKey
	count := map[nodeKey]int{}
	err := eachRef(db, "evm", nodeKey{}, func(parent nodeKey, r reference, _ node) error {
		count[r.nk]++
		switch {
		case r.nk.version < parent.version && older == (nodeKey{}):
			older = r.nk
		case r.nk.version == parent.version && same == (nodeKey{}):
			if r.nk.nonce <= parent.nonce {
				t.Fatalf("%v references same-version %v with a lower nonce; the single pass relies on pre-order nonces", parent, r.nk)
			}
			same = r.nk
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if older == (nodeKey{}) || same == (nodeKey{}) || older == same {
		t.Fatalf("fixture lacks both reference kinds: older=%v same=%v", older, same)
	}

	// findParents starts its scan at the target's version; it must still see
	// every parent, which all live at that version or later.
	out = captureStdout(t, func() {
		if err := findParents(db, names, older); err != nil {
			t.Fatal(err)
		}
	})
	if want := fmt.Sprintf("%d to %v", count[older], older); !strings.Contains(out, want) {
		t.Fatalf("output missing %q:\n%s", want, out)
	}

	damaged := t.TempDir()
	buildVersioned(t, damaged, versions, updates, keyspace, older, same)
	db, names = openFixture(t, damaged)

	out = captureStdout(t, func() {
		if err := auditAll(db, names, 1000); err != nil {
			t.Fatal(err)
		}
	})
	want := fmt.Sprintf("%d dangling, 2 missing node(s), 1 store(s) affected", count[older]+count[same])
	for _, w := range []string{
		want,
		fmt.Sprintf("%v is missing: %d reference(s)", older, count[older]),
		fmt.Sprintf("%v is missing: %d reference(s)", same, count[same]),
	} {
		if !strings.Contains(out, w) {
			t.Fatalf("output missing %q:\n%s", w, out)
		}
	}
}

// TestAuditMaxReport checks that truncation is stated rather than silent.
func TestAuditMaxReport(t *testing.T) {
	dir := t.TempDir()
	buildFixture(t, dir, nodeKey{version: 1, nonce: 7}, nodeKey{version: 1, nonce: 9})
	db, names := openFixture(t, dir)

	out := captureStdout(t, func() {
		if err := auditAll(db, names, 1); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"... and 1 more missing node(s) not printed", "2 dangling, 2 missing node(s)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// TestUnlockedOpensAHeldDatabase holds a fixture open the way a running node
// does and checks that only the unlocked filesystem can open it again.
func TestUnlockedOpensAHeldDatabase(t *testing.T) {
	dir := t.TempDir()
	buildFixture(t, dir)
	path := filepath.Join(dir, "application.db")

	holder, err := pebble.Open(path, &pebble.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	if db, err := pebble.Open(path, &pebble.Options{ReadOnly: true}); err == nil {
		db.Close()
		t.Fatal("a second open succeeded while the lock was held")
	}

	db, err := pebble.Open(path, &pebble.Options{ReadOnly: true, FS: unlocked{vfs.Default}})
	if err != nil {
		t.Fatalf("unlocked open: %v", err)
	}
	defer db.Close()
	if names, err := storeNames(db); err != nil || len(names) != 2 {
		t.Fatalf("read through the unlocked open: names=%v err=%v", names, err)
	}
}

func captureStdout(t testing.TB, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()
	w.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
