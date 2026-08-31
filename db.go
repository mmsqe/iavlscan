// application.db is pebble or goleveldb, and the IAVL layout above it is the
// same either way. This file is the only place that knows which: everything
// else works through kvDB.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// The values -backend takes.
const (
	backendAuto   = "auto"
	backendPebble = "pebble"
	backendLevel  = "goleveldb"
	backendRocks  = "rocksdb"
)

func unknownBackend(name string) error {
	return fmt.Errorf("unknown -backend %q; want %s, %s, %s or %s",
		name, backendAuto, backendPebble, backendLevel, backendRocks)
}

// dbBackend is the format in force. Only the `find` hints read it, and -decode
// prints those with no database open, hence a default rather than empty.
var dbBackend = backendPebble

// errNotFound is the one Get error that means the key is absent; anything
// else is the database failing to answer, which for this tool is a different
// diagnosis from a missing node.
var errNotFound = errors.New("key not found")

// kvDB is the part of a key/value store the scans use.
type kvDB interface {
	// Get returns a copy of one key's value; errNotFound if it is absent.
	Get(key []byte) ([]byte, error)
	// NewIter walks [lower, upper); a nil upper runs to the end.
	NewIter(lower, upper []byte) (kvIter, error)
	Delete(key []byte) error
	Close() error
}

// kvIter is a cursor over one key range. Key and Value are only valid until
// the next move, as they are in both backends.
type kvIter interface {
	First() bool
	Last() bool
	Next() bool
	Prev() bool
	SeekGE(key []byte) bool
	Valid() bool
	Key() []byte
	Value() []byte
	Error() error
	Close() error
}

// setBackend validates -backend and records what it named. -decode opens no
// database, so its hints have only this to go on.
func setBackend(name string) error {
	switch name {
	case backendPebble, backendLevel, backendRocks:
		dbBackend = name
	case backendAuto:
	default:
		return unknownBackend(name)
	}
	return nil
}

// openDB opens the database at path, working out its format unless -backend
// named one. write is for -delete, the one mode that changes anything and so
// the one that must hold the lock; noLock skips it, to read a database a
// running node holds open.
func openDB(path, backend string, write, noLock bool) (kvDB, error) {
	if backend == backendAuto {
		var err error
		if backend, err = detectBackend(path); err != nil {
			return nil, err
		}
	}
	dbBackend = backend
	noLock = noLock && !write
	switch backend {
	case backendPebble:
		return openPebble(path, write, noLock)
	case backendLevel:
		return openLevel(path, write, noLock)
	case backendRocks:
		return openRocks(path, write, noLock)
	}
	return nil, unknownBackend(backend)
}

// detectBackend reads the directory listing, taking each backend's own file
// before the ones it shares: IDENTITY is rocksdb's alone and MARKER pebble's,
// though both write OPTIONS, and only goleveldb is left to claim CURRENT.
// Table names settle nothing, goleveldb having written .sst too before v1.14.
func detectBackend(path string) (string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	var isPebble, isLevel bool
	for _, e := range entries {
		switch name := e.Name(); {
		case name == "IDENTITY":
			return backendRocks, nil
		case strings.HasPrefix(name, "MARKER."), strings.HasPrefix(name, "OPTIONS-"):
			isPebble = true
		case name == "CURRENT", strings.HasSuffix(name, ".ldb"):
			isLevel = true
		}
	}
	switch {
	case isPebble:
		return backendPebble, nil
	case isLevel:
		return backendLevel, nil
	}
	return "", errors.New("not a pebble, goleveldb or rocksdb directory; pass -backend to say which")
}

func openPebble(path string, write, noLock bool) (kvDB, error) {
	opts := &pebble.Options{ReadOnly: !write}
	if noLock {
		opts.FS = unlocked{vfs.Default}
	}
	db, err := pebble.Open(path, opts)
	if err != nil {
		return nil, err
	}
	return pebbleDB{db}, nil
}

func openLevel(path string, write, noLock bool) (kvDB, error) {
	o := &opt.Options{ReadOnly: !write}
	var (
		db  *leveldb.DB
		err error
	)
	if noLock {
		db, err = leveldb.Open(unlockedDir{path}, o)
	} else {
		db, err = leveldb.OpenFile(path, o)
	}
	if err != nil {
		return nil, err
	}
	return levelDB{db}, nil
}

// pebbleDB adapts pebble to kvDB. Pebble's own iterator is already a kvIter.
type pebbleDB struct{ db *pebble.DB }

// Get copies the value out: pebble's own only lives until the closer runs.
func (d pebbleDB) Get(key []byte) ([]byte, error) {
	val, closer, err := d.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	return bytes.Clone(val), nil
}

func (d pebbleDB) NewIter(lower, upper []byte) (kvIter, error) {
	it, err := d.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	return it, nil
}

func (d pebbleDB) Delete(key []byte) error { return d.db.Delete(key, pebble.Sync) }
func (d pebbleDB) Close() error            { return d.db.Close() }

