// iavlscan finds which IAVL store owns a node missing from a cosmos-sdk
// application.db, given the nodeKey from a "Value missing for key" error.
// The node is gone, so it looks for the parents that still reference it.
//
// Keys are s/k:<store>/s<version:8be><nonce:4be>. Parents hold children as
// varints, not raw keys, so nodes have to be decoded to find one.
package main

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "iavlscan: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath = flag.String("db", "", "path to application.db")
		rawKey = flag.String("nodekey", "", "find the parents of this node; hex as printed in the error")
		audit  = flag.Bool("audit", false, "check every reference in every store, reporting dangling ones")
		maxRep = flag.Int("max-report", 20, "with -audit, dangling references to print per store")
		list   = flag.Bool("list", false, "print each store's node key range")
		store  = flag.String("store", "", "restrict -audit and -nodekey to one store, and read -decode's tree key under it")
		decode = flag.String("decode", "", "decode one node value, hex as printed by `pebble find`; needs no -db")
		noLock = flag.Bool("no-lock", false, "skip the directory lock, to read a database a running node holds open")
		treeK  = flag.String("treekey", "", "walk -store's latest tree down to this tree key (hex), printing the path a write takes")
		del    = flag.String("delete", "", "delete this node (hex nodeKey) from -store to simulate damage; irreversible, the node must be stopped")
		hrp    = flag.String("bech32", "mantra", "account prefix for addresses in tree keys; empty prints them as hex")
		back   = flag.String("backend", backendAuto, "database format: auto, pebble, goleveldb or rocksdb")
	)
	flag.Usage = usage
	flag.Parse()
	bech32HRP = *hrp
	if err := setBackend(*back); err != nil {
		return err
	}

	if *decode != "" {
		return decodeValue(*store, *decode)
	}
	var target nodeKey
	if *rawKey != "" {
		var err error
		if target, err = parseNodeKey(*rawKey); err != nil {
			return err
		}
	} else if !*list && !*audit && *treeK == "" && *del == "" {
		flag.Usage()
		return fmt.Errorf("one of -nodekey, -audit, -list, -treekey, -delete or -decode is required")
	}
	if *dbPath == "" {
		flag.Usage()
		return fmt.Errorf("-db is required")
	}
	if (*treeK != "" || *del != "") && *store == "" {
		return fmt.Errorf("-treekey and -delete need -store")
	}

	// -delete is the one mode that writes; it opens read-write and so must
	// hold the lock itself.
	if *del != "" {
		nk, err := parseNodeKey(*del)
		if err != nil {
			return err
		}
		db, err := openDB(*dbPath, *back, true, false)
		if err != nil {
			return fmt.Errorf("open %s: %w", *dbPath, err)
		}
		defer db.Close()
		return deleteNode(db, *store, nk)
	}

	// Opening read-only still takes the directory lock, so the node has to be
	// stopped or this pointed at a snapshot -- unless told not to take it.
	db, err := openDB(*dbPath, *back, false, *noLock)
	if err != nil {
		return fmt.Errorf("open %s: %w", *dbPath, err)
	}
	defer db.Close()

	names, err := storeNames(db)
	if err != nil {
		return fmt.Errorf("enumerate stores: %w", err)
	}
	fmt.Printf("%d stores: %s\n\n", len(names), strings.Join(names, " "))
	if *store != "" {
		if !slices.Contains(names, *store) {
			return fmt.Errorf("no store named %q", *store)
		}
		names = []string{*store}
	}

	switch {
	case *list:
		return listRanges(db, names)
	case *audit:
		return auditAll(db, names, *maxRep)
	case *treeK != "":
		key, err := hex.DecodeString(strings.TrimPrefix(*treeK, "0x"))
		if err != nil {
			return fmt.Errorf("parse treekey: %w", err)
		}
		_, err = walkTo(db, *store, key)
		return err
	default:
		return findParents(db, names, target)
	}
}

