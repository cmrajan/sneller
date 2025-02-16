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

package blockfmt

import (
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/SnellerInc/sneller/compr"
	"github.com/SnellerInc/sneller/ion"
	"github.com/SnellerInc/sneller/jsonrl"
	"github.com/klauspost/compress/zstd"
)

// we try to keep this many bytes in-flight
// at all times, regardless of the level of
// parallelism we are using to ingest files
const wantInflight = 80 * 1024 * 1024

// RowFormat is the interface through which
// input streams are converted into aligned
// output blocks.
type RowFormat interface {
	Convert(r io.Reader, dst *ion.Chunker) error
	// Name is the name of the format
	// that will be included in an index description.
	Name() string
	UseHints(schema []byte) error
}

// Input is a combination of
// an input stream and a row-formatting function.
// Together they produce output blocks.
type Input struct {
	// Path and ETag are used to
	// populate the ObjectInfo
	// in an Index built from a Converter.
	Path, ETag string
	// Size is the size of the input, in bytes
	Size int64
	// R is the source of unformatted data
	R io.ReadCloser
	// F is the formatter that produces output blocks
	F RowFormat
	// Err is an error specific
	// to this input that is populated
	// by Converter.Run.
	Err error
}

type jsonConverter struct {
	decomp       func(r io.Reader) (io.Reader, error)
	compname     string
	hints        *jsonrl.Hint
	isCloudtrail bool
}

func (j *jsonConverter) Name() string {
	if j.compname == "" {
		return "json"
	}
	return "json." + j.compname
}

func (j *jsonConverter) Convert(r io.Reader, dst *ion.Chunker) error {
	rc := r
	var err, err2 error
	if j.decomp != nil {
		rc, err = j.decomp(r)
		if err != nil {
			return err
		}
	}
	if j.isCloudtrail {
		err = jsonrl.ConvertCloudtrail(rc, dst)
	} else {
		err = jsonrl.Convert(rc, dst, j.hints)
	}
	if j.decomp != nil {
		// if the decompressor (i.e. gzip.Reader)
		// has a Close() method, then use that;
		// this lets us check the integrity of
		// gzip checksums, etc.
		if cc, ok := rc.(io.Closer); ok {
			err2 = cc.Close()
		}
	}
	if err == nil {
		err = err2
	}
	return err
}

func (j *jsonConverter) UseHints(hints []byte) error {
	if j.isCloudtrail && hints != nil {
		return fmt.Errorf("cloudtrail.json.gz format does not accept hints")
	}
	if hints == nil {
		j.hints = nil
		return nil
	}
	s, err := jsonrl.ParseHint(hints)
	if err != nil {
		return err
	}
	j.hints = s
	return nil
}

type ionConverter struct{}

func (i ionConverter) Name() string { return "ion" }

func (i ionConverter) Convert(r io.Reader, dst *ion.Chunker) error {
	_, err := dst.ReadFrom(r)
	if err != nil {
		return fmt.Errorf("converting UnsafeION: %w", err)
	}
	return nil
}

func (i ionConverter) UseHints(hints []byte) error {
	return nil
}

// UnsafeION converts raw ion by
// decoding and re-encoding it.
//
// NOTE: UnsafeION is called UnsafeION
// because the ion package has not been
// hardened against arbitrary user input.
// FIXME: harden the ion package against
// malicious input and then rename this
// to something else.
func UnsafeION() RowFormat {
	return ionConverter{}
}

// SuffixToFormat is a list of known
// filename suffixes that correspond
// to known constructors for RowFormat
// objects.
var SuffixToFormat = map[string]func() RowFormat{
	".json": func() RowFormat {
		return &jsonConverter{decomp: nil}
	},
	".json.zst": func() RowFormat {
		return &jsonConverter{
			decomp: func(r io.Reader) (io.Reader, error) {
				return zstd.NewReader(r)
			},
			compname: "zst",
		}
	},
	".json.gz": func() RowFormat {
		return &jsonConverter{
			decomp: func(r io.Reader) (io.Reader, error) {
				return gzip.NewReader(r)
			},
			compname: "gz",
		}
	},
}

// CloudtrailJSON produces the RowFormat associated
// with parsing AWS Cloudtrail data compressed with
// the given compression name, which should be one
// of ".gz" ".zst" or "" (none). (The only compression
// used in practice by AWS is ".gz")
func CloudtrailJSON(compression string) RowFormat {
	switch compression {
	case "":
		return &jsonConverter{
			isCloudtrail: true,
		}
	case ".gz":
		return &jsonConverter{
			decomp: func(r io.Reader) (io.Reader, error) {
				return gzip.NewReader(r)
			},
			compname:     "gz",
			isCloudtrail: true,
		}
	case ".zst":
		return &jsonConverter{
			decomp: func(r io.Reader) (io.Reader, error) {
				return zstd.NewReader(r)
			},
			compname:     "zst",
			isCloudtrail: true,
		}
	default:
		panic("bad compression passed to blockfmt.CloudtrailJSON")
	}
}

