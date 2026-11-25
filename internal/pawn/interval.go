package pawn

import "math"

type interval struct {
	low, high       float64
	lowInc, highInc bool
}

func (iv interval) contains(x float64) bool {
	if iv.lowInc {
		if x < iv.low {
			return false
		}
	} else if x <= iv.low {
		return false
	}
	if iv.highInc {
		if x > iv.high {
			return false
		}
	} else if x >= iv.high {
		return false
	}
	return true
}

type iNode struct {
	id       int32
	iv       interval
	priority uint64
	left     *iNode
	right    *iNode

	maxHigh    float64
	maxHighInc bool
}

type intervalTree struct {
	root *iNode
}

func newIntervalTree() *intervalTree { return &intervalTree{} }

func (t *intervalTree) Insert(id int32, iv interval) {
	n := &iNode{id: id, iv: iv, priority: stablePriority(id, ivKey(iv))}
	t.root = insertINode(t.root, n)
}

func ivKey(iv interval) string {
	// 同一张表内 handle 已足够分散；salt 只需避免不同 Matcher 形态完全相关。
	return "interval"
}

func insertINode(root, n *iNode) *iNode {
	if root == nil {
		recalcINode(n)
		return n
	}
	if lessIntervalKey(n.iv.low, n.id, root.iv.low, root.id) {
		root.left = insertINode(root.left, n)
		if root.left.priority > root.priority {
			root = rotateRight(root)
		}
	} else {
		root.right = insertINode(root.right, n)
		if root.right.priority > root.priority {
			root = rotateLeft(root)
		}
	}
	recalcINode(root)
	return root
}

func lessIntervalKey(aLow float64, aID int32, bLow float64, bID int32) bool {
	if aLow != bLow {
		return aLow < bLow
	}
	return aID < bID
}

func rotateLeft(x *iNode) *iNode {
	y := x.right
	x.right = y.left
	y.left = x
	recalcINode(x)
	recalcINode(y)
	return y
}

func rotateRight(x *iNode) *iNode {
	y := x.left
	x.left = y.right
	y.right = x
	recalcINode(x)
	recalcINode(y)
	return y
}

func recalcINode(n *iNode) {
	n.maxHigh = n.iv.high
	n.maxHighInc = n.iv.highInc
	if n.left != nil {
		mergeMaxHigh(n.left.maxHigh, n.left.maxHighInc, &n.maxHigh, &n.maxHighInc)
	}
	if n.right != nil {
		mergeMaxHigh(n.right.maxHigh, n.right.maxHighInc, &n.maxHigh, &n.maxHighInc)
	}
}

func mergeMaxHigh(h float64, inc bool, maxHigh *float64, maxInc *bool) {
	if h > *maxHigh {
		*maxHigh = h
		*maxInc = inc
	} else if h == *maxHigh && inc {
		*maxInc = true
	}
}

func subtreeMayContain(n *iNode, x float64) bool {
	if n == nil {
		return false
	}
	if n.maxHigh < x {
		return false
	}
	if n.maxHigh == x && !n.maxHighInc {
		// 子树里最高的右端点在 x 处是开区间；更低的右端点不可能包含 x。
		return false
	}
	return true
}

// Query 按点查询所有包含 x 的区间。输出集合的复杂度通常为 O(log n + k)，
// 极端全匹配场景下 k 本身就是结果大小。
func (t *intervalTree) Query(x float64, visit func(int32)) {
	queryINode(t.root, x, visit)
}

func queryINode(n *iNode, x float64, visit func(int32)) {
	if n == nil || !subtreeMayContain(n, x) {
		return
	}

	// 左子树的 low 都不大于当前 low，但仍需用 maxHigh 剪枝。
	queryINode(n.left, x, visit)

	if n.iv.contains(x) {
		visit(n.id)
	}

	// 只有当前 low <= x 时，右子树才可能存在 low <= x 的节点。
	if n.iv.low < x || (n.iv.low == x) {
		queryINode(n.right, x, visit)
	}
}

var _ = math.Inf