// walkTo descends a store's latest tree to a tree key the way a Get or Set
// does, printing each node on the way. A node missing from that path is the
// one the next write to the key will fail on. Returns the leaf's node key.
func walkTo(db kvDB, store string, key []byte) (nodeKey, error) {
	nk, ok := latestRoot(db, store)
	if !ok {
		return nodeKey{}, fmt.Errorf("%s has no nodes", store)
	}
	fmt.Printf("== %s: path to %x ==\n", store, key)
	var hops int
	for {
		n, ok := getNode(db, store, nk)
		if !ok {
			return nodeKey{}, fmt.Errorf("%s: %v is missing; a write to %x fails here", store, nk, key)
		}
		switch {
		case n.ref: // an unchanged version points at an earlier root
			// nodeDB.GetRootKey takes one hop and reads whatever it lands on as
			// a node, so a chain is a shape iavl cannot follow either.
			if hops++; hops > 1 {
				return nodeKey{}, fmt.Errorf("%s: %v is a reference root pointing at another one, which iavl does not resolve", store, nk)
			}
			nk = n.refTo
			continue
		case n.empty:
			return nodeKey{}, fmt.Errorf("%s is empty at its latest version", store)
		}
		fmt.Printf("  %-18v %s\n", nk, where(store, n))
		if n.leaf {
			if !bytes.Equal(n.key, key) {
				return nodeKey{}, fmt.Errorf("%x is not in %s; the walk ended at %v", key, store, nk)
			}
			fmt.Printf("leaf %v holds it: iavlscan -db <db> -store %s -delete %x%x\n", nk, store, nodeTag, nk.bytes())
			return nk, nil
		}
		if n.left.legacy || n.right.legacy {
			return nodeKey{}, fmt.Errorf("%v has a legacy child, which this walk cannot follow", nk)
		}
		// Inner keys are the smallest key of the right subtree.
		if bytes.Compare(key, n.key) < 0 {
			nk = n.left.nk
		} else {
			nk = n.right.nk
		}
	}
}

// latestRoot is the root key of the store's newest version: every version
// writes a root record at nonce 1, so it is the highest version seen.
func latestRoot(db kvDB, store string) (nodeKey, bool) {
	prefix := nodePrefix(store)
	it, err := db.NewIter(prefix, upperBound(prefix))
	if err != nil {
		return nodeKey{}, false
	}
	defer it.Close()
	last, ok := edgeNode(it, it.Last, it.Prev, len(prefix))
	return nodeKey{version: last.version, nonce: 1}, ok
}

// deleteNode removes one node, the way a bad prune would, so the failure a
// damaged node produces can be reproduced on purpose.
func deleteNode(db kvDB, store string, nk nodeKey) error {
	key := nodeDBKey(store, nk)
	if _, err := db.Get(key); err != nil {
		return fmt.Errorf("%s has no node %v: %w", store, nk, err)
	}
	if err := db.Delete(key); err != nil {
		return err
	}
	fmt.Printf("deleted %s %v\n", store, nk)
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `iavlscan finds the IAVL store that owns a missing node.

usage:
  iavlscan -db <application.db> -audit
  iavlscan -db <application.db> -nodekey <hex>
  iavlscan -db <application.db> -list
  iavlscan -db <application.db> -store <name> -treekey <hex>
  iavlscan -db <application.db> -store <name> -delete <hex>   (simulates damage)
  iavlscan -decode <hex> [-store <name>]

The database is pebble, goleveldb or rocksdb, read off its directory unless
-backend says which. Rocksdb needs a build with -tags rocksdb.

Pebble and goleveldb lock the directory even in read-only mode, so the node
must be stopped, or -no-lock passed to read its database anyway; rocksdb takes
no lock to read. The node is unaffected either way, but a compaction can fail
the scan midway; retry, or use a snapshot.

flags:
`)
	flag.PrintDefaults()
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

