package main

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/iavl"
	iavldb "github.com/cosmos/iavl/db"
)

// BenchmarkAudit runs the audit over a ~3.6M node store that grew across 2000
// versions. Building the fixture takes ~30s and is not timed.
func BenchmarkAudit(b *testing.B) {
	for _, backend := range testBackends() {
		b.Run(backend, func(b *testing.B) {
			dir := b.TempDir()
			buildVersioned(b, backend, dir, 2000, 200, 50000)
			benchAudit(b, dir)
		})
	}
}

// BenchmarkAuditDamaged is the shape the tool is built for and the one the
// clean pass cannot show: many nodes gone, and every later version's rewritten
// parent still pointing at them. Watch the allocations rather than the time.
func BenchmarkAuditDamaged(b *testing.B) {
	for _, backend := range testBackends() {
		b.Run(backend, func(b *testing.B) {
			dir := b.TempDir()
			buildStable(b, backend, dir, 50000, 4000)
			damage(b, backend, dir, 300)
			benchAudit(b, dir)
		})
	}
}

// benchAudit times auditAll over an already-built fixture.
func benchAudit(b *testing.B, dir string) {
	b.Helper()
	db, names := openFixture(b, dir)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		captureStdout(b, func() {
			if err := auditAll(db, names, 20); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// buildStable writes a wide tree once and then rewrites only a hot path for
// many versions, the way a mainnet store behaves. A node that sits still is
// referenced again by every later version, which is what makes one missing node
// worth thousands of dangling references.
func buildStable(b *testing.B, backend, dir string, keyspace, versions int) {
	b.Helper()
	db := newFixtureDB(b, backend, dir)
	defer db.Close()

	pdb := dbm.NewPrefixDB(db, []byte(rootPrefix+"evm/"))
	tree := iavl.NewMutableTree(iavldb.NewWrapper(pdb), 0, false, iavl.NewNopLogger())
	set := func(i int, val []byte) {
		if _, err := tree.Set([]byte(fmt.Sprintf("key%06d", i)), val); err != nil {
			b.Fatalf("set: %v", err)
		}
	}
	save := func() {
		if _, _, err := tree.SaveVersion(); err != nil {
			b.Fatalf("save: %v", err)
		}
	}
	for i := range keyspace {
		set(i, []byte{byte(i)})
	}
	save()
	for v := range versions {
		for i := range 3 { // the hot path, rewritten every version
			set(i, []byte{byte(v)})
		}
		save()
	}
}

// damage deletes the n most-referenced nodes, the ones whose loss costs the
// most dangling references.
func damage(b *testing.B, backend, dir string, n int) {
	b.Helper()
	path := filepath.Join(dir, "application.db")
	db, err := openDB(path, backend, true, false)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	count := map[nodeKey]int{}
	if err := eachRef(db, "evm", nodeKey{}, func(_ nodeKey, r reference, _ node) error {
		count[r.nk]++
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	popular := make([]nodeKey, 0, len(count))
	for nk := range count {
		popular = append(popular, nk)
	}
	slices.SortFunc(popular, func(x, y nodeKey) int { return cmp.Compare(count[y], count[x]) })

	captureStdout(b, func() {
		for _, nk := range popular[:min(n, len(popular))] {
			if err := deleteNode(db, "evm", nk); err != nil {
				b.Fatalf("damage %v: %v", nk, err)
			}
		}
	})
}
