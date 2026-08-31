// iavlscan finds which IAVL store owns a node missing from a cosmos-sdk
// application.db, given the nodeKey from a "Value missing for key" error.
// The node is gone, so it looks for the parents that still reference it.
//
// Keys are s/k:<store>/s<version:8be><nonce:4be>. Parents hold children as
// varints, not raw keys, so nodes have to be decoded to find one.
package main

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

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
		jobs   = flag.Int("jobs", 1, "stores to scan at once for -audit and -nodekey; each -audit job holds its own node index in memory")
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
		return auditAll(db, names, *maxRep, *jobs)
	case *treeK != "":
		key, err := hex.DecodeString(strings.TrimPrefix(*treeK, "0x"))
		if err != nil {
			return fmt.Errorf("parse treekey: %w", err)
		}
		_, err = walkTo(db, *store, key)
		return err
	default:
		return findParents(db, names, target, *jobs)
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

// scanStores runs scan over the stores, jobs at a time. Stores share nothing,
// so a disk that answers several reads at once finishes several stores in the
// time one took. Each result reaches emit in store order as soon as its own
// scan lands, so a run of hours still says where it has got to.
func scanStores[T any](names []string, jobs int, scan func(string) (T, error), emit func(string, T) error) error {
	type result struct {
		val  T
		err  error
		done chan struct{}
	}
	var (
		results = make([]result, len(names))
		sem     = make(chan struct{}, max(jobs, 1))
		stopped atomic.Bool
		wg      sync.WaitGroup
	)
	// A scan still reading must not outlive the database it is reading, so an
	// early return waits for whatever is in flight.
	defer wg.Wait()
	for i, name := range names {
		r := &results[i]
		r.done = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(r.done)
			sem <- struct{}{}
			defer func() { <-sem }()
			// Nothing queued behind a failure is worth reading, which is also
			// what keeps -jobs 1 stopping exactly where a plain loop would.
			if stopped.Load() {
				return
			}
			r.val, r.err = scan(name)
		}()
	}
	for i, name := range names {
		<-results[i].done
		if results[i].err == nil {
			results[i].err = emit(name, results[i].val)
		}
		if results[i].err != nil {
			stopped.Store(true)
			return results[i].err
		}
	}
	return nil
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
// dangling. -jobs stores are scanned at once; unlike the audit, a scan here
// holds only its report lines, so extra jobs cost nothing worth counting.
func findParents(db kvDB, names []string, target nodeKey, jobs int) error {
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
	type parentScan struct {
		scanned, hits, dangling int
		lines                   []string
	}
	var scanned, hits, dangling int
	err := scanStores(names, jobs, func(name string) (parentScan, error) {
		var s parentScan
		// A parent is never older than its child, so nothing before the
		// target's version can reference it.
		from := nodeKey{version: target.version}
		err := eachRef(db, name, from, func(parent nodeKey, r reference, n node) error {
			s.scanned++
			if r.nk != target {
				return nil
			}
			s.hits++
			note := ""
			if !present[name] {
				s.dangling++
				note = " (dangling)"
			}
			// Formatted here rather than kept: n's slices are only valid
			// during this call.
			s.lines = append(s.lines,
				fmt.Sprintf("  %s: %s %s %s%s; %s", name, parent, r.side, target, note, where(name, n)))
			return nil
		})
		if err != nil {
			return s, fmt.Errorf("scan store %s: %w", name, err)
		}
		return s, nil
	}, func(_ string, s parentScan) error {
		for _, l := range s.lines {
			fmt.Println(l)
		}
		scanned += s.scanned
		hits += s.hits
		dangling += s.dangling
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("\nscanned %d references, %d to %v, %d dangling\n", scanned, hits, target, dangling)
	if hits == 0 {
		fmt.Println("nothing references it: either pruning was right to remove it and the " +
			"fault came from elsewhere, or this is not the database that produced the error.")
	}
	return nil
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