// eachNode calls fn for every node of one store from key `from` on, in key
// order. The value is only valid during the call.
func eachNode(db kvDB, store string, from nodeKey, fn func(nodeKey, []byte) error) error {
	prefix := nodePrefix(store)
	it, err := db.NewIter(nodeDBKey(store, from), upperBound(prefix))
	if err != nil {
		return err
	}
	defer it.Close()

	// A mainnet store takes hours, so report the rate every so often.
	var seen int
	start, last := time.Now(), time.Now()
	for ok := it.First(); ok; ok = it.Next() {
		k := it.Key()
		if len(k) != len(prefix)+nodeKeyLen {
			continue
		}
		if seen++; seen%1_000_000 == 0 && time.Since(last) >= 30*time.Second {
			fmt.Printf("  ... %s: %dM nodes, %.0fk nodes/s\n", store, seen/1_000_000,
				float64(seen)/time.Since(start).Seconds()/1000)
			last = time.Now()
		}
		if err := fn(decodeNodeKey(k[len(prefix):]), it.Value()); err != nil {
			return err
		}
	}
	return it.Error()
}

// eachRef calls fn for every (version, nonce) reference held by one store's
// nodes: two children per inner node, one pointer per reference root. The
// parent node's slices are only valid during the call.
func eachRef(db kvDB, store string, from nodeKey, fn func(parent nodeKey, r reference, n node) error) error {
	buf := make([]reference, 0, 2)
	return eachNode(db, store, from, func(parent nodeKey, val []byte) error {
		n, ok := decodeNode(val)
		if !ok {
			return nil
		}
		for _, r := range n.outgoing(buf) {
			if err := fn(parent, r, n); err != nil {
				return err
			}
		}
		return nil
	})
}

// storeNames walks the s/k: range, skipping each store's contents once its
// name is known ('/'+1 == '0'), so this costs one seek per store.
func storeNames(db kvDB) ([]string, error) {
	prefix := []byte(rootPrefix)
	it, err := db.NewIter(prefix, upperBound(prefix))
	if err != nil {
		return nil, err
	}
	defer it.Close()

	var out []string
	for it.First(); it.Valid(); {
		rest := it.Key()[len(prefix):]
		i := bytes.IndexByte(rest, '/')
		if i < 0 {
			it.Next()
			continue
		}
		name := string(rest[:i])
		out = append(out, name)
		it.SeekGE([]byte(rootPrefix + name + "0"))
	}
	return out, it.Error()
}

// listRanges prints the first and last node key of each store. Two seeks per
// store, no scan.
func listRanges(db kvDB, names []string) error {
	for _, name := range names {
		prefix := nodePrefix(name)
		it, err := db.NewIter(prefix, upperBound(prefix))
		if err != nil {
			return err
		}
		first, ok := edgeNode(it, it.First, it.Next, len(prefix))
		last, _ := edgeNode(it, it.Last, it.Prev, len(prefix))
		err = it.Error()
		it.Close()
		if err != nil {
			return err
		}
		if !ok {
			fmt.Printf("  %-24s (no nodes)\n", name)
			continue
		}
		fmt.Printf("  %-24s first=%-18v last=%v\n", name, first, last)
	}
	return nil
}

// edgeNode returns the first node key found from one end of the range.
func edgeNode(it kvIter, start, step func() bool, prefixLen int) (nodeKey, bool) {
	for ok := start(); ok; ok = step() {
		if k := it.Key(); len(k) == prefixLen+nodeKeyLen {
			return decodeNodeKey(k[prefixLen:]), true
		}
	}
	return nodeKey{}, false
}

// findParents answers two questions about the target: is it in any store, and
// who references it. A reference from a store the target is absent from is
// dangling.
func findParents(db kvDB, names []string, target nodeKey) error {
	present := map[string]bool{}
	for _, name := range names {
		if n, ok := getNode(db, name, target); ok {
			present[name] = true
			fmt.Printf("%v is present in %s, %s\n", target, name, where(name, n))
		}
	}
	if len(present) == 0 {
		fmt.Printf("%v is present in no store\n", target)
	}

	fmt.Printf("\n== references to %v ==\n", target)
	var scanned, hits, dangling int
	for _, name := range names {
		// A parent is never older than its child, so nothing before the
		// target's version can reference it.
		from := nodeKey{version: target.version}
		err := eachRef(db, name, from, func(parent nodeKey, r reference, n node) error {
			scanned++
			if r.nk != target {
				return nil
			}
			hits++
			note := ""
			if !present[name] {
				dangling++
				note = " (dangling)"
			}
			fmt.Printf("  %s: %s %s %s%s; %s\n", name, parent, r.side, target, note, where(name, n))
			return nil
		})
		if err != nil {
			return fmt.Errorf("scan store %s: %w", name, err)
		}
	}
	fmt.Printf("\nscanned %d references, %d to %v, %d dangling\n", scanned, hits, target, dangling)
	if hits == 0 {
		fmt.Println("nothing references it: either pruning was right to remove it and the " +
			"fault came from elsewhere, or this is not the database that produced the error.")
	}
	return nil
}

