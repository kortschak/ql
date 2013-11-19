// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Well known handles
// 1: root
// 2: id

package ql

import (
	"crypto/sha1"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/camlistore/lock"
	"github.com/cznic/exp/lldb"
)

const (
	magic = "\x60\xdbql"
)

var (
	_ btreeIterator = (*fileBTreeIterator)(nil)
	_ storage       = (*file)(nil)
	_ temp          = (*fileTemp)(nil)
)

// OpenFile returns a DB backed by a named file. The back end limits the size
// of a record to about 64 kB.
func OpenFile(name string, opt *Options) (db *DB, err error) {
	var f lldb.OSFile
	if f = opt.OSFile; f == nil {
		f, err = os.OpenFile(name, os.O_RDWR, 0666)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}

			if !opt.CanCreate {
				return nil, err
			}

			f, err = os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0666)
			if err != nil {
				return nil, err
			}
		}
	}

	fi, err := newFileFromOSFile(f) // always ACID
	if err != nil {
		return
	}

	if fi.tempFile = opt.TempFile; fi.tempFile == nil {
		fi.tempFile = func(dir, prefix string) (f lldb.OSFile, err error) {
			f0, err := ioutil.TempFile(dir, prefix)
			return f0, err
		}
	}

	return newDB(fi)
}

// Options amend the behavior of OpenFile.
//
// CanCreate
//
// The CanCreate option enables OpenFile to create the DB file if it does not
// exists.
//
// OSFile
//
// OSFile allows to pass an os.File like back end providing, for example,
// encrypted storage. If this field is nil then OpenFile uses the file named by
// the 'name' parameter instead.
//
// TempFile
//
// TempFile provides a temporary file used for evaluating the GROUP BY, ORDER
// BY, ... clauses. The hook is intended to be used by encrypted DB back ends
// to avoid leaks of unecrypted data to such temp files by providing temp files
// which are encrypted as well. Note that *os.File satisfies the lldb.OSFile
// interface.
//
// If TempFile is nil it defaults to ioutil.TempFile.
type Options struct {
	CanCreate bool
	OSFile    lldb.OSFile
	TempFile  func(dir, prefix string) (f lldb.OSFile, err error)
}

type fileBTreeIterator struct {
	en *lldb.BTreeEnumerator
	t  *fileTemp
}

func (it *fileBTreeIterator) Next() (k, v []interface{}, err error) {
	bk, bv, err := it.en.Next()
	if err != nil {
		return
	}

	if k, err = lldb.DecodeScalars(bk); err != nil {
		return
	}

	if err = enforce(k, it.t.colsK); err != nil {
		return
	}

	if v, err = lldb.DecodeScalars(bv); err != nil {
		return
	}

	err = enforce(v, it.t.colsV)
	return
}

var lldbCollators = map[bool]func(a, b []byte) int{false: lldbCollateDesc, true: lldbCollate}

func lldbCollateDesc(a, b []byte) int {
	return -lldbCollate(a, b)
}

func lldbCollate(a, b []byte) (r int) {
	da, err := lldb.DecodeScalars(a)
	if err != nil {
		log.Panic(err)
	}

	db, err := lldb.DecodeScalars(b)
	if err != nil {
		log.Panic(err)
	}

	r, err = lldb.Collate(da, db, nil)
	if err != nil {
		log.Panic(err)
	}

	return
}

func enforce(val []interface{}, cols []*col) (err error) {
	for i, v := range val {
		if val[i], err = convert(v, cols[i].typ); err != nil {
			return
		}
	}
	return
}

func infer(from []interface{}, to *[]*col) {
	if len(*to) == 0 {
		*to = make([]*col, len(from))
		for i := range *to {
			(*to)[i] = &col{}
		}
	}
	for i, c := range *to {
		if f := from[i]; f != nil {
			switch x := f.(type) {
			//case nil:
			case idealComplex:
				c.typ = qComplex128
				from[i] = complex128(x)
			case idealFloat:
				c.typ = qFloat64
				from[i] = float64(x)
			case idealInt:
				c.typ = qInt64
				from[i] = int64(x)
			case idealRune:
				c.typ = qInt32
				from[i] = int32(x)
			case idealUint:
				c.typ = qUint64
				from[i] = uint64(x)
			case bool:
				c.typ = qBool
			case complex128:
				c.typ = qComplex128
			case complex64:
				c.typ = qComplex64
			case float64:
				c.typ = qFloat64
			case float32:
				c.typ = qFloat32
			case int8:
				c.typ = qInt8
			case int16:
				c.typ = qInt16
			case int32:
				c.typ = qInt32
			case int64:
				c.typ = qInt64
			case string:
				c.typ = qString
			case uint8:
				c.typ = qUint8
			case uint16:
				c.typ = qUint16
			case uint32:
				c.typ = qUint32
			case uint64:
				c.typ = qUint64
			}
		}
	}
}

type fileTemp struct {
	*file
	colsK []*col
	colsV []*col
	t     *lldb.BTree
}

func (t *fileTemp) BeginTransaction() error {
	return nil
}