// Converter performs single- or
// multi-stream conversion of a list of inputs
// in parallel.
type Converter struct {
	// Prepend, if R is not nil,
	// is a blockfmt-formatted stream
	// of data to prepend to the output stream.
	Prepend struct {
		R       io.ReadCloser
		Trailer *Trailer
	}
	// Inputs is the list of input
	// streams that need to be converted
	// into the output format.
	Inputs []Input
	// Output is the Uploader to which
	// data will be written. The Uploader
	// will be wrapped in a CompressionWriter
	// or MultiWriter depending on the number
	// of input streams and the parallelism setting.
	Output Uploader
	// Comp is the name of the compression
	// algorithm used for uploaded data blocks.
	Comp string
	// Align is the pre-compression alignment
	// of chunks written to the uploader.
	Align int
	// FlushMeta is the maximum interval
	// at which metadata is flushed.
	// Note that metadata may be flushed
	// below this interval if there is not
	// enough input data to make the intervals this wide.
	FlushMeta int
	// TargetSize is the target size of
	// chunks written to the Uploader.
	TargetSize int
	// Parallel is the maximum parallelism of
	// uploads. If Parallel is <= 0, then
	// GOMAXPROCS is used instead.
	Parallel int
	// DisablePrefetch, if true, disables
	// prefetching of inputs.
	DisablePrefetch bool

	// trailer built by the writer. This is only
	// set if the object was written successfully.
	trailer *Trailer
}

// static errors known to be fatal to decoding
var isFatal = []error{
	jsonrl.ErrNoMatch,
	jsonrl.ErrTooLarge,
	ion.ErrTooLarge,
	gzip.ErrHeader,
	zstd.ErrReservedBlockType,
	zstd.ErrMagicMismatch,
	zstd.ErrUnknownDictionary,
	zstd.ErrWindowSizeExceeded,
	zstd.ErrWindowSizeTooSmall,
	zstd.ErrBlockTooSmall,

	// TODO: ion errors from transcoding?
}

// IsFatal returns true if the error
// is an error known to be fatal when
// returned from blockfmt.Format.Convert.
// (A fatal error is one that will not
// disappear on a retry.)
func IsFatal(err error) bool {
	for i := range isFatal {
		if errors.Is(err, isFatal[i]) {
			return true
		}
	}
	var cie flate.CorruptInputError
	return errors.As(err, &cie)
}

// MultiStream returns whether the configuration of Converter
// would lead to a multi-stream upload.
func (c *Converter) MultiStream() bool {
	return len(c.Inputs) > 1 && (c.Parallel <= 0 || c.Parallel > 1)
}

// Run runs the conversion operation
// and returns the first error it ecounters.
// Additionally, it will populate c.Inputs[*].Err
// with any errors associated with the inputs.
// Note that Run stops at the first encountered
// error, so if one of the Inputs has Err set,
// then subsequent items in Inputs may not
// have been processed at all.
func (c *Converter) Run() error {
	if len(c.Inputs) == 0 {
		return errors.New("no inputs or merge sources")
	}
	if len(c.Inputs) == 0 {
		// proxy uploader is never closed by `runSingle()` or `runMulti()`
		// in this case -> finish uploading the uncommitted parts + trailer
		return c.Output.Close(nil)
	}
	if c.MultiStream() {
		return c.runMulti()
	}
	return c.runSingle()
}

