package pawn

import (
	"math"
	"strconv"
)

func mathPow(a, b float64) float64 { return math.Pow(a, b) }

func formatNumber(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// PopCount64 暴露给统计/校验代码，保持位掩码统计一致。
func PopCount64(x uint64) int {
	n := 0
	for x != 0 {
		x &= x - 1
		n++
	}
	return n
}
