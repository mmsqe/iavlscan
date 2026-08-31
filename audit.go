// The -audit machinery: one pass per store, checking that every reference
// lands on a node that exists.
package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

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
// bound: one pass over every node, and 8 bytes of memory per node of each store
// being scanned; -jobs stores are scanned at once.
func auditAll(db kvDB, names []string, maxReport, jobs int) error {
	var (
		totalRefs, totalDangling, totalMissing, totalGaps int
		hitStores                                         []string
	)
	err := scanStores(names, jobs, func(name string) (storeAudit, error) {
		a, err := auditStore(db, name)
		if err != nil {
			return a, fmt.Errorf("audit store %s: %w", name, err)
		}
		return a, nil
	}, func(name string, a storeAudit) error {
		dangling := a.danglingRefs()
		totalRefs += a.refs
		totalDangling += dangling
		totalMissing += len(a.missing)
		totalGaps += len(a.gaps)
		fmt.Printf("  %-24s nodes=%-9d refs=%-9d dangling=%-9d missing=%-9d root gaps=%d\n",
			name, a.nodes, a.refs, dangling, len(a.missing), len(a.gaps))
		if len(a.missing) == 0 && len(a.gaps) == 0 {
			return nil
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
		return nil
	})
	if err != nil {
		return err
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
// node keys into a sorted index as it goes (the iteration yields them in key
// order, and both fields are big-endian).
//
// A child is created no later than its parent, and within one version parents
// take lower nonces than children (saveNewNodes assigns them pre-order). So in
// key order a reference to an older version always finds its node already in
// the index, and a same-version reference only has to wait until the version
// ends. Nothing needs a second pass.
func auditStore(db kvDB, store string) (a storeAudit, err error) {
	var (
		index   nodeIndex
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
			if !index.resolves(d.ref.nk) {
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
		index.add(packed)
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
			case !index.resolves(r.nk):
				byNode.note(d)
			}
		}
		return nil
	})
	flush()
	a.nodes, a.missing = index.n, byNode.sorted()
	return a, err
}

// nodeIndex holds one store's packed node keys in ascending order, in blocks
// rather than in one slice. A growing slice passes through holding both its old
// and new self, and the largest mainnet store's index runs to hundreds of
// megabytes, so that peak is what decides whether the audit fits in memory.
// Blocks never move once written, at a few percent on a cached lookup.
type nodeIndex struct {
	blocks [][]uint64
	// tails is each block's last key. Read off the blocks themselves they would
	// put a cache miss in every step of the search; together they stay in cache.
	tails []uint64
	n     int
}

// add takes the next key; the scan yields them in order, so the index sorts
// itself.
func (x *nodeIndex) add(packed uint64) {
	if x.n%indexBlock == 0 {
		x.blocks = append(x.blocks, make([]uint64, 0, indexBlock))
		x.tails = append(x.tails, 0)
	}
	last := len(x.blocks) - 1
	x.blocks[last] = append(x.blocks[last], packed)
	x.tails[last] = packed
	x.n++
}

// holds reports whether the index has this node key: one search for the block
// whose last key is the first not below it, then one search inside that block.
func (x *nodeIndex) holds(nk nodeKey) bool {
	packed, ok := packNodeKey(nk)
	if !ok {
		return false // nothing in the index can match an impossible key
	}
	i, _ := slices.BinarySearch(x.tails, packed)
	if i == len(x.blocks) {
		return false
	}
	_, found := slices.BinarySearch(x.blocks[i], packed)
	return found
}

// resolves reports whether a reference finds a node, mirroring nodeDB.GetNode:
// a missing key whose nonce is 1 falls back to (version, 0), the reformatted
// root deleteVersion leaves behind. Without that fallback every pruned root
// would look dangling.
func (x *nodeIndex) resolves(nk nodeKey) bool {
	return x.holds(nk) || (nk.nonce == 1 && x.holds(nodeKey{version: nk.version}))
}
