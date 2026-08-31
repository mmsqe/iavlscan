//go:build rocksdb

// Rocksdb needs cgo and the C++ library, which `go install` cannot assume, so
// it is the one backend behind a build tag. The README carries the recipe.
package main

import (
	"bytes"

	"github.com/linxGnu/grocksdb"
)

// openRocks opens a rocksdb application.db. Read-only never takes the LOCK,
// which is how rocksdb lets a running node's database be read, so noLock has
// nothing to skip and is accepted without effect.
func openRocks(path string, write, _ bool) (kvDB, error) {
	// The defaults create nothing missing and read any block-based table,
	// whatever options wrote it.
	opts := grocksdb.NewDefaultOptions()
	var (
		db  *grocksdb.DB
		err error
	)
	if write {
		db, err = grocksdb.OpenDb(opts, path)
	} else {
		// A running node always has a WAL, so finding one cannot be an error.
		db, err = grocksdb.OpenDbForReadOnly(opts, path, false)
	}
	if err != nil {
		opts.Destroy()
		return nil, err
	}
	return rocksDB{db, opts}, nil
}

// rocksDB adapts rocksdb to kvDB.
type rocksDB struct {
	db   *grocksdb.DB
	opts *grocksdb.Options
}

// Get seeks rather than calling rocksdb_get, which answers NULL both for a
// missing key and for a zero-length value -- and iavl writes a zero-length
// value for an empty root, which would then read as a node gone missing.
func (d rocksDB) Get(key []byte) ([]byte, error) {
	it, err := d.NewIter(key, upperBound(key))
	if err != nil {
		return nil, err
	}
	defer it.Close()
	if !it.First() || !bytes.Equal(it.Key(), key) {
		// A miss with a quiet cursor is an absent key; a noisy one failed.
		if err := it.Error(); err != nil {
			return nil, err
		}
		return nil, errNotFound
	}
	return bytes.Clone(it.Value()), nil
}

func (d rocksDB) NewIter(lower, upper []byte) (kvIter, error) {
	// An empty bound is not an absent one: rocksdb would read it as the empty
	// key and put the whole range out of bounds.
	ro := grocksdb.NewDefaultReadOptions()
	if len(lower) > 0 {
		ro.SetIterateLowerBound(lower)
	}
	if len(upper) > 0 {
		ro.SetIterateUpperBound(upper)
	}
	return rocksIter{d.db.NewIterator(ro), ro}, nil
}

func (d rocksDB) Delete(key []byte) error {
	wo := grocksdb.NewDefaultWriteOptions()
	wo.SetSync(true)
	defer wo.Destroy()
	return d.db.Delete(wo, key)
}

func (d rocksDB) Close() error {
	d.db.Close()
	d.opts.Destroy()
	return nil
}

// rocksIter adapts rocksdb's cursor. Its key and value slices are borrowed from
// the cursor's own buffer, so they last until the next move and are never freed
// -- the rule the other two backends follow anyway. The read options carry the
// iteration bounds, so they have to outlive the cursor.
type rocksIter struct {
	it *grocksdb.Iterator
	ro *grocksdb.ReadOptions
}

func (it rocksIter) First() bool            { it.it.SeekToFirst(); return it.it.Valid() }
func (it rocksIter) Last() bool             { it.it.SeekToLast(); return it.it.Valid() }
func (it rocksIter) Next() bool             { it.it.Next(); return it.it.Valid() }
func (it rocksIter) Prev() bool             { it.it.Prev(); return it.it.Valid() }
func (it rocksIter) SeekGE(key []byte) bool { it.it.Seek(key); return it.it.Valid() }
func (it rocksIter) Valid() bool            { return it.it.Valid() }
func (it rocksIter) Key() []byte            { return sliceBytes(it.it.Key()) }
func (it rocksIter) Value() []byte          { return sliceBytes(it.it.Value()) }
func (it rocksIter) Error() error           { return it.it.Err() }

func (it rocksIter) Close() error {
	err := it.it.Err()
	it.it.Close()
	it.ro.Destroy()
	return err
}

// sliceBytes is Slice.Data() for a cursor off its range, where rocksdb hands
// back no slice at all rather than an empty one.
func sliceBytes(s *grocksdb.Slice) []byte {
	if s == nil {
		return nil
	}
	return s.Data()
}
