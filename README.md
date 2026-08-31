# iavlscan

Finds which IAVL store owns a node that has gone missing from a Cosmos SDK
`application.db`.

```
ERR iavl set error error="Value missing for key [...] corresponding to nodeKey 730000000000fe7e17000000f2"
```

That error names the node but not the store, and the node itself is gone, so
iavlscan works backwards from the parents that still point at it.

```sh
go build -o iavlscan .

iavlscan -db ~/.mantrachain/data/application.db -audit               # every dangling reference
iavlscan -db ~/.mantrachain/data/application.db -audit -jobs 4       # four stores at a time
iavlscan -db ~/.mantrachain/data/application.db -nodekey 7300...f2   # is one node present, and who references it
iavlscan -db ~/.mantrachain/data/application.db -list                # node key range per store
iavlscan -decode "[0c421501f18296...0242]"                           # one node value, no database

iavlscan -db … -store bank -treekey 0214a6fd…616d616e747261            # the path a write to that key takes
iavlscan -db … -store bank -delete 7300000000000000030000001f          # delete a node to simulate damage
```

`-treekey` walks the latest tree to a key the way `Set` does, so a node missing
on that path is the one the next write fails on; its last line is the
`-delete` command for the leaf. `-delete` exists to reproduce the fault in a
test — it is irreversible and opens the database read-write.

`-store evm` restricts `-audit` and `-nodekey` to one store, and tells `-decode`
which layout to read its tree key under.

The database is pebble, goleveldb or rocksdb, read off its directory unless
`-backend` says which. Rocksdb needs cgo, so it sits behind a build tag; a build
without it still names a rocksdb database rather than calling it unreadable.

```sh
nix profile install nixpkgs#rocksdb_9_10
export CGO_CFLAGS="-I$HOME/.nix-profile/include"
export CGO_LDFLAGS="-L$HOME/.nix-profile/lib"
go build -tags "rocksdb grocksdb_clean_link" -o iavlscan .
```

Both tags earn their place: the pinned grocksdb compiles against rocksdb 9.x —
the pairing mantrachain builds with — not 10.x, and `grocksdb_clean_link` leaves
`-lsnappy -lzstd -llz4 -lz` to the rocksdb library, which nix has already
pointed at its own store paths.

The node must be stopped: pebble and goleveldb lock the directory even in
read-only mode. Point `-db` at a snapshot or a copy, or pass `-no-lock` to read
the running node's database in place; rocksdb takes no lock to read. Nothing is
written either way, so the node is unaffected, but a compaction can remove a
file under the scan and fail it. `-audit` and `-nodekey` read every node once
and are disk bound, so on a mainnet database expect hours. `-jobs` scans that
many stores at once, which is what shortens both; an `-audit` job also holds
8 bytes of memory per node of its store.

`-audit` is the one to start with:

```
  distribution             nodes=30665252  refs=42565394  dangling=3766961  missing=217       root gaps=0
      (v16678423,n242) is missing: 80018 reference(s) from parents v17447578..v17527595; left child of (v17447578,n172), tree key=0614388f...86aa1 (distribution/ValidatorCurrentRewards val=mantravaloper18z8jw8wpqq3pe8t87cuf7ed98eg8s64pn6udx3)
      ... and 216 more missing node(s) not printed (raise -max-report to see them)

checked 42565394 references: 3766961 dangling, 217 missing node(s), 0 root gap(s), 1 store(s) affected
```

Root gaps are counted separately: nothing references a root, so the reference
check cannot see one go missing. A gap parks pruning on `version does not
exist`.

Count missing nodes, not references: a parent rewritten every block references
the same missing child once per version. One missing node is surgical, what a
targeted delete looks like; many is a prune gone wrong or bulk loss. The tree
key names the module data under the damaged branch. To tell a delete from a
lost write, `pebble find` on the missing node key shows whether a `DEL`
tombstone survives; on the other two the report points at `-nodekey` instead.

## Tree keys

A tree key prints as hex, then as the module record it encodes when the store's
layout is known (`acc`, `bank`, `staking`, `distribution`, `slashing`, `mint`,
`gov`, `evm`, `erc20`, `feemarket`, read off cosmos-sdk v0.53 and cosmos/evm).
Unknown prefixes and leftover bytes stay hex rather than guess, so a wrong table
shows up as a `rest=` tail. Addresses render under `-bech32` (default
`mantra`); `-bech32 ""` keeps them hex.
