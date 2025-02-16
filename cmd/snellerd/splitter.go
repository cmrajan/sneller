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
	"net"

	"github.com/SnellerInc/sneller/expr"
	"github.com/SnellerInc/sneller/expr/blob"
	"github.com/SnellerInc/sneller/ion"
	"github.com/SnellerInc/sneller/plan"
	"github.com/SnellerInc/sneller/tenant/tnproto"
	"github.com/dchest/siphash"
)

const defaultSplitSize = int64(100 * 1024 * 1024)

type splitter struct {
	SplitSize int64
	workerID  tnproto.ID
	peers     []*net.TCPAddr
	selfAddr  string

	// compute total size of input blobs
	// and the maximum # of bytes scanned after
	// sparse indexing has been applied
	total, maxscan int64
}

func (s *server) newSplitter(workerID tnproto.ID, peers []*net.TCPAddr) *splitter {
	split := &splitter{
		SplitSize: s.splitSize,
		workerID:  workerID,
		peers:     peers,
	}
	if s.remote != nil {
		split.selfAddr = s.remote.String()
	}
	return split
}

func (s *splitter) Split(table expr.Node, handle plan.TableHandle) (plan.Subtables, error) {
	var blobs []blob.Interface
	fh, ok := handle.(*filterHandle)
	if !ok {
		return nil, fmt.Errorf("cannot split table handle of type %T", handle)
	}
	size := s.SplitSize
	if s.SplitSize == 0 {
		size = defaultSplitSize
	}
	flt := fh.compiled
	if flt == nil && fh.filter != nil {
		flt, _ = compileFilter(fh.filter)
	}
	splits := make([]split, len(s.peers))
	for i := range splits {
		splits[i].tp = s.transport(i)
	}
	insert := func(b blob.Interface) error {
		i, err := s.partition(b)
		if err != nil {
			return err
		}
		splits[i].blobs = append(splits[i].blobs, len(blobs))
		blobs = append(blobs, b)
		return nil
	}
	for _, b := range fh.blobs.Contents {
		stat, err := b.Stat()
		if err != nil {
			return nil, err
		}
		c, ok := b.(*blob.Compressed)
		if !ok {
			// we can only really do interesting
			// splitting stuff with blob.Compressed
			if err := insert(b); err != nil {
				return nil, err
			}
			s.total += stat.Size
			s.maxscan += stat.Size
			continue
		}
		s.total += c.Trailer.Decompressed()
		sub, err := c.Split(int(size))
		if err != nil {
			return nil, err
		}
		for i := range sub {
			// only insert blobs that satisfy
			// the predicate pushdown conditions
			scan := maxscan(&sub[i], flt)
			if scan == 0 {
				continue
			}
			s.maxscan += scan
			if err := insert(&sub[i]); err != nil {
				return nil, err
			}
		}
	}
	thfn := func(blobs []blob.Interface, flt expr.Node) plan.TableHandle {
		return &filterHandle{
			blobs:  &blob.List{Contents: blobs},
			filter: flt,
		}
	}
	return &subtables{
		splits: compact(splits),
		table:  table,
		blobs:  blobs,
		filter: nil, // pushed down later
		fn:     thfn,
	}, nil
}

// compact compacts splits so that any splits with no
// blobs are removed from the list.
func compact(splits []split) []split {
	out := splits[:0]
	for i := range splits {
		if len(splits[i].blobs) > 0 {
			out = append(out, splits[i])
		}
	}
	return out
}

// maxscan calculates the max scan size of a blob,
// optionally with filter f applied. If this returns 0,
// the entire blob is excluded by the filter.
func maxscan(pc *blob.CompressedPart, f filter) (scan int64) {
	t := pc.Parent.Trailer
	blocks := t.Blocks[pc.StartBlock:pc.EndBlock]
	for i := range blocks {
		if f == nil || f(&t.Sparse, pc.StartBlock+i) != never {
			scan += int64(blocks[i].Chunks) << t.BlockShift
		}
	}
	return scan
}

// partition returns the index of the peer which should
// handle the specified blob.
func (s *splitter) partition(b blob.Interface) (int, error) {
	info, err := b.Stat()
	if err != nil {
		return 0, err
	}

	// just two fixed random values
	key0 := uint64(0x5d1ec810)
	key1 := uint64(0xfebed702)

	hash := siphash.Hash(key0, key1, []byte(info.ETag))
	maxUint64 := ^uint64(0)
	idx := hash / (maxUint64 / uint64(len(s.peers)))
	return int(idx), nil
}

func (s *splitter) transport(i int) plan.Transport {
	nodeID := s.peers[i].String()
	if nodeID == s.selfAddr {
		return &plan.LocalTransport{}
	}
	return &tnproto.Remote{
		Tenant: s.workerID,
		Net:    "tcp",
		Addr:   nodeID,
	}
}

type split struct {
	tp    plan.Transport
	blobs []int
}

