package render

import "golang.org/x/image/math/fixed"

func fixed266(v float64) fixed.Int26_6 {
	return fixed.Int26_6(int64(v) << 6)
}
