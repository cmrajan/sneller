// Copyright (C) 2022 Sneller, Inc.
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"hash"
	"io"
	"path"
	"time"

	"github.com/SnellerInc/sneller/date"
	"github.com/SnellerInc/sneller/db"
	"github.com/SnellerInc/sneller/expr"
	"github.com/SnellerInc/sneller/ion/blockfmt"
	"github.com/SnellerInc/sneller/plan"

	"golang.org/x/crypto/blake2b"
)

type savedIndex struct {
	db, table string
	index     *blockfmt.Index
}

type savedList struct {
	db   string
	list []string
}

// fsEnv provides a plan.Env from a db.FS
type fsEnv struct {
	root   db.FS
	db     string
	tenant db.Tenant

	recent []savedIndex
	lists  []savedList

	// FIXME: change cachedEnv and don't
	// keep the accumulated state here:
	hash    hash.Hash
	modtime date.Time
}

func environ(t db.Tenant, dbname string) (cachedEnv, error) {
	root, err := t.Root()
	if err != nil {
		return nil, err
	}
	src, ok := root.(db.FS)
	if !ok {
		return nil, fmt.Errorf("db %T from auth cannot be used for reading", root)
	}
	h, _ := blake2b.New256(nil)
	return &fsEnv{
		tenant: t,
		root:   src,
		db:     dbname,
		hash:   h,
	}, nil
}

func syntax(f string, args ...interface{}) error {
	return &expr.SyntaxError{
		Msg: fmt.Sprintf(f, args...),
	}
}

func tsplit(p *expr.Path) (string, string, error) {
	d, ok := p.Rest.(*expr.Dot)
	if !ok {
		return "", "", syntax("no database+table reference in %q", p)
	}
	if d.Rest != nil {
		return "", "", syntax("trailing path expression %q in table not supported", d.Rest)
	}
	return p.First, d.Field, nil
}

// CacheValues implements cachedEnv.CacheValues
func (f *fsEnv) CacheValues() ([]byte, time.Time) {
	return f.hash.Sum(nil), f.modtime.Time()
}

var _ plan.Indexer = (*fsEnv)(nil)

func (f *fsEnv) Index(p expr.Node) (plan.Index, error) {
	return f.index(p)
}

func (f *fsEnv) index(e expr.Node) (*blockfmt.Index, error) {
	var dbname, table string
	var err error
	p, ok := e.(*expr.Path)
	if !ok {
		return nil, syntax("unexpected table expression %q", expr.ToString(e))
	}
	// if a database was already provided,
	// then we expect just the table identifier;
	// otherwise, we expect db.table
	if f.db == "" {
		dbname, table, err = tsplit(p)
	} else {
		dbname = f.db
		table = p.First
		if p.Rest != nil {
			err = syntax("trailing path expression %q in table not supported", p.Rest)
		}
	}
	if err != nil {
		return nil, err
	}

	// if a query references the same table
	// more than once (common with CTEs, nested SELECTs, etc.),
	// then don't load the index more than once; it is expensive
	for i := range f.recent {
		if f.recent[i].db == dbname && f.recent[i].table == table {
			return f.recent[i].index, nil
		}
	}
	index, err := db.OpenPartialIndex(f.root, dbname, table, f.tenant.Key())
	if err != nil {
		return nil, err
	}
	f.recent = append(f.recent, savedIndex{
		db:    dbname,
		table: table,
		index: index,
	})
	if f.modtime.IsZero() || f.modtime.Before(index.Created) {
		f.modtime = index.Created
	}
	// FIXME: actually use the ETag of the index
	//
	// but, the modtime of the index plus its name
	// ought to be unique per-input
	io.WriteString(f.hash, path.Join(dbname, table))
	io.WriteString(f.hash, index.Created.String())
	return index, nil
}

// Stat implements plan.Env.Stat
func (f *fsEnv) Stat(e, where expr.Node) (plan.TableHandle, error) {
	index, err := f.index(e)
	if err != nil {
		return nil, err
	}
	var keep func(*blockfmt.SparseIndex, int) bool
	var match filter
	if where != nil {
		if m, ok := compileFilter(where); ok {
			match = m
			keep = func(s *blockfmt.SparseIndex, n int) bool {
				return match(s, n) != never
			}
		}
	}
	blobs, err := db.Blobs(f.root, index, keep)
	if err != nil {
		return nil, err
	}
	return &filterHandle{filter: where, compiled: match, blobs: blobs}, nil
}

var _ plan.TableLister = (*fsEnv)(nil)

// ListTables implements plan.TableLister.ListTables
func (f *fsEnv) ListTables(dbname string) ([]string, error) {
	if dbname == "" {
		dbname = f.db
	}
	for i := range f.lists {
		if f.lists[i].db == dbname {
			return f.lists[i].list, nil
		}
	}
	li, err := db.ListTables(f.root, dbname)
	if err != nil {
		return nil, err
	}
	f.lists = append(f.lists, savedList{
		db:   dbname,
		list: li,
	})
	return li, nil
}

var _ plan.UploadEnv = (*fsEnv)(nil)

func (f *fsEnv) Uploader() plan.UploadFS {
	fs, _ := f.root.(plan.UploadFS)
	return fs
}

func (f *fsEnv) Key() *blockfmt.Key {
	return f.tenant.Key()
}
