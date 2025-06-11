package bsupio

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/brimdata/super"
	"github.com/brimdata/super/pkg/peeker"
	"github.com/brimdata/super/runtime/sam/expr"
	"github.com/brimdata/super/runtime/sam/op"
	"github.com/brimdata/super/zbuf"
	"github.com/brimdata/super/zcode"
)

type scanner struct {
	ctx        context.Context
	cancel     context.CancelFunc
	parser     parser
	progress   zbuf.Progress
	validate   bool
	once       sync.Once
	workers    []*worker
	workerCh   chan *worker
	resultChCh chan chan op.Result
	err        error
	eof        bool
}

func newScanner(ctx context.Context, sctx *super.Context, r io.Reader, pushdown zbuf.Pushdown, opts ReaderOpts) (zbuf.Scanner, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &scanner{
		ctx:    ctx,
		cancel: cancel,
		parser: parser{
			peeker:  peeker.NewReader(r, opts.Size, opts.Max),
			types:   NewDecoder(sctx),
			maxSize: opts.Max,
		},
		validate:   opts.Validate,
		workerCh:   make(chan *worker, opts.Threads+1),
		resultChCh: make(chan chan op.Result, opts.Threads+1),
	}
	for range opts.Threads {
		var bf *expr.BufferFilter
		var f expr.Evaluator
		if pushdown != nil {
			var err error
			bf, err = pushdown.BSUPFilter()
			if err != nil {
				return nil, err
			}
			f, err = pushdown.DataFilter()
			if err != nil {
				return nil, err
			}
		}
		s.workers = append(s.workers, newWorker(ctx, &s.progress, bf, f, s.validate))
	}
	return s, nil
}

func (s *scanner) Pull(done bool) (zbuf.Batch, error) {
	s.once.Do(s.start)
	if done {
		s.cancel()
		for range s.resultChCh {
			// Wait for the s.parser goroutine to exit so we know it
			// won't continue reading from the underlying io.Reader.
		}
		s.eof = true
		return nil, nil
	}
	if s.err != nil || s.eof {
		return nil, s.err
	}
	for {
		select {
		case ch := <-s.resultChCh:
			result, ok := <-ch
			if !ok {
				continue
			}
			if result.Batch == nil || result.Err != nil {
				if _, ok := result.Err.(*zbuf.Control); !ok {
					s.eof = true
					s.err = result.Err
					s.cancel()
				}
			}
			return result.Batch, result.Err
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		}
	}
}