func (t *fileTemp) Get(k []interface{}) (v []interface{}, err error) {
	bk, err := lldb.EncodeScalars(k...)
	if err != nil {
		return
	}

	bv, err := t.t.Get(nil, bk)
	if err != nil {
		return
	}

	return lldb.DecodeScalars(bv)
}

func (t *fileTemp) Drop() (err error) {
	if t.f0 == nil {
		return
	}

	fn := t.f0.Name()
	if err = t.f0.Close(); err != nil {
		return
	}

	if fn == "" {
		return
	}

	return os.Remove(fn)
}

func (t *fileTemp) SeekFirst() (it btreeIterator, err error) {
	en, err := t.t.SeekFirst()
	if err != nil {
		return
	}

	return &fileBTreeIterator{t: t, en: en}, nil
}

func (t *fileTemp) Set(k, v []interface{}) (err error) {
	infer(k, &t.colsK)
	infer(v, &t.colsV)

	bk, err := lldb.EncodeScalars(k...)
	if err != nil {
		return
	}

	bv, err := lldb.EncodeScalars(v...)
	if err != nil {
		return
	}

	return t.t.Set(bk, bv)
}

type file struct {
	a        *lldb.Allocator
	codec    *gobCoder
	f        lldb.Filer
	f0       lldb.OSFile
	id       int64
	lck      io.Closer
	name     string
	rwmu     sync.RWMutex
	tempFile func(dir, prefix string) (f lldb.OSFile, err error)
	wal      *os.File
}

func newFileFromOSFile(f lldb.OSFile) (fi *file, err error) {
	nm := lockName(f.Name())
	lck, err := lock.Lock(nm)
	if err != nil {
		if lck != nil {
			lck.Close()
		}
		return nil, err
	}

	close := true
	defer func() {
		if close && lck != nil {
			lck.Close()
		}
	}()

	var w *os.File
	closew := false
	wn := walName(f.Name())
	w, err = os.OpenFile(wn, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0666)
	closew = true
	defer func() {
		if closew {
			nm := w.Name()
			w.Close()
			os.Remove(nm)
			w = nil
		}
	}()

	if err != nil {
		if !os.IsExist(err) {
			return nil, err
		}

		closew = false
		w, err = os.OpenFile(wn, os.O_RDWR, 0666)
		if err != nil {
			return nil, err
		}

		closew = true
		st, err := w.Stat()
		if err != nil {
			return nil, err
		}

		if st.Size() != 0 {
			return nil, fmt.Errorf("non empty WAL file %s exists", wn)
		}
	}

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	switch sz := info.Size(); {
	case sz == 0:
		b := make([]byte, 16)
		copy(b, []byte(magic))
		if _, err := f.Write(b); err != nil {
			return nil, err
		}

		filer := lldb.Filer(lldb.NewOSFiler(f))
		filer = lldb.NewInnerFiler(filer, 16)
		if filer, err = lldb.NewACIDFiler(filer, w); err != nil {
			return nil, err
		}

		a, err := lldb.NewAllocator(filer, &lldb.Options{})
		if err != nil {
			return nil, err
		}

		a.Compress = true
		s := &file{
			a:     a,
			codec: newGobCoder(),
			f0:    f,
			f:     filer,
			lck:   lck,
			name:  f.Name(),
			wal:   w,
		}
		if err = s.BeginTransaction(); err != nil {
			return nil, err
		}

		h, err := s.Create()
		if err != nil {
			return nil, err
		}

		if h != 1 { // root
			log.Panic("internal error")
		}

		if h, err = s.a.Alloc(make([]byte, 8)); err != nil {
			return nil, err
		}

		if h != 2 { // id
			log.Panic("internal error")
		}

		close, closew = false, false
		return s, s.Commit()
	default:
		b := make([]byte, 16)
		if _, err := f.Read(b); err != nil {
			return nil, err
		}

		if string(b[:len(magic)]) != magic {
			return nil, fmt.Errorf("unknown file format")
		}

		filer := lldb.Filer(lldb.NewOSFiler(f))
		filer = lldb.NewInnerFiler(filer, 16)
		if filer, err = lldb.NewACIDFiler(filer, w); err != nil {
			return nil, err
		}

		a, err := lldb.NewAllocator(filer, &lldb.Options{})
		if err != nil {
			return nil, err
		}

		bid, err := a.Get(nil, 2) // id
		if err != nil {
			return nil, err
		}

		if len(bid) != 8 {
			return nil, fmt.Errorf("corrupted id |% x|", bid)
		}

		id := int64(0)
		for _, v := range bid {
			id = (id << 8) | int64(v)
		}

		a.Compress = true
		s := &file{
			a:     a,
			codec: newGobCoder(),
			f0:    f,
			f:     filer,
			id:    id,
			lck:   lck,
			name:  f.Name(),
			wal:   w,
		}

		close, closew = false, false
		return s, nil
	}
}

func (s *file) Acid() bool { return s.wal != nil }

