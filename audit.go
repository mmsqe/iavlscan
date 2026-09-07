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
	nodes        int
	refs         int
	missing      []missingNode // what the dangling references point at, oldest first
	unreferenced []nodeKey     // non-root nodes no parent names: leaked garbage
	gaps         []versionRange
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

// report prints up to maxReport of n lines, then says how many it held back.
// A negative maxReport holds them all back. line is called only for the lines
// printed, so it is where a read that places the record belongs.
func report(n, maxReport int, what string, line func(i int) string) {
	maxReport = max(maxReport, 0)
	for i := range min(n, maxReport) {
		fmt.Printf("      %s\n", line(i))
	}
	if rest := n - maxReport; rest > 0 {
		fmt.Printf("      ... and %d more %s not printed (raise -max-report to see them)\n", rest, what)
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

// sameVersionRef is a reference waiting for its version to finish: parent and
// child nonce, the version being the one under scan. Twelve bytes against
// dangling's forty matter because a state-synced store writes its whole tree
// at one version, so every reference it holds waits here at once.
type sameVersionRef struct {
	parent, child int32
	side          side
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
// bound: one pass over every node, and 8 bytes of memory per node of each
// store being scanned, plus 12 per reference of the version under scan; -jobs
// stores are scanned at once.
func auditAll(db kvDB, names []string, maxReport, jobs int) error {
	var (
		totalRefs, totalDangling, totalMissing, totalStranded, totalGaps int
		hitStores                                                        []string
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
		totalStranded += len(a.unreferenced)
		totalGaps += len(a.gaps)
		fmt.Printf("  %-24s nodes=%-9d refs=%-9d dangling=%-9d missing=%-9d unreferenced=%-9d root gaps=%d\n",
			name, a.nodes, a.refs, dangling, len(a.missing), len(a.unreferenced), len(a.gaps))
		if len(a.missing) == 0 && len(a.unreferenced) == 0 && len(a.gaps) == 0 {
			return nil
		}
		hitStores = append(hitStores, name)
		// Nothing references a root, so the reference check cannot see one go
		// missing; a gap is what parks the pruner on "version does not exist".
		report(len(a.gaps), maxReport, "root gap(s)", func(i int) string {
			return a.gaps[i].String()
		})
		report(len(a.missing), maxReport, "missing node(s)", func(i int) string {
			m := a.missing[i]
			return fmt.Sprintf("%s is missing: %d reference(s) from parents v%d..v%d; %s of %s, %s",
				m.nk, m.refs, m.first.parent.version, m.last, m.first.ref.side, m.first.parent,
				whereOf(db, name, m.first.parent))
		})
		report(len(a.unreferenced), maxReport, "unreferenced node(s)", func(i int) string {
			nk := a.unreferenced[i]
			return fmt.Sprintf("%s is referenced by nothing; %s", nk, whereOf(db, name, nk))
		})
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("\nchecked %d references: %d dangling, %d missing node(s), %d unreferenced node(s), %d root gap(s), %d store(s) affected\n",
		totalRefs, totalDangling, totalMissing, totalStranded, totalGaps, len(hitStores))
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
		pending []sameVersionRef // checked once the version is complete
		version int64
		byNode  = missingNodes{}
		named   bitset // one bit per index position: some parent names this node
	)
	// settle is a reference's verdict: mark the node it lands on, or record
	// it as dangling.
	settle := func(d dangling) {
		if i, ok := index.find(d.ref.nk); ok {
			named.set(i)
		} else {
			byNode.note(d)
		}
	}
	// hasVersion and GetRoot both look only at nonce 1, so a version above
	// the first one carrying it, but with none of its own, is a hole the
	// pruner parks on, reporting "version does not exist". Nodes that outlive
	// their own root are ordinary: a pruned version keeps whatever later
	// versions still share.
	var lastRoot int64
	flush := func() {
		for _, p := range pending {
			settle(dangling{
				parent: nodeKey{version, p.parent},
				ref:    reference{nodeKey{version, p.child}, p.side},
			})
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
			if r.nk.version == parent.version {
				pending = append(pending, sameVersionRef{parent.nonce, r.nk.nonce, r.side})
			} else {
				// An older child is indexed already; a newer one is not its
				// child at all, and waiting for the version to end would not
				// bring it into the index either.
				settle(dangling{parent: parent, ref: r})
			}
		}
		return nil
	})
	flush()

	// The reverse check: every node past the roots must be some parent's
	// child. A failed prune leaves subtrees no root reaches; this finds their
	// tops, while what hangs below stays referenced by the garbage above it.
	for i := 0; i < index.n; i++ {
		if nk := index.at(i); !named.has(i) && nk.nonce > 1 {
			a.unreferenced = append(a.unreferenced, nk)
		}
	}
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

// pos is the position of a node key in the index: one search for the block
// whose last key is the first not below it, then one search inside that block.
func (x *nodeIndex) pos(nk nodeKey) (int, bool) {
	packed, ok := packNodeKey(nk)
	if !ok {
		return 0, false // nothing in the index can match an impossible key
	}
	i, _ := slices.BinarySearch(x.tails, packed)
	if i == len(x.blocks) {
		return 0, false
	}
	j, found := slices.BinarySearch(x.blocks[i], packed)
	return i*indexBlock + j, found
}

// at is the node key at one position, every block being full but the last.
func (x *nodeIndex) at(i int) nodeKey {
	return unpackNodeKey(x.blocks[i/indexBlock][i%indexBlock])
}

// bitset is one bit per index position, grown as it is set.
type bitset []uint64

func (b *bitset) set(i int) {
	for w := i >> 6; len(*b) <= w; {
		*b = append(*b, 0)
	}
	(*b)[i>>6] |= 1 << (i & 63)
}

func (b bitset) has(i int) bool {
	w := i >> 6
	return w < len(b) && b[w]&(1<<(i&63)) != 0
}

// find is where a reference lands, mirroring nodeDB.GetNode: a missing key
// whose nonce is 1 falls back to (version, 0), the reformatted root
// deleteVersion leaves behind. Without that fallback every pruned root would
// look dangling.
func (x *nodeIndex) find(nk nodeKey) (int, bool) {
	if i, ok := x.pos(nk); ok {
		return i, true
	}
	if nk.nonce == 1 {
		return x.pos(nodeKey{version: nk.version})
	}
	return 0, false
}