func (s *scanner) start() {
	for _, w := range s.workers {
		go w.run(s.workerCh)
	}
	go func() {
		defer close(s.resultChCh)
		// This is the input goroutine that reads message blocks
		// from the input.  Types and control messages are decoded
		// in this thread and data blocks are distributed to the workers
		// with the property that all types for a given data block will
		// exist in the decoder types state (which in turn points to the
		// shared query context) before the worker is given the buffer
		// to (optionally) uncompress, filter, and decode when matched.
		// When we hit end-of-stream, a new type context and types slice are
		// created for the new data batches.  Since all data is mapped to
		// the shared context and each worker maps its values independently,
		// the decode pipeline continues to operate concurrenlty without
		// any problem even when faced with changing localized type state.
		for {
			frame, err := s.parser.read()
			if err != nil {
				if _, ok := err.(*zbuf.Control); ok {
					s.sendControl(err)
					continue
				}
				if err == io.EOF {
					err = nil
				}
				s.sendControl(err)
				return
			}
			// Grab a free worker and give it this values message frame to work on
			// along with the present Decoder's local-to-shared type state.
			// We queue up the worker's resultCh so batches are delivered in order.
			select {
			case worker := <-s.workerCh:
				w := work{
					types:    s.parser.types,
					frame:    frame,
					resultCh: make(chan op.Result, 1),
				}
				select {
				case s.resultChCh <- w.resultCh:
					select {
					case worker.workCh <- w:
					case <-s.ctx.Done():
						close(w.resultCh)
						return
					}
				case <-s.ctx.Done():
					return
				}
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

// sendControl provides a means for the input thread to send control
// messages and error/EOF in order with the worker threads.
func (s *scanner) sendControl(err error) bool {
	ch := make(chan op.Result, 1)
	ch <- op.Result{Err: err}
	select {
	case s.resultChCh <- ch:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *scanner) Progress() zbuf.Progress {
	return s.progress.Copy()
}

// worker is used by both the non-threaded synchronous scanner as well as
// the threaded scanner.  As long as run() is not called, scanBatch() can
// be safely used without any channel involvement.
type worker struct {
	ctx          context.Context
	progress     *zbuf.Progress
	workCh       chan work
	bufferFilter *expr.BufferFilter
	filter       expr.Evaluator
	ectx         expr.Context
	validate     bool

	typeCache super.TypeCache
	vals      []super.Value
}

type work struct {
	// Workers need access to the local Decoder types slice to map deserialized
	// type IDs into shared-context types and bufferfilter needs to map local IDs
	// to field names and we accomplish both goals using the Decoder's implementation
	// of the super.TypeFetcher interface.
	types    super.TypeFetcher
	frame    frame
	resultCh chan op.Result
}

func newWorker(ctx context.Context, p *zbuf.Progress, bf *expr.BufferFilter, f expr.Evaluator, validate bool) *worker {
	return &worker{
		ctx:          ctx,
		progress:     p,
		workCh:       make(chan work),
		bufferFilter: bf,
		filter:       f,
		ectx:         expr.NewContext(),
		validate:     validate,
	}
}

func (w *worker) run(workerCh chan<- *worker) {
	for {
		// Tell the scanner we're ready for work.
		select {
		case workerCh <- w:
		case <-w.ctx.Done():
			return
		}
		// Grab the work the scanner gave us.  The scanner will arrange
		// to pull the result off our resultCh and preserve order.
		select {
		case work := <-w.workCh:
			// If the buffer is compressed, decompress it.
			// If not, it wasn't compressed in the original data
			// stream and we handle both cases the same from
			// here on out  The important bit is we are doing
			// the decompress and the boyer-moore short-circuit
			// scan on a processor cache-friendly buffer and
			// throwing it all out asap if it is not needed.
			if work.frame.zbuf != nil {
				if err := work.frame.decompress(); err != nil {
					work.resultCh <- op.Result{Err: err}
					continue
				}
				work.frame.zbuf.free()
			}
			// Either the frame was compressed or it was uncompressed.
			// In either case,the uncompressed data is now in work.blk.
			// We hand ownership of ubuf over to scanBatch.  the zbuf
			// has been freed above so no need to free work.blk.
			// If the batch survives, the work.blk.ubuf will go with it
			// and will get freed when the batch's Unref count hits 0.
			batch, err := w.scanBatch(work.frame.ubuf, work.types)
			if batch != nil || err != nil {
				work.resultCh <- op.Result{Batch: batch, Err: err}
			}
			close(work.resultCh)
		case <-w.ctx.Done():
			return
		}
	}
}

func (w *worker) scanBatch(buf *buffer, types super.TypeFetcher) (zbuf.Batch, error) {
	// If w.bufferFilter evaluates to false, we know buf cannot contain
	// values matching w.filter.
	if w.bufferFilter != nil && !w.bufferFilter.Eval(types, buf.Bytes()) {
		atomic.AddInt64(&w.progress.BytesRead, int64(buf.length()))
		buf.free()
		return nil, nil
	}
	// Otherwise, build a batch by reading all values in the buffer.
	w.typeCache.Reset(types)
	w.vals = w.vals[:0]
	var progress zbuf.Progress
	for buf.length() > 0 {
		var val super.Value
		if err := w.decodeVal(buf, &val); err != nil {
			buf.free()
			return nil, err
		}
		if w.wantValue(val, &progress) {
			w.vals = append(w.vals, val)
		}
	}
	w.progress.Add(progress)
	if len(w.vals) == 0 {
		buf.free()
		return nil, nil
	}
	return newBatch(buf, w.vals), nil
}

func (w *worker) decodeVal(buf *buffer, valRef *super.Value) error {
	id, err := readUvarintAsInt(buf)
	if err != nil {
		return err
	}
	n, err := zcode.ReadTag(buf)
	if err != nil {
		return errBadFormat
	}
	var b []byte
	if n == 0 {
		b = []byte{}
	} else if n > 0 {
		b, err = buf.read(n)
		if err != nil && err != io.EOF {
			if err == peeker.ErrBufferOverflow {
				return fmt.Errorf("large value of %d bytes exceeds maximum read buffer", n)
			}
			return errBadFormat
		}
	}
	typ, err := w.typeCache.LookupType(id)
	if err != nil {
		return fmt.Errorf("bsupio: %w", err)
	}
	*valRef = super.NewValue(typ, b)
	if w.validate {
		if err := valRef.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (w *worker) wantValue(val super.Value, progress *zbuf.Progress) bool {
	progress.BytesRead += int64(len(val.Bytes()))
	progress.RecordsRead++
	// It's tempting to call w.bufferFilter.Eval on rec.Bytes here, but that
	// might call FieldNameFinder.Find, which could explode or return false
	// negatives because it expects a buffer of BSON value messages, and
	// rec.Bytes is just a BSON value.  (A BSON value message is a header
	// indicating a type ID followed by a value of that type.)
	if w.filter == nil || check(w.ectx, val, w.filter) {
		progress.BytesMatched += int64(len(val.Bytes()))
		progress.RecordsMatched++
		return true
	}
	return false
}

func check(ectx expr.Context, this super.Value, filter expr.Evaluator) bool {
	val := filter.Eval(ectx, this)
	return val.Type() == super.TypeBool && val.Bool()
}