// encode as [tp, blobs]
func (s *split) encode(st *ion.Symtab, buf *ion.Buffer) error {
	buf.BeginList(-1)
	if err := plan.EncodeTransport(s.tp, st, buf); err != nil {
		return err
	}
	buf.BeginList(-1)
	for i := range s.blobs {
		buf.WriteInt(int64(s.blobs[i]))
	}
	buf.EndList()
	buf.EndList()
	return nil
}

func decodeSplit(st *ion.Symtab, body []byte) (split, error) {
	var s split
	if ion.TypeOf(body) != ion.ListType {
		return s, fmt.Errorf("expected a list; found ion type %s", ion.TypeOf(body))
	}
	body, _ = ion.Contents(body)
	if body == nil {
		return s, fmt.Errorf("invalid list encoding")
	}
	var err error
	s.tp, err = plan.DecodeTransport(st, body)
	if err != nil {
		return s, err
	}
	body = body[ion.SizeOf(body):]
	_, err = ion.UnpackList(body, func(body []byte) error {
		n, _, err := ion.ReadInt(body)
		if err != nil {
			return err
		}
		s.blobs = append(s.blobs, int(n))
		return nil
	})
	return s, err
}

// A tableHandleFn is used to produce a TableHandle
// from a list of blobs and a filter.
type tableHandleFn func(blobs []blob.Interface, flt expr.Node) plan.TableHandle

// subtables is the plan.Subtables implementation
// returned by (*splitter).Split.
type subtables struct {
	splits []split
	table  expr.Node
	blobs  []blob.Interface
	filter expr.Node
	next   *subtables // set if combined

	// fn is called to produce the TableHandles
	// embedded in the subtables
	fn tableHandleFn
}

// Len implements plan.Subtables.Len.
func (s *subtables) Len() int {
	n := len(s.splits)
	if s.next != nil {
		n += s.next.Len()
	}
	return n
}

// Subtable implements plan.Subtables.Subtable.
func (s *subtables) Subtable(i int, sub *plan.Subtable) {
	if s.next != nil && i >= len(s.splits) {
		s.next.Subtable(i-len(s.splits), sub)
		return
	}
	sp := &s.splits[i]
	name := fmt.Sprintf("part.%d", i)
	table := &expr.Table{
		Binding: expr.Bind(s.table, name),
	}
	blobs := make([]blob.Interface, len(sp.blobs))
	for i, bi := range sp.blobs {
		blobs[i] = s.blobs[bi]
	}
	*sub = plan.Subtable{
		Transport: sp.tp,
		Table:     table,
		Handle:    s.fn(blobs, s.filter),
	}
}

// Encode implements plan.Subtables.Encode.
func (s *subtables) Encode(st *ion.Symtab, dst *ion.Buffer) error {
	// encode as [splits, table, blobs, filter, next]
	dst.BeginList(-1)
	dst.BeginList(-1)
	for i := range s.splits {
		if err := s.splits[i].encode(st, dst); err != nil {
			return err
		}
	}
	dst.EndList()
	s.table.Encode(dst, st)
	lst := blob.List{Contents: s.blobs}
	lst.Encode(dst, st)
	if s.filter == nil {
		dst.WriteNull()
	} else {
		s.filter.Encode(dst, st)
	}
	if s.next == nil {
		dst.WriteNull()
	} else if err := s.next.Encode(st, dst); err != nil {
		return err
	}
	dst.EndList()
	return nil
}

func decodeSubtables(st *ion.Symtab, body []byte, fn tableHandleFn) (*subtables, error) {
	if ion.TypeOf(body) != ion.ListType {
		return nil, fmt.Errorf("expected a list; found ion type %s", ion.TypeOf(body))
	}
	body, _ = ion.Contents(body)
	if body == nil {
		return nil, fmt.Errorf("invalid list encoding")
	}
	s := &subtables{fn: fn}
	body, err := ion.UnpackList(body, func(body []byte) error {
		sp, err := decodeSplit(st, body)
		if err != nil {
			return err
		}
		s.splits = append(s.splits, sp)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.table, body, err = expr.Decode(st, body)
	if err != nil {
		return nil, err
	}
	lst, err := blob.DecodeList(st, body)
	if err != nil {
		return nil, err
	}
	s.blobs = lst.Contents
	body = body[ion.SizeOf(body):]
	if ion.TypeOf(body) != ion.NullType {
		s.filter, body, err = expr.Decode(st, body)
		if err != nil {
			return nil, err
		}
	} else {
		body = body[ion.SizeOf(body):]
	}
	if ion.TypeOf(body) != ion.NullType {
		s.next, err = decodeSubtables(st, body, fn)
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Filter implements plan.Subtables.Filter.
func (s *subtables) Filter(e expr.Node) {
	s.filter = e
	if s.next != nil {
		s.next.Filter(e)
	}
}

// Append implements plan.Subtables.Append.
func (s *subtables) Append(sub plan.Subtables) plan.Subtables {
	end := s
	for end.next != nil {
		end = end.next
	}
	end.next = sub.(*subtables)
	return s
}