// versionRange is a run of versions with no root record.
type versionRange struct{ from, to int64 }

func (r versionRange) String() string {
	if r.from == r.to {
		return fmt.Sprintf("no root record for version %d", r.from)
	}
	return fmt.Sprintf("no root record for versions %d..%d (%d)", r.from, r.to, r.to-r.from+1)
}

// storeAudit is what one store's pass found.
type storeAudit struct {
	nodes   int
	refs    int
	missing []missingNode // what the dangling references point at, oldest first
	gaps    []versionRange
}

// danglingRefs is how many references resolve to nothing. Each one is counted
// against the node it points at, so the nodes carry the total between them.
func (a storeAudit) danglingRefs() int {
	var n int
	for _, m := range a.missing {
		n += m.refs
	}
	return n
}

// report prints up to maxReport lines, then says how many it held back. A
// negative one holds them all back rather than slicing past the end.
func report[T fmt.Stringer](items []T, maxReport int, what string) {
	maxReport = max(maxReport, 0)
	for _, it := range items[:min(len(items), maxReport)] {
		fmt.Printf("      %s\n", it)
	}
	if n := len(items) - maxReport; n > 0 {
		fmt.Printf("      ... and %d more %s not printed (raise -max-report to see them)\n", n, what)
	}
}

// dangling is one reference to a node that is not in the database.
type dangling struct {
	parent nodeKey
	ref    reference
}

// missingNode is one absent node and what points at it: the earliest
// reference, kept whole for the report, the latest parent version, and how
// many references there are in between.
type missingNode struct {
	nk    nodeKey
	first dangling
	last  int64
	refs  int
}

// placed is a missing node ready to print. Locating the parent costs a read,
// so it happens in String, which report only calls for the lines it prints.
type placed struct {
	missingNode
	db    kvDB
	store string
}

func (p placed) String() string {
	return fmt.Sprintf("%s is missing: %d reference(s) from parents v%d..v%d; %s of %s, %s",
		p.nk, p.refs, p.first.parent.version, p.last, p.first.ref.side, p.first.parent,
		whereOf(p.db, p.store, p.first.parent))
}

// missingNodes collects dangling references by the node they point at. A parent
// rewritten every block points at the same missing child once per version, so
// this holds one entry per missing node rather than one per reference.
type missingNodes map[nodeKey]*missingNode

// note records one dangling reference. They arrive in parent order, so the
// first one seen is the oldest and the last one wins.
func (m missingNodes) note(d dangling) {
	n, ok := m[d.ref.nk]
	if !ok {
		n = &missingNode{nk: d.ref.nk, first: d}
		m[d.ref.nk] = n
	}
	n.last = d.parent.version
	n.refs++
}

// sorted lays the nodes out oldest first, the order they were written in.
func (m missingNodes) sorted() []missingNode {
	out := make([]missingNode, 0, len(m))
	for _, n := range m {
		out = append(out, *n)
	}
	slices.SortFunc(out, func(a, b missingNode) int {
		return cmp.Or(cmp.Compare(a.nk.version, b.nk.version), cmp.Compare(a.nk.nonce, b.nk.nonce))
	})
	return out
}

