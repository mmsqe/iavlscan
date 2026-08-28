//go:build !rocksdb

package main

import "errors"

// openRocks stands in for the backend this build left out; detection names
// rocksdb either way, so its database is told what it is, not called unreadable.
func openRocks(string, bool, bool) (kvDB, error) {
	return nil, errors.New("built without rocksdb; rebuild with -tags rocksdb")
}
