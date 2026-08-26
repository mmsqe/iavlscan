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
iavlscan -db ~/.mantrachain/data/application.db -nodekey 7300...f2   # is one node present, and who references it
iavlscan -db ~/.mantrachain/data/application.db -list                # node key range per store
iavlscan -decode "[0c421501f18296...0242]"                           # one node value, no database
```

`-store evm` restricts `-audit` and `-nodekey` to one store, and tells `-decode`
which layout to read its tree key under.

The node must be stopped: pebble locks the directory even in read-only mode.
Point `-db` at a snapshot or a copy otherwise. `-audit` and `-nodekey` read
every node once and are disk bound, so on a mainnet database expect hours;
`-audit` also holds 8 bytes of memory per node of the largest store.

`-audit` is the one to start with:

```
  bank                     nodes=9364      refs=13742     dangling=1
      (v17530112,n88) left child (v16678423,n242) is missing; tree key=0214a6fd...e2616d616e747261
        (bank/Balance holder=mantra15m77x4pe6w9vtpuqm22qxu0ds7vn4ehzwx8pls denom="amantra")

checked 83926 references: 1 dangling, 1 distinct missing node(s), 1 store(s) affected
```

One dangling edge is surgical, what a targeted delete looks like. Many across
stores is bulk loss. The tree key names the module data under the damaged
branch. To tell a delete from a lost write, `pebble find` on the missing key
shows whether a `DEL` tombstone survives.

## Tree keys

A tree key prints as hex, then as the module record it encodes when the store's
layout is known (`acc`, `bank`, `staking`, `distribution`, `slashing`, `mint`,
`gov`, `evm`, `erc20`, `feemarket`, read off cosmos-sdk v0.53 and cosmos/evm).
Unknown prefixes and leftover bytes stay hex rather than guess, so a wrong table
shows up as a `rest=` tail. Addresses render under `-bech32` (default
`mantra`); `-bech32 ""` keeps them hex.
