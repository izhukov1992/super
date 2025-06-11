package expr

import (
	"github.com/RoaringBitmap/roaring"
	"github.com/brimdata/super"
	"github.com/brimdata/super/vector"
	"github.com/brimdata/super/vector/bitvec"
)

type conditional struct {
	sctx      *super.Context
	predicate Evaluator
	thenExpr  Evaluator
	elseExpr  Evaluator
}

func NewConditional(sctx *super.Context, predicate, thenExpr, elseExpr Evaluator) Evaluator {
	return &conditional{
		sctx:      sctx,
		predicate: predicate,
		thenExpr:  thenExpr,
		elseExpr:  elseExpr,
	}
}

func (c *conditional) Eval(this vector.Any) vector.Any {
	predVec := c.predicate.Eval(this)
	boolsMap, errsMap := BoolMask(predVec)
	if errsMap.GetCardinality() == uint64(this.Len()) {
		return c.predicateError(predVec)
	}
	if boolsMap.GetCardinality() == uint64(this.Len()) {
		return c.thenExpr.Eval(this)
	}
	if boolsMap.IsEmpty() && errsMap.IsEmpty() {
		return c.elseExpr.Eval(this)
	}
	thenVec := c.thenExpr.Eval(vector.Pick(this, boolsMap.ToArray()))
	// elseMap is the difference between boolsMap or errsMap
	elseMap := roaring.Or(boolsMap, errsMap)
	elseMap.Flip(0, uint64(this.Len()))
	elseIndex := elseMap.ToArray()
	elseVec := c.elseExpr.Eval(vector.Pick(this, elseIndex))
	tags := make([]uint32, this.Len())
	for _, idx := range elseIndex {
		tags[idx] = 1
	}
	vecs := []vector.Any{thenVec, elseVec}
	if !errsMap.IsEmpty() {
		errsIndex := errsMap.ToArray()
		for _, idx := range errsIndex {
			tags[idx] = 2
		}
		vecs = append(vecs, c.predicateError(vector.Pick(predVec, errsIndex)))
	}
	return vector.NewDynamic(tags, vecs)
}

func (c *conditional) predicateError(vec vector.Any) vector.Any {
	return vector.Apply(false, func(vecs ...vector.Any) vector.Any {
		return vector.NewWrappedError(c.sctx, "?-operator: bool predicate required", vecs[0])
	}, vec)
}

func BoolMask(mask vector.Any) (*roaring.Bitmap, *roaring.Bitmap) {
	bools := roaring.New()
	errs := roaring.New()
	if dynamic, ok := mask.(*vector.Dynamic); ok {
		reverse := dynamic.ReverseTagMap()
		for i, val := range dynamic.Values {
			boolMaskRidx(reverse[i], bools, errs, val)
		}
	} else {
		boolMaskRidx(nil, bools, errs, mask)
	}
	return bools, errs
}

func boolMaskRidx(ridx []uint32, bools, errs *roaring.Bitmap, vec vector.Any) {
	switch vec := vec.(type) {
	case *vector.Const:
		if !vec.Value().Ptr().AsBool() {
			return
		}
		if !vec.Nulls.IsZero() {
			if ridx != nil {
				for i, idx := range ridx {
					if !vec.Nulls.IsSet(uint32(i)) {
						bools.Add(idx)
					}
				}
			} else {
				for i := range vec.Len() {
					if !vec.Nulls.IsSet(i) {
						bools.Add(i)
					}
				}
			}
		} else {
			if ridx != nil {
				bools.AddMany(ridx)
			} else {
				bools.AddRange(0, uint64(vec.Len()))
			}
		}
	case *vector.Bool:
		trues := vec.Bits
		if !vec.Nulls.IsZero() {
			// if null and true set to false
			trues = bitvec.And(trues, bitvec.Not(vec.Nulls))
		}
		if ridx != nil {
			for i, idx := range ridx {
				if trues.IsSetDirect(uint32(i)) {
					bools.Add(idx)
				}
			}
		} else {
			bools.Or(roaring.FromDense(trues.GetBits(), true))
		}
	case *vector.Error:
		if ridx != nil {
			errs.AddMany(ridx)
		} else {
			errs.AddRange(0, uint64(vec.Len()))
		}
	}
}
