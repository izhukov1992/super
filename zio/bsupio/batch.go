package bsupio

import (
	"sync"
	"sync/atomic"

	"github.com/brimdata/super"
	"github.com/brimdata/super/zbuf"
)

type batch struct {
	buf  *buffer
	refs int32
	vals []super.Value
}

var _ zbuf.Batch = (*batch)(nil)

var batchPool sync.Pool

// newBatch takes ownership of buf but not vals.
func newBatch(buf *buffer, vals []super.Value) *batch {
	b, ok := batchPool.Get().(*batch)
	if !ok {
		b = &batch{}
	}
	b.buf = buf
	b.refs = 1
	b.vals = append(b.vals[:0], vals...)
	return b
}

func (b *batch) Ref() { atomic.AddInt32(&b.refs, 1) }

func (b *batch) Unref() {
	if refs := atomic.AddInt32(&b.refs, -1); refs == 0 {
		if b.buf != nil {
			b.buf.free()
			b.buf = nil
		}
		batchPool.Put(b)
	} else if refs < 0 {
		panic("bsupio: negative batch reference count")
	}
}

func (b *batch) Values() []super.Value { return b.vals }

// XXX this should be ok, but we should handle nil receiver in scope so push
// will do the right thing
func (*batch) Vars() []super.Value { return nil }
