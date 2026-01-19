package orderbook

// Red-Black Tree Implementation
//
// A red-black tree is a self-balancing binary search tree that guarantees
// O(log n) operations for insert, delete, and search. This is used to
// maintain price levels in sorted order.
//
// Properties:
// 1. Every node is either red or black
// 2. The root is always black
// 3. Red nodes cannot have red children (no two reds in a row)
// 4. Every path from root to nil has the same number of black nodes
//
// Why Red-Black Tree for Order Books:
// - O(log n) insert/delete for adding/removing price levels
// - O(1) access to min/max (best bid/ask) with cached pointers
// - Ordered traversal for depth queries
// - Better worst-case than AVL for insert-heavy workloads

type color bool

const (
	red   color = true
	black color = false
)

// rbNode is a node in the red-black tree.
type rbNode struct {
	price  int64
	level  *PriceLevel
	color  color
	left   *rbNode
	right  *rbNode
	parent *rbNode
}

// RBTree is a red-black tree keyed by price.
type RBTree struct {
	root       *rbNode
	size       int
	minNode    *rbNode // Cached for O(1) access
	maxNode    *rbNode // Cached for O(1) access
	descending bool    // If true, "min" returns max (for bids)
}

// NewRBTree creates a new red-black tree.
// If descending is true, Min() returns the maximum value (useful for bids
// where "best" means highest price).
func NewRBTree(descending bool) *RBTree {
	return &RBTree{
		descending: descending,
	}
}

// Size returns the number of nodes in the tree.
func (t *RBTree) Size() int {
	return t.size
}

// IsEmpty returns true if the tree has no nodes.
func (t *RBTree) IsEmpty() bool {
	return t.size == 0
}

// Min returns the minimum price level (or maximum if descending).
// This is the "best" price for matching.
// Time complexity: O(1) due to caching.
func (t *RBTree) Min() *PriceLevel {
	if t.descending {
		if t.maxNode == nil {
			return nil
		}
		return t.maxNode.level
	}
	if t.minNode == nil {
		return nil
	}
	return t.minNode.level
}

// Get retrieves the price level at the given price.
// Time complexity: O(log n)
func (t *RBTree) Get(price int64) *PriceLevel {
	node := t.search(price)
	if node == nil {
		return nil
	}
	return node.level
}

// Insert adds a price level to the tree.
// Time complexity: O(log n)
func (t *RBTree) Insert(level *PriceLevel) {
	newNode := &rbNode{
		price: level.Price,
		level: level,
		color: red,
	}

	if t.root == nil {
		newNode.color = black
		t.root = newNode
		t.minNode = newNode
		t.maxNode = newNode
		t.size = 1
		return
	}

	// Standard BST insert
	var parent *rbNode
	current := t.root
	for current != nil {
		parent = current
		if level.Price < current.price {
			current = current.left
		} else if level.Price > current.price {
			current = current.right
		} else {
			// Price already exists, update level
			current.level = level
			return
		}
	}

	newNode.parent = parent
	if level.Price < parent.price {
		parent.left = newNode
	} else {
		parent.right = newNode
	}

	t.size++

	// Update min/max cache
	if t.minNode == nil || level.Price < t.minNode.price {
		t.minNode = newNode
	}
	if t.maxNode == nil || level.Price > t.maxNode.price {
		t.maxNode = newNode
	}

	// Fix red-black properties
	t.insertFixup(newNode)
}

// Delete removes a price level from the tree.
// Time complexity: O(log n)
func (t *RBTree) Delete(price int64) {
	node := t.search(price)
	if node == nil {
		return
	}

	t.size--

	// Update min/max cache before deletion
	if node == t.minNode {
		t.minNode = t.successor(node)
	}
	if node == t.maxNode {
		t.maxNode = t.predecessor(node)
	}

	t.deleteNode(node)
}

// ForEach iterates over all price levels in order.
// For asks (ascending), iterates lowest to highest.
// For bids (descending tree), iterates highest to lowest.
func (t *RBTree) ForEach(fn func(*PriceLevel) bool) {
	if t.descending {
		t.reverseInOrder(t.root, fn)
	} else {
		t.inOrder(t.root, fn)
	}
}

// search finds a node with the given price.
func (t *RBTree) search(price int64) *rbNode {
	current := t.root
	for current != nil {
		if price < current.price {
			current = current.left
		} else if price > current.price {
			current = current.right
		} else {
			return current
		}
	}
	return nil
}

// inOrder traverses the tree in ascending order.
func (t *RBTree) inOrder(node *rbNode, fn func(*PriceLevel) bool) bool {
	if node == nil {
		return true
	}
	if !t.inOrder(node.left, fn) {
		return false
	}
	if !fn(node.level) {
		return false
	}
	return t.inOrder(node.right, fn)
}

// reverseInOrder traverses the tree in descending order.
func (t *RBTree) reverseInOrder(node *rbNode, fn func(*PriceLevel) bool) bool {
	if node == nil {
		return true
	}
	if !t.reverseInOrder(node.right, fn) {
		return false
	}
	if !fn(node.level) {
		return false
	}
	return t.reverseInOrder(node.left, fn)
}

// successor returns the next node in order.
func (t *RBTree) successor(node *rbNode) *rbNode {
	if node.right != nil {
		current := node.right
		for current.left != nil {
			current = current.left
		}
		return current
	}
	parent := node.parent
	for parent != nil && node == parent.right {
		node = parent
		parent = parent.parent
	}
	return parent
}

