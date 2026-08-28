//go:build rocksdb

package main

import dbm "github.com/cosmos/cosmos-db"

// Fill in the fixture constructor the untagged build leaves nil, which is what
// puts rocksdb into eachBackend.
func init() {
	newRocksDB = func(dir string) (dbm.DB, error) { return dbm.NewRocksDB("application", dir, nil) }
}