// auditAll checks every reference in every store, so the damage can be read as
// isolated (one bad prune decision) or widespread (bulk loss). It is disk
// bound: one pass over every node, 8 bytes of memory per node of the largest
// store, and one entry per missing node however many references it has.
func auditAll(db kvDB, names []string, maxReport int) error {
	var (
		totalRefs, totalDangling, totalMissing, totalGaps int
		hitStores                                         []string
	)
	for _, name := range names {
		a, err := auditStore(db, name)
		if err != nil {
			return fmt.Errorf("audit store %s: %w", name, err)
		}
		dangling := a.danglingRefs()
		totalRefs += a.refs
		totalDangling += dangling
		totalMissing += len(a.missing)
		totalGaps += len(a.gaps)
		fmt.Printf("  %-24s nodes=%-9d refs=%-9d dangling=%-9d missing=%-9d root gaps=%d\n",
			name, a.nodes, a.refs, dangling, len(a.missing), len(a.gaps))
		if len(a.missing) == 0 && len(a.gaps) == 0 {
			continue
		}
		hitStores = append(hitStores, name)
		// Nothing references a root, so the reference check cannot see one go
		// missing; a gap is what parks the pruner on "version does not exist".
		report(a.gaps, maxReport, "root gap(s)")
		lines := make([]placed, len(a.missing))
		for j, m := range a.missing {
			lines[j] = placed{m, db, name}
		}
		report(lines, maxReport, "missing node(s)")
	}

	fmt.Printf("\nchecked %d references: %d dangling, %d missing node(s), %d root gap(s), %d store(s) affected\n",
		totalRefs, totalDangling, totalMissing, totalGaps, len(hitStores))
	if len(hitStores) == 0 {
		fmt.Println("every reference resolves; this database is internally consistent")
	} else {
		fmt.Printf("affected stores: %s\n", strings.Join(hitStores, " "))
	}
	return nil
}

// auditStore checks every reference of one store in a single pass, packing
// node keys into a sorted array as it goes (the iteration yields them in key
// order, and both fields are big-endian).
//
// A child is created no later than its parent, and within one version parents
// take lower nonces than children (saveNewNodes assigns them pre-order). So in
// key order a reference to an older version always finds its node already in
// the array, and a same-version reference only has to wait until the version
// ends. Nothing needs a second pass.
func auditStore(db kvDB, store string) (a storeAudit, err error) {
	var (
		keys    []uint64
		buf     = make([]reference, 0, 2)
		pending []dangling // same-version references, checked once the version is complete
		version int64
		byNode  = missingNodes{}
	)
	// hasVersion and GetRoot both look only at nonce 1, so a version above
	// the first one carrying it, but with none of its own, is a hole the
	// pruner parks on, reporting "version does not exist". Nodes that outlive
	// their own root are ordinary: a pruned version keeps whatever later
	// versions still share.
	var lastRoot int64
	flush := func() {
		for _, d := range pending {
			if !resolves(keys, d.ref.nk) {
				byNode.note(d)
			}
		}
		pending = pending[:0]
	}
	err = eachNode(db, store, nodeKey{}, func(parent nodeKey, val []byte) error {
		if parent.version != version {
			flush()
			version = parent.version
		}
		packed, ok := packNodeKey(parent)
		if !ok {
			return fmt.Errorf("node key %v does not fit the packed form", parent)
		}
		keys = append(keys, packed)
		if parent.nonce == 1 {
			if lastRoot != 0 && parent.version > lastRoot+1 {
				a.gaps = append(a.gaps, versionRange{lastRoot + 1, parent.version - 1})
			}
			lastRoot = parent.version
		}

		n, ok := decodeNode(val)
		if !ok {
			return nil
		}
		for _, r := range n.outgoing(buf) {
			a.refs++
			d := dangling{parent: parent, ref: r}
			switch {
			case r.nk.version >= parent.version:
				pending = append(pending, d)
			case !resolves(keys, r.nk):
				byNode.note(d)
			}
		}
		return nil
	})
	flush()
	a.nodes, a.missing = len(keys), byNode.sorted()
	return a, err
}

// getNode fetches and decodes one node, with nodeDB.GetNode's fallback from a
// missing (v,1) to the reformatted root (v,0).
func getNode(db kvDB, store string, nk nodeKey) (node, bool) {
	val, err := db.Get(nodeDBKey(store, nk))
	if err != nil && nk.nonce == 1 {
		return getNode(db, store, nodeKey{version: nk.version})
	}
	if err != nil {
		return node{}, false
	}
	return decodeNode(val)
}