// levelDB adapts goleveldb to kvDB. Its Get already returns a copy.
type levelDB struct{ db *leveldb.DB }

func (d levelDB) Get(key []byte) ([]byte, error) {
	val, err := d.db.Get(key, nil)
	if errors.Is(err, leveldb.ErrNotFound) {
		return nil, errNotFound
	}
	return val, err
}

func (d levelDB) NewIter(lower, upper []byte) (kvIter, error) {
	return levelIter{d.db.NewIterator(&util.Range{Start: lower, Limit: upper}, nil)}, nil
}

func (d levelDB) Delete(key []byte) error { return d.db.Delete(key, &opt.WriteOptions{Sync: true}) }
func (d levelDB) Close() error            { return d.db.Close() }

// levelIter adapts goleveldb's cursor: it seeks under another name, and is
// released rather than closed, which is when its error stops being readable.
type levelIter struct{ iterator.Iterator }

func (it levelIter) SeekGE(key []byte) bool { return it.Seek(key) }

func (it levelIter) Close() error {
	err := it.Error()
	it.Release()
	return err
}

// unlocked is a pebble filesystem whose directory lock is a no-op, to read a
// database a running node holds open. ReadOnly writes nothing, so the node is
// unaffected; a compaction can delete a file under the reader, which fails the
// scan rather than skewing it.
type unlocked struct{ vfs.FS }

func (unlocked) Lock(string) (io.Closer, error) { return nopCloser{}, nil }

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// unlockedDir is the same escape for goleveldb, which has no filesystem to
// swap: enough of storage.Storage to open a database read-only, taking no LOCK
// and refusing everything that writes.
type unlockedDir struct{ path string }

var errReadOnly = errors.New("opened read-only, without the directory lock")

type noLocker struct{}

func (noLocker) Unlock() {}

func (unlockedDir) Lock() (storage.Locker, error) { return noLocker{}, nil }
func (unlockedDir) Log(string)                    {}
func (unlockedDir) Close() error                  { return nil }

func (unlockedDir) SetMeta(storage.FileDesc) error                  { return errReadOnly }
func (unlockedDir) Create(storage.FileDesc) (storage.Writer, error) { return nil, errReadOnly }
func (unlockedDir) Remove(storage.FileDesc) error                   { return errReadOnly }
func (unlockedDir) Rename(_, _ storage.FileDesc) error              { return errReadOnly }

// GetMeta reads CURRENT, whose one line names the manifest in force.
func (d unlockedDir) GetMeta() (storage.FileDesc, error) {
	b, err := os.ReadFile(filepath.Join(d.path, "CURRENT"))
	if err != nil {
		return storage.FileDesc{}, err
	}
	var num int64
	if _, err := fmt.Sscanf(string(b), "MANIFEST-%d", &num); err != nil {
		return storage.FileDesc{}, fmt.Errorf("%s/CURRENT names no manifest: %q", d.path, b)
	}
	return storage.FileDesc{Type: storage.TypeManifest, Num: num}, nil
}

func (d unlockedDir) List(ft storage.FileType) ([]storage.FileDesc, error) {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return nil, err
	}
	var out []storage.FileDesc
	for _, e := range entries {
		if fd, ok := parseFileDesc(e.Name()); ok && fd.Type&ft != 0 {
			out = append(out, fd)
		}
	}
	return out, nil
}

// Open also tries the .sst name goleveldb wrote tables under before v1.14.
func (d unlockedDir) Open(fd storage.FileDesc) (storage.Reader, error) {
	f, err := os.Open(filepath.Join(d.path, fd.String()))
	if os.IsNotExist(err) && fd.Type == storage.TypeTable {
		f, err = os.Open(filepath.Join(d.path, fmt.Sprintf("%06d.sst", fd.Num)))
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// parseFileDesc mirrors goleveldb's own fsParseName, which is unexported.
func parseFileDesc(name string) (storage.FileDesc, bool) {
	var (
		num  int64
		tail string
	)
	if _, err := fmt.Sscanf(name, "%d.%s", &num, &tail); err == nil {
		switch tail {
		case "log":
			return storage.FileDesc{Type: storage.TypeJournal, Num: num}, true
		case "ldb", "sst":
			return storage.FileDesc{Type: storage.TypeTable, Num: num}, true
		case "tmp":
			return storage.FileDesc{Type: storage.TypeTemp, Num: num}, true
		}
		return storage.FileDesc{}, false
	}
	if n, _ := fmt.Sscanf(name, "MANIFEST-%d%s", &num, &tail); n == 1 {
		return storage.FileDesc{Type: storage.TypeManifest, Num: num}, true
	}
	return storage.FileDesc{}, false
}

// upperBound is the exclusive end of a prefix range.
func upperBound(prefix []byte) []byte {
	ub := bytes.Clone(prefix)
	for i := len(ub) - 1; i >= 0; i-- {
		if ub[i] < 0xff {
			ub[i]++
			return ub[:i+1]
		}
	}
	return nil
}