func errSet(p *error, errs ...error) (err error) {
	err = *p
	for _, e := range errs {
		if err != nil {
			return
		}
		*p, err = e, e
	}
	return
}

func (s *file) lock() func() {
	s.rwmu.Lock()
	return s.rwmu.Unlock
}

func (s *file) rLock() func() {
	s.rwmu.RLock()
	return s.rwmu.RUnlock
}

func (s *file) Close() (err error) {
	if s.wal != nil {
		defer s.lock()()
	}

	es := s.f0.Sync()
	ef := s.f0.Close()
	var ew error
	if s.wal != nil {
		ew = s.wal.Close()
	}
	el := s.lck.Close()
	return errSet(&err, es, ef, ew, el)
}

func (s *file) Name() string { return s.name }

func (s *file) Verify() (allocs int64, err error) {
	if s.wal != nil {
		defer s.lock()()
	}
	var stat lldb.AllocStats
	if err = s.a.Verify(lldb.NewMemFiler(), nil, &stat); err != nil {
		return
	}

	allocs = stat.AllocAtoms
	return
}

func (s *file) CreateTemp(asc bool) (bt temp, err error) {
	f, err := s.tempFile("", "ql-tmp-")
	if err != nil {
		return nil, err
	}

	fn := f.Name()
	filer := lldb.NewOSFiler(f)
	a, err := lldb.NewAllocator(filer, &lldb.Options{})
	if err != nil {
		f.Close()
		os.Remove(fn)
		return nil, err
	}

	t, _, err := lldb.CreateBTree(a, lldbCollators[asc])
	if err != nil {
		f.Close()
		if fn != "" {
			os.Remove(fn)
		}
		return nil, err
	}

	x := &fileTemp{file: &file{
		a:  a,
		f0: f,
	},
		t: t}
	return x, nil
}

func (s *file) BeginTransaction() (err error) {
	if s.wal != nil {
		defer s.lock()()
	}
	return s.f.BeginUpdate()
}

func (s *file) Rollback() (err error) {
	if s.wal != nil {
		defer s.lock()()
	}
	return s.f.Rollback()
}

func (s *file) Commit() (err error) {
	if s.wal != nil {
		defer s.lock()()
	}
	return s.f.EndUpdate()
}

func (s *file) Create(data ...interface{}) (h int64, err error) {
	if s.wal != nil {
		defer s.lock()()
	}
	b, err := lldb.EncodeScalars(data...)
	if err != nil {
		return
	}

	return s.a.Alloc(b)
}

func (s *file) Delete(h int64) (err error) {
	if s.wal != nil {
		defer s.lock()()
	}
	return s.a.Free(h)
}

func (s *file) ResetID() (err error) {
	s.id = 0
	return
}

func (s *file) ID() (int64, error) {
	if s.wal != nil {
		defer s.lock()()
	}
	s.id++
	b := make([]byte, 8)
	id := s.id
	for i := 7; i >= 0; i-- {
		b[i] = byte(id)
		id >>= 8
	}

	return s.id, s.a.Realloc(2, b)
}

func (s *file) Read(dst []interface{}, h int64, cols ...*col) (data []interface{}, err error) {
	if s.wal != nil {
		defer s.rLock()()
	}
	b, err := s.a.Get(nil, h) //TODO +bufs
	if err != nil {
		return
	}

	rec, err := lldb.DecodeScalars(b)
	if err != nil {
		return
	}

	for _, col := range cols {
		i := col.index + 2
		switch col.typ {
		case 0:
		case qBool:
		case qComplex64:
			rec[i] = complex64(rec[i].(complex128))
		case qComplex128:
		case qFloat32:
			rec[i] = float32(rec[i].(float64))
		case qFloat64:
		case qInt8:
			rec[i] = int8(rec[i].(int64))
		case qInt16:
			rec[i] = int16(rec[i].(int64))
		case qInt32:
			rec[i] = int32(rec[i].(int64))
		case qInt64:
		case qString:
		case qUint8:
			rec[i] = uint8(rec[i].(uint64))
		case qUint16:
			rec[i] = uint16(rec[i].(uint64))
		case qUint32:
			rec[i] = uint32(rec[i].(uint64))
		case qUint64:
		default:
			log.Panic("internal error")
		}
	}

	return rec, nil
}

func (s *file) Update(h int64, data ...interface{}) (err error) {
	if s.wal != nil {
		defer s.lock()()
	}
	b, err := lldb.EncodeScalars(data...)
	if err != nil {
		return
	}

	return s.a.Realloc(h, b)
}

func lockName(dbname string) string {
	base := filepath.Base(filepath.Clean(dbname)) + "lockfile"
	h := sha1.New()
	io.WriteString(h, base)
	return filepath.Join(filepath.Dir(dbname), fmt.Sprintf(".%x", h.Sum(nil)))
}

func walName(dbname string) (r string) {
	base := filepath.Base(filepath.Clean(dbname))
	h := sha1.New()
	io.WriteString(h, base)
	return filepath.Join(filepath.Dir(dbname), fmt.Sprintf(".%x", h.Sum(nil)))
}