func (c *Converter) runSingle() error {
	comp := compr.Compression(c.Comp)
	if comp == nil {
		return fmt.Errorf("compression %q unavailable", c.Comp)
	}
	w := &CompressionWriter{
		Output:     c.Output,
		Comp:       comp,
		InputAlign: c.Align,
		TargetSize: c.TargetSize,
		// try to make the blocks at least
		// half the target size
		MinChunksPerBlock: c.FlushMeta / (c.Align * 2),
	}
	cn := ion.Chunker{
		W:          w,
		Align:      w.InputAlign,
		RangeAlign: c.FlushMeta,
	}
	err := c.runPrepend(&cn)
	if err != nil {
		return err
	}
	ready := make([]chan struct{}, len(c.Inputs))
	next := 1
	inflight := int64(0) // # bytes being prefetched
	for i := range c.Inputs {
		// make sure that prefetching has completed
		// on this entry if we had queued it up
		var saved chan struct{}
		if ready[i] != nil {
			<-ready[i]
			saved, ready[i] = ready[i], nil
			inflight -= c.Inputs[i].Size
		}
		// fast-forward the prefetch pointer
		// if we had a run of large files
		if next <= i {
			next = i + 1
		}
		// start readahead on inputs that we will need
		for !c.DisablePrefetch && inflight < wantInflight && (next-i) < 64 && next < len(c.Inputs) {
			if saved != nil {
				ready[next] = saved
				saved = nil
			} else {
				ready[next] = make(chan struct{}, 1)
			}
			go func(r io.Reader, done chan struct{}) {
				r.Read([]byte{})
				done <- struct{}{}
			}(c.Inputs[next].R, ready[next])
			inflight += c.Inputs[next].Size
			next++
		}

		err := c.Inputs[i].F.Convert(c.Inputs[i].R, &cn)
		err2 := c.Inputs[i].R.Close()
		if err == nil {
			err = err2
		}
		if err != nil {
			// wait for prefetching to stop
			for _, c := range ready[i:next] {
				if c != nil {
					<-c
				}
			}
			// close everything we haven't already closed
			tail := c.Inputs[i+1:]
			for j := range tail {
				tail[j].R.Close()
			}
			c.Inputs[i].Err = err
			return err
		}
	}
	err = cn.Flush()
	if err != nil {
		return err
	}
	err = w.Close()
	c.trailer = &w.Trailer
	return err
}

func (c *Converter) runPrepend(cn *ion.Chunker) error {
	if c.Prepend.R == nil {
		return nil
	}
	cn.WalkTimeRanges = collectRanges(c.Prepend.Trailer)
	d := Decoder{}
	d.Set(c.Prepend.Trailer, 0)
	_, err := d.Copy(cn, c.Prepend.R)
	c.Prepend.R.Close()
	cn.WalkTimeRanges = nil
	return err
}

func (c *Converter) runMulti() error {
	comp := compr.Compression(c.Comp)
	if comp == nil {
		return fmt.Errorf("compression %q unavailable", c.Comp)
	}
	w := &MultiWriter{
		Output:     c.Output,
		Comp:       comp,
		InputAlign: c.Align,
		TargetSize: c.TargetSize,
		// try to make the blocks at least
		// half the target size
		MinChunksPerBlock: c.FlushMeta / (c.Align * 2),
	}
	p := c.Parallel
	if p <= 0 {
		p = runtime.GOMAXPROCS(0)
	}
	startc := make(chan *Input, p)
	readyc := startc
	if p >= len(c.Inputs) {
		p = len(c.Inputs)
	} else if !c.DisablePrefetch {
		max := 64
		if max > len(c.Inputs) {
			max = len(c.Inputs)
		}
		readyc = doPrefetch(startc, max, wantInflight)
	}
	errs := make(chan error, p)
	consume := func(in chan *Input) {
		for in := range in {
			in.R.Close()
		}
	}
	for i := 0; i < p; i++ {
		wc, err := w.Open()
		if err != nil {
			close(readyc)
			return err
		}
		go func(i int) {
			// if we encounter an error,
			// drain the queue and close each item
			defer consume(startc)
			cn := ion.Chunker{
				W:          wc,
				Align:      w.InputAlign,
				RangeAlign: c.FlushMeta,
			}
			if i == 0 {
				err := c.runPrepend(&cn)
				if err != nil {
					errs <- fmt.Errorf("prepend: %w", err)
					return
				}
			}
			for in := range startc {
				err := in.F.Convert(in.R, &cn)
				err2 := in.R.Close()
				if err == nil {
					err = err2
				}
				if err != nil {
					in.Err = err
					errs <- fmt.Errorf("%s: %w", in.Path, err)
					return
				}
			}
			err := cn.Flush()
			if err != nil {
				errs <- err
				return
			}
			errs <- wc.Close()
		}(i)
	}
	for i := range c.Inputs {
		readyc <- &c.Inputs[i]
	}
	// will cause readyc to be closed
	// when the queue has been drained:
	close(readyc)
	var extra int
	var outerr error
	for i := 0; i < p; i++ {
		err := <-errs
		if outerr == nil {
			outerr = err
		} else {
			extra++
		}
	}
	if outerr != nil {
		if extra > 0 {
			return fmt.Errorf("%w (and %d other errors)", outerr, extra)
		}
		return outerr
	}
	// don't finalize unless everything
	// up to this point succeeded
	if err := w.Close(); err != nil {
		return err
	}
	c.trailer = &w.Trailer
	return nil
}

func (c *Converter) Trailer() *Trailer {
	return c.trailer
}
