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

//go:build none
// +build none

// This is a fake tenant process
// that we are using for testing.

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	"github.com/SnellerInc/sneller/db"
	"github.com/SnellerInc/sneller/ion"
	"github.com/SnellerInc/sneller/plan"
	"github.com/SnellerInc/sneller/tenant/dcache"
	"github.com/SnellerInc/sneller/tenant/tnproto"
	"github.com/SnellerInc/sneller/vm"
)

type Env struct {
	cache   *dcache.Cache
	eventfd *os.File
	evbuf   [8]byte
}

func (e *Env) post() {
	binary.LittleEndian.PutUint64(e.evbuf[:], 1)
	e.eventfd.Write(e.evbuf[:])
}

var _ plan.UploaderDecoder = (*Env)(nil)

func (e *Env) DecodeUploader(st *ion.Symtab, buf []byte) (plan.UploadFS, error) {
	return db.DecodeDirFS(st, buf)
}

// handle implements plan.Handle and cache.Segment
type Handle struct {
	filename string
	size     int64
	env      *Env
	repeat   int
}

func (h *Handle) Align() int { return 1024 * 1024 }

func (h *Handle) Size() int64 { return h.size * int64(h.repeat) }

func (h *Handle) ETag() string {
	w := sha256.New()
	io.WriteString(w, h.filename)
	io.WriteString(w, strconv.Itoa(h.repeat))
	return base64.URLEncoding.EncodeToString(w.Sum(nil))
}

type repeatReader struct {
	file  *os.File
	count int
}

func (r *repeatReader) Read(dst []byte) (int, error) {
	for {
		if r.count <= 0 {
			return 0, io.EOF
		}
		n, err := r.file.Read(dst)
		if n > 0 || err != io.EOF {
			return n, err
		}
		r.count--
		r.file.Seek(0, io.SeekStart)
	}
}

func (r *repeatReader) Close() error { return r.file.Close() }

func (h *Handle) Open() (io.ReadCloser, error) {
	f, err := os.Open(h.filename)
	if err != nil {
		return nil, err
	}
	return &repeatReader{
		file:  f,
		count: h.repeat,
	}, nil
}

func (h *Handle) Decode(dst io.Writer, src []byte) error {
	if h.Align() > vm.PageSize {
		return fmt.Errorf("align %d > vm.PageSize %d", h.Align(), vm.PageSize)
	}
	buf := vm.Malloc()
	defer vm.Free(buf)
	for off := int64(0); off < h.Size(); off += int64(h.Align()) {
		mem := src[off:]
		if len(mem) > h.Align() {
			mem = mem[:h.Align()]
		}
		_, err := dst.Write(buf[:copy(buf, mem)])
		if err != nil {
			return err
		}
	}
	return nil
}

type tableHandle Handle

func (t *tableHandle) Open() (vm.Table, error) {
	return t.env.cache.Table((*Handle)(t), 0), nil
}

func (t *tableHandle) Encode(dst *ion.Buffer, st *ion.Symtab) error {
	dst.BeginStruct(-1)
	if t.repeat > 1 {
		dst.BeginField(st.Intern("repeat"))
		dst.WriteInt(int64(t.repeat))
	}
	dst.BeginField(st.Intern("filename"))
	dst.WriteString(t.filename)
	dst.EndStruct()
	return nil
}

func (e *Env) DecodeHandle(st *ion.Symtab, buf []byte) (plan.TableHandle, error) {
	if len(buf) == 0 {
		return nil, fmt.Errorf("no TableHandle present")
	}
	th := &tableHandle{
		env:    e,
		repeat: 1,
	}
	_, err := ion.UnpackStruct(st, buf, func(name string, field []byte) error {
		switch name {
		case "repeat":
			n, _, err := ion.ReadInt(field)
			if err != nil {
				return err
			}
			th.repeat = int(n)
		case "filename":
			str, _, err := ion.ReadString(field)
			if err != nil {
				return err
			}
			fi, err := os.Stat(str)
			if err != nil {
				return err
			}
			th.filename = str
			th.size = fi.Size()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return th, nil
}

func die(err error) {
	fmt.Fprintln(os.Stderr, err.Error())
	os.Exit(111)
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "worker" {
		die(errors.New("expected to run in worker mode"))
	}

	workerCmd := flag.NewFlagSet("worker", flag.ExitOnError)
	workerTenant := workerCmd.String("t", "", "tenant identifier")
	workerControlSocket := workerCmd.Int("c", -1, "control socket")
	eventfd := workerCmd.Int("e", -1, "eventfd")
	if workerCmd.Parse(os.Args[2:]) != nil {
		die(errors.New("invalid arguments passed to stub"))
	}

	if *workerControlSocket == -1 {
		die(errors.New("no control socket file descriptor"))
	}
	if *eventfd == -1 {
		die(errors.New("no eventfd file descriptor"))
	}

	_ = *workerTenant

	f := os.NewFile(uintptr(*workerControlSocket), "<ctlsock>")
	conn, err := net.FileConn(f)
	if err != nil {
		die(err)
	}
	f.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		die(fmt.Errorf("unexpected fd type %T", conn))
	}

	evfd := os.NewFile(uintptr(*eventfd), "<eventfd>")

	cachedir := os.Getenv("CACHEDIR")
	if cachedir == "" {
		die(errors.New("no CACHEDIR variable set"))
	}

	defer uc.Close()
	env := Env{eventfd: evfd}
	env.cache = dcache.New(cachedir, env.post)
	err = tnproto.Serve(uc, &env)
	if err != nil {
		die(err)
	}
}
