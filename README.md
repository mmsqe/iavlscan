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
iavlscan -db ~/.mantrachain/data/application.db -nodekey 7300...f2   # parents of one node
iavlscan -db ~/.mantrachain/data/application.db -list                # node key range per store
iavlscan -decode "[0c421501f18296...0242]"                           # one node value, no database
```

`-store evm` restricts `-audit` and `-nodekey` to one store.

The node must be stopped: pebble locks the directory even in read-only mode.
Point `-db` at a snapshot or a copy otherwise. `-audit` and `-nodekey` read
every node once and are disk bound, so on a mainnet database expect hours;
`-audit` also holds 8 bytes of memory per node of the largest store.

`-audit` is the one to start with:

```
  evm                      nodes=9364      refs=13742     dangling=1
      (v17530112,n88) left child (v16678423,n242) is missing; tree key=02a1b2...

checked 83926 references: 1 dangling, 1 distinct missing node(s), 1 store(s) affected
```

One dangling edge is surgical, what a targeted delete looks like. Many across
stores is bulk loss. The tree key names the module data under the damaged
branch. To tell a delete from a lost write, `pebble find` on the missing key
shows whether a `DEL` tombstone survives.