// predecessor returns the previous node in order.
func (t *RBTree) predecessor(node *rbNode) *rbNode {
	if node.left != nil {
		current := node.left
		for current.right != nil {
			current = current.right
		}
		return current
	}
	parent := node.parent
	for parent != nil && node == parent.left {
		node = parent
		parent = parent.parent
	}
	return parent
}

// rotateLeft performs a left rotation.
func (t *RBTree) rotateLeft(x *rbNode) {
	y := x.right
	x.right = y.left
	if y.left != nil {
		y.left.parent = x
	}
	y.parent = x.parent
	if x.parent == nil {
		t.root = y
	} else if x == x.parent.left {
		x.parent.left = y
	} else {
		x.parent.right = y
	}
	y.left = x
	x.parent = y
}

// rotateRight performs a right rotation.
func (t *RBTree) rotateRight(x *rbNode) {
	y := x.left
	x.left = y.right
	if y.right != nil {
		y.right.parent = x
	}
	y.parent = x.parent
	if x.parent == nil {
		t.root = y
	} else if x == x.parent.right {
		x.parent.right = y
	} else {
		x.parent.left = y
	}
	y.right = x
	x.parent = y
}

// insertFixup restores red-black properties after insertion.
func (t *RBTree) insertFixup(z *rbNode) {
	for z.parent != nil && z.parent.color == red {
		if z.parent == z.parent.parent.left {
			y := z.parent.parent.right
			if y != nil && y.color == red {
				z.parent.color = black
				y.color = black
				z.parent.parent.color = red
				z = z.parent.parent
			} else {
				if z == z.parent.right {
					z = z.parent
					t.rotateLeft(z)
				}
				z.parent.color = black
				z.parent.parent.color = red
				t.rotateRight(z.parent.parent)
			}
		} else {
			y := z.parent.parent.left
			if y != nil && y.color == red {
				z.parent.color = black
				y.color = black
				z.parent.parent.color = red
				z = z.parent.parent
			} else {
				if z == z.parent.left {
					z = z.parent
					t.rotateRight(z)
				}
				z.parent.color = black
				z.parent.parent.color = red
				t.rotateLeft(z.parent.parent)
			}
		}
	}
	t.root.color = black
}

// transplant replaces subtree rooted at u with subtree rooted at v.
func (t *RBTree) transplant(u, v *rbNode) {
	if u.parent == nil {
		t.root = v
	} else if u == u.parent.left {
		u.parent.left = v
	} else {
		u.parent.right = v
	}
	if v != nil {
		v.parent = u.parent
	}
}

// deleteNode removes a node from the tree.
func (t *RBTree) deleteNode(z *rbNode) {
	var x, xParent *rbNode
	y := z
	yOriginalColor := y.color

	if z.left == nil {
		x = z.right
		xParent = z.parent
		t.transplant(z, z.right)
	} else if z.right == nil {
		x = z.left
		xParent = z.parent
		t.transplant(z, z.left)
	} else {
		y = z.right
		for y.left != nil {
			y = y.left
		}
		yOriginalColor = y.color
		x = y.right
		if y.parent == z {
			xParent = y
		} else {
			xParent = y.parent
			t.transplant(y, y.right)
			y.right = z.right
			y.right.parent = y
		}
		t.transplant(z, y)
		y.left = z.left
		y.left.parent = y
		y.color = z.color
	}

	if yOriginalColor == black {
		t.deleteFixup(x, xParent)
	}
}

// deleteFixup restores red-black properties after deletion.
func (t *RBTree) deleteFixup(x *rbNode, xParent *rbNode) {
	for x != t.root && (x == nil || x.color == black) {
		if x == xParent.left {
			w := xParent.right
			if w != nil && w.color == red {
				w.color = black
				xParent.color = red
				t.rotateLeft(xParent)
				w = xParent.right
			}
			if w == nil || ((w.left == nil || w.left.color == black) && (w.right == nil || w.right.color == black)) {
				if w != nil {
					w.color = red
				}
				x = xParent
				xParent = x.parent
			} else {
				if w.right == nil || w.right.color == black {
					if w.left != nil {
						w.left.color = black
					}
					w.color = red
					t.rotateRight(w)
					w = xParent.right
				}
				w.color = xParent.color
				xParent.color = black
				if w.right != nil {
					w.right.color = black
				}
				t.rotateLeft(xParent)
				x = t.root
			}
		} else {
			w := xParent.left
			if w != nil && w.color == red {
				w.color = black
				xParent.color = red
				t.rotateRight(xParent)
				w = xParent.left
			}
			if w == nil || ((w.right == nil || w.right.color == black) && (w.left == nil || w.left.color == black)) {
				if w != nil {
					w.color = red
				}
				x = xParent
				xParent = x.parent
			} else {
				if w.left == nil || w.left.color == black {
					if w.right != nil {
						w.right.color = black
					}
					w.color = red
					t.rotateLeft(w)
					w = xParent.left
				}
				w.color = xParent.color
				xParent.color = black
				if w.left != nil {
					w.left.color = black
				}
				t.rotateRight(xParent)
				x = t.root
			}
		}
	}
	if x != nil {
		x.color = black
	}
}
