# RB-Tree Testing Strategy

## Why Keep Custom Implementation?

The custom RB-Tree is performance-critical and educationally valuable. Instead of replacing with a library, we should add comprehensive tests.

## Test Coverage Needed

### 1. **Correctness Tests**
- [ ] Insert maintains sorted order
- [ ] Delete maintains sorted order
- [ ] Min/Max cache stays synchronized
- [ ] Red-black properties maintained after operations
- [ ] Descending mode works correctly

### 2. **Property-Based Tests**
```go
// Example: Verify RB-Tree properties always hold
func TestRedBlackProperties(t *testing.T) {
    tree := NewRBTree(false)

    // Insert random values
    for i := 0; i < 1000; i++ {
        price := rand.Int63()
        tree.Insert(NewPriceLevel(price))

        // Verify properties after each insert
        assert.True(t, verifyBlackHeight(tree.root))
        assert.True(t, verifyNoConsecutiveReds(tree.root))
    }
}
```

### 3. **Performance Tests**
```go
func BenchmarkInsert(b *testing.B) {
    tree := NewRBTree(false)
    b.ResetTimer()

    for i := 0; i < b.N; i++ {
        tree.Insert(NewPriceLevel(int64(i)))
    }
}

func BenchmarkMinAccess(b *testing.B) {
    tree := setupTreeWith1000Levels()
    b.ResetTimer()

    for i := 0; i < b.N; i++ {
        _ = tree.Min() // Should be O(1)
    }
}
```

### 4. **Edge Cases**
- [ ] Empty tree operations
- [ ] Single node tree
- [ ] Duplicate prices
- [ ] Delete non-existent price
- [ ] Large tree (100k+ nodes)

### 5. **Cache Validation**
```go
func TestMinMaxCacheValid(t *testing.T) {
    tree := NewRBTree(false)

    // Insert values
    tree.Insert(NewPriceLevel(100))
    tree.Insert(NewPriceLevel(50))
    tree.Insert(NewPriceLevel(150))

    // Verify cache
    assert.Equal(t, int64(50), tree.Min().Price)

    // Delete min
    tree.Delete(50)

    // Cache should update to next min
    assert.Equal(t, int64(100), tree.Min().Price)
}
```

## Comparison with Library

| Feature | Custom RB-Tree | gods Library |
|---------|---------------|--------------|
| **Min/Max Cache** | O(1) always | O(log n) on delete |
| **Code Lines** | 459 | ~100 wrapper |
| **Dependencies** | 0 | 1 external |
| **Educational Value** | High | Low |
| **Maintenance** | Need tests | Already tested |
| **Performance** | Optimized for use case | Generic |
| **Interview Value** | Shows expertise | Shows pragmatism |

## Recommendation

**Keep custom implementation + Add tests**

The performance benefit of O(1) cached min/max is critical for an exchange where `GetBestBid/Ask()` is called on every match. The gods library would require O(log n) traversal to rebuild cache after deletions.

## Quick Win: Add Property Validation

Add this to your existing tests:

```go
// internal/orderbook/rbtree_test.go
func verifyRedBlackProperties(t *testing.T, tree *RBTree) {
    if tree.root == nil {
        return
    }

    // Property 1: Root is black
    assert.Equal(t, black, tree.root.color, "Root must be black")

    // Property 2: No consecutive red nodes
    verifyNoConsecutiveReds(t, tree.root)

    // Property 3: Same black height on all paths
    verifyBlackHeight(t, tree.root)

    // Property 4: Min/Max cache is correct
    actualMin := findMinNode(tree.root)
    assert.Equal(t, actualMin, tree.minNode, "Min cache invalid")
}
```

This gives you confidence without external dependencies.