// whereOf is where(getNode(...)) for a report, "?" if the node cannot be read.
func whereOf(db kvDB, store string, nk nodeKey) string {
	if n, ok := getNode(db, store, nk); ok {
		return where(store, n)
	}
	return "?"
}

// where places a record for a report: the tree key it sits under, or the kind
// of root it is when it has none.
func where(store string, n node) string {
	switch {
	case n.ref:
		return fmt.Sprintf("reference root -> %v", n.refTo)
	case n.empty:
		return "empty root"
	}
	return "tree key=" + describeKey(store, n.key)
}

// resolves reports whether a reference finds a node, mirroring nodeDB.GetNode:
// a missing key whose nonce is 1 falls back to (version, 0), the reformatted
// root deleteVersion leaves behind. Without that fallback every pruned root
// would look dangling.
func resolves(keys []uint64, nk nodeKey) bool {
	if has(keys, nk) {
		return true
	}
	return nk.nonce == 1 && has(keys, nodeKey{version: nk.version})
}

func has(keys []uint64, nk nodeKey) bool {
	packed, ok := packNodeKey(nk)
	if !ok {
		return false // nothing in the index can match an impossible key
	}
	_, found := slices.BinarySearch(keys, packed)
	return found
}

// packNodeKey folds a node key into one uint64, preserving the stored order.
// Both fields are written unsigned and big-endian, so this is lossless for any
// version below 2^32 -- far above any real chain height.
func packNodeKey(nk nodeKey) (uint64, bool) {
	if nk.version < 0 || nk.version > math.MaxUint32 || nk.nonce < 0 {
		return 0, false
	}
	return uint64(nk.version)<<32 | uint64(uint32(nk.nonce)), true //nolint:gosec // nonce is checked non-negative
}

// decodeValue prints one node value, taken from the [...] field of a
// `pebble find` or `pebble sstable scan` line. Without a store the tree key
// stays hex.
func decodeValue(store, raw string) error {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(strings.TrimSuffix(raw, "]"), "[") // as pebble prints it
	raw = strings.TrimPrefix(raw, "0x")
	buf, err := hex.DecodeString(raw)
	if err != nil {
		return fmt.Errorf("parse value %q: %w", raw, err)
	}
	n, ok := decodeNode(buf)
	if !ok {
		return fmt.Errorf("not an iavl node value (%d bytes)", len(buf))
	}

	switch {
	case n.empty:
		fmt.Println("empty root: this version has no tree")
		return nil
	case n.ref:
		fmt.Printf("reference root: this version's root is unchanged, so it points at %v\n", n.refTo)
		fmt.Println(n.refTo.find())
		return nil
	}

	kind := "inner"
	if n.leaf {
		kind = "leaf"
	}
	fmt.Printf("height    %d  (%s)\n", n.height, kind)
	fmt.Printf("size      %d\n", n.size)
	fmt.Printf("tree key  %s\n", describeKey(store, n.key))
	if n.leaf {
		fmt.Printf("value     %s\n", describe(n.value))
	} else {
		fmt.Printf("hash      %x\n", n.hash)
		fmt.Printf("left      %s\n", n.left)
		fmt.Printf("right     %s\n", n.right)
	}
	// iavl writes nothing after the last field, so leftovers mean the value was
	// truncated, or was never an iavl node to begin with.
	if n.trailing != 0 {
		fmt.Printf("\nwarning: %d trailing byte(s) -- decode is suspect\n", n.trailing)
	}
	return nil
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

// upperBound is the exclusive end of a prefix range.
func upperBound(prefix []byte) []byte {
	ub := bytes.Clone(prefix)
	for i := len(ub) - 1; i >= 0; i-- {
		if ub[i] < 0xff {
			ub[i]++
			return ub[:i+1]
		}
	}
	return nil
}

// describe renders a tree key or value as hex, adding the quoted text when it
// is all printable.
func describe(b []byte) string {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("%x", b)
		}
	}
	return fmt.Sprintf("%x (%q)", b, b)
}
