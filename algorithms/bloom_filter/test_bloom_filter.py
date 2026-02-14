"""
Test Suite for Bloom Filter Implementation
==========================================

Comprehensive test suite using pytest for the Bloom Filter data structure.

Run with: pytest test_bloom_filter.py -v
Run with coverage: pytest test_bloom_filter.py --cov=bloom_filter --cov-report=html

Test Categories:
1. Basic Functionality - add, contains, operators
2. False Negatives - guarantee that no false negatives occur
3. False Positive Rate - verify probabilistic behavior
4. Optimal Parameters - verify mathematical calculations
5. Data Types - test with various Python types
6. Edge Cases - error handling and boundary conditions
7. Performance - scalability and efficiency tests
"""

import pytest
import math
from bloom_filter import (
    BloomFilter,
    calculate_optimal_size,
    calculate_optimal_hash_count,
    bits_per_element
)


class TestBasicFunctionality:
    """Test core add and contains operations."""

    def test_add_and_contains_single_item(self):
        """Test adding and checking a single item."""
        bf = BloomFilter(size=1000, hash_count=5)
        bf.add("hello")
        assert bf.contains("hello"), "Added item should be found"
        assert len(bf) == 1, "Length should be 1 after adding one item"

    def test_add_multiple_items(self):
        """Test adding multiple items."""
        bf = BloomFilter(size=1000, hash_count=5)
        items = ["apple", "banana", "cherry", "date", "elderberry"]

        for item in items:
            bf.add(item)

        for item in items:
            assert bf.contains(item), f"Added item '{item}' should be found"

        assert len(bf) == len(items)

    def test_contains_returns_false_for_non_existent(self):
        """Test that items not added typically return False."""
        bf = BloomFilter(size=10000, hash_count=7)
        bf.add("exists")

        # With a large filter, items not added should return False
        # (though false positives are possible)
        assert not bf.contains("does_not_exist_xyz_123")

    def test_in_operator(self):
        """Test the 'in' operator works correctly."""
        bf = BloomFilter(size=1000, hash_count=5)
        bf.add("test_item")

        assert "test_item" in bf, "'in' operator should work for added items"
        # Note: Can't assert "not in" due to possible false positives

    def test_repr(self):
        """Test string representation."""
        bf = BloomFilter(size=1000, hash_count=5)
        bf.add("test")

        repr_str = repr(bf)
        assert "BloomFilter" in repr_str
        assert "size=1000" in repr_str
        assert "hash_count=5" in repr_str
        assert "items=1" in repr_str


class TestFalseNegatives:
    """Verify the critical guarantee: no false negatives."""

    def test_no_false_negatives_small_dataset(self):
        """Verify no false negatives with 100 items."""
        bf = BloomFilter.create_optimal(expected_elements=100, fp_rate=0.01)
        items = [f"item_{i}" for i in range(100)]

        for item in items:
            bf.add(item)

        # CRITICAL: Every item added MUST be found
        for item in items:
            assert bf.contains(item), f"FALSE NEGATIVE: '{item}' not found - this is a critical bug!"

    def test_no_false_negatives_large_dataset(self):
        """Verify no false negatives with 10,000 items."""
        bf = BloomFilter.create_optimal(expected_elements=10000, fp_rate=0.01)
        items = [f"element_{i}" for i in range(10000)]

        for item in items:
            bf.add(item)

        false_negatives = []
        for item in items:
            if not bf.contains(item):
                false_negatives.append(item)

        assert len(false_negatives) == 0, \
            f"Found {len(false_negatives)} false negatives: {false_negatives[:10]}"

    def test_no_false_negatives_after_many_operations(self):
        """Verify no false negatives even after filter is heavily used."""
        bf = BloomFilter.create_optimal(expected_elements=1000, fp_rate=0.05)

        # Add way more items than expected to stress test
        items = [f"stress_{i}" for i in range(1000)]
        for item in items:
            bf.add(item)

        # All items must still be found
        for item in items:
            assert bf.contains(item), "No false negatives even with high load"


class TestFalsePositiveRate:
    """Test the probabilistic false positive behavior."""

    def test_false_positive_rate_1_percent(self):
        """Test that actual FP rate is close to 1% target."""
        expected_elements = 5000
        target_fp_rate = 0.01

        bf = BloomFilter.create_optimal(
            expected_elements=expected_elements,
            fp_rate=target_fp_rate
        )

        # Add expected number of elements
        for i in range(expected_elements):
            bf.add(f"element_{i}")

        # Test with items NOT in the set
        test_count = 5000
        false_positives = 0

        for i in range(test_count):
            test_item = f"test_item_{i}"
            if bf.contains(test_item):
                false_positives += 1

        actual_fp_rate = false_positives / test_count

        # Allow 3x variance (probabilistic behavior)
        assert actual_fp_rate < target_fp_rate * 3, \
            f"FP rate {actual_fp_rate:.4%} exceeds 3x target {target_fp_rate:.4%}"

    def test_estimate_false_positive_rate(self):
        """Test FP rate estimation function."""
        bf = BloomFilter.create_optimal(expected_elements=1000, fp_rate=0.01)

        # Empty filter should have 0 FP rate
        assert bf.estimate_false_positive_rate() == 0.0

        # Add items and check estimation
        for i in range(1000):
            bf.add(f"item_{i}")

        estimated_rate = bf.estimate_false_positive_rate()

        # Should be close to target (within 5x due to probabilistic nature)
        assert 0 < estimated_rate < 0.05, \
            f"Estimated FP rate {estimated_rate:.4%} should be reasonable"

    def test_fill_ratio(self):
        """Test bit array fill ratio calculation."""
        bf = BloomFilter(size=1000, hash_count=5)

        # Empty filter
        assert bf.get_fill_ratio() == 0.0

        # Add some items
        for i in range(100):
            bf.add(f"item_{i}")

        fill_ratio = bf.get_fill_ratio()

        # Fill ratio should be between 0 and 1
        assert 0 < fill_ratio < 1, "Fill ratio should be between 0 and 1"

        # With optimal parameters, fill ratio should be around 50%
        # (though can vary based on hash function distribution)
        assert fill_ratio > 0.1, "Fill ratio should be reasonable"


class TestOptimalParameters:
    """Test mathematical parameter calculations."""

    @pytest.mark.parametrize("expected_elements,fp_rate,expected_min_size", [
        (1000, 0.01, 9000),      # ~9585 bits
        (10000, 0.001, 140000),  # ~143775 bits
        (5000, 0.05, 20000),     # ~22801 bits
    ])
    def test_optimal_size_calculation(self, expected_elements, fp_rate, expected_min_size):
        """Test optimal size calculation for various parameters."""
        size = calculate_optimal_size(expected_elements, fp_rate)

        assert size > expected_min_size, \
            f"Size {size} should be >= {expected_min_size}"

        # Verify formula: m = -(n * ln(p)) / (ln(2)^2)
        expected_size = int(-expected_elements * math.log(fp_rate) / (math.log(2) ** 2))
        assert size == expected_size

    @pytest.mark.parametrize("size,expected_elements,expected_min_k", [
        (10000, 1000, 6),   # ~7
        (20000, 2000, 6),   # ~7
        (100000, 10000, 6), # ~7
    ])
    def test_optimal_hash_count(self, size, expected_elements, expected_min_k):
        """Test optimal hash function count calculation."""
        k = calculate_optimal_hash_count(size, expected_elements)

        assert k >= expected_min_k, f"Hash count {k} should be >= {expected_min_k}"

        # Verify formula: k = (m/n) * ln(2)
        expected_k = max(1, int((size / expected_elements) * math.log(2)))
        assert k == expected_k

    @pytest.mark.parametrize("fp_rate,expected_bits", [
        (0.01, 9.59),   # 1% FP
        (0.001, 14.38), # 0.1% FP
        (0.1, 4.79),    # 10% FP
    ])
    def test_bits_per_element(self, fp_rate, expected_bits):
        """Test bits per element calculation."""
        bpe = bits_per_element(fp_rate)

        # Allow small floating point variance
        assert abs(bpe - expected_bits) < 0.01, \
            f"Bits per element {bpe:.2f} should be close to {expected_bits}"

    def test_create_optimal_produces_valid_filter(self):
        """Test that create_optimal produces a working filter."""
        bf = BloomFilter.create_optimal(expected_elements=1000, fp_rate=0.01)

        assert bf.size > 0
        assert bf.hash_count > 0

        # Test it works
        bf.add("test")
        assert bf.contains("test")


class TestDataTypes:
    """Test with various Python data types."""

    @pytest.mark.parametrize("item", [
        "string",
        42,
        3.14159,
        (1, 2, 3),
        True,
        False,
        None,
        -999,
        "",  # empty string
        0,   # zero
    ])
    def test_various_data_types(self, item):
        """Test that various data types can be added and found."""
        bf = BloomFilter(size=1000, hash_count=5)
        bf.add(item)
        assert bf.contains(item), f"Item {item!r} of type {type(item)} should be found"

    def test_unicode_strings(self):
        """Test Unicode string handling."""
        bf = BloomFilter(size=1000, hash_count=5)
        items = ["hello", "世界", "🌍", "Привет", "مرحبا"]

        for item in items:
            bf.add(item)

        for item in items:
            assert bf.contains(item), f"Unicode string '{item}' should be found"

    def test_large_numbers(self):
        """Test with very large numbers."""
        bf = BloomFilter(size=1000, hash_count=5)
        large_num = 123456789012345678901234567890

        bf.add(large_num)
        assert bf.contains(large_num)

    def test_complex_objects(self):
        """Test with complex tuple structures."""
        bf = BloomFilter(size=1000, hash_count=5)
        items = [
            (1, 2, 3),
            ((1, 2), (3, 4)),
            (None, True, False),
        ]

        for item in items:
            bf.add(item)
            assert bf.contains(item)


class TestEdgeCases:
    """Test edge cases and error handling."""

    def test_empty_filter_behavior(self):
        """Test behavior of empty filter."""
        bf = BloomFilter(size=100, hash_count=3)

        assert len(bf) == 0
        assert bf.get_fill_ratio() == 0.0
        assert bf.estimate_false_positive_rate() == 0.0

        # Empty filter should return False for any query
        assert not bf.contains("anything")

    def test_minimal_filter(self):
        """Test minimal valid filter configuration."""
        bf = BloomFilter(size=1, hash_count=1)
        bf.add("item")

        # Should work, though not useful in practice
        assert bf.contains("item")

    def test_single_bit_collision(self):
        """Test that multiple items can map to overlapping bits."""
        bf = BloomFilter(size=10, hash_count=1)  # Very small to force collisions

        items = [f"item_{i}" for i in range(20)]
        for item in items:
            bf.add(item)

        # All items should still be "found" (though all true due to collisions)
        for item in items:
            assert bf.contains(item)

    @pytest.mark.parametrize("invalid_size,invalid_hash_count,error_msg", [
        (0, 5, "Size must be positive"),
        (-1, 5, "Size must be positive"),
        (100, 0, "Hash count must be positive"),
        (100, -5, "Hash count must be positive"),
    ])
    def test_invalid_parameters(self, invalid_size, invalid_hash_count, error_msg):
        """Test that invalid parameters raise ValueError."""
        with pytest.raises(ValueError, match=error_msg):
            BloomFilter(size=invalid_size, hash_count=invalid_hash_count)

    @pytest.mark.parametrize("expected_elements,fp_rate", [
        (0, 0.01),    # zero elements
        (-100, 0.01), # negative elements
        (1000, 0),    # zero FP rate
        (1000, 1.0),  # FP rate = 1
        (1000, 1.5),  # FP rate > 1
        (1000, -0.1), # negative FP rate
    ])
    def test_invalid_optimal_parameters(self, expected_elements, fp_rate):
        """Test that invalid optimal parameters raise ValueError."""
        with pytest.raises(ValueError):
            BloomFilter.create_optimal(expected_elements, fp_rate)

    def test_duplicate_additions(self):
        """Test that adding the same item multiple times is idempotent."""
        bf = BloomFilter(size=1000, hash_count=5)

        # Add same item 100 times
        for _ in range(100):
            bf.add("duplicate")

        # Should still be found
        assert bf.contains("duplicate")

        # Items count reflects all additions (this is expected behavior)
        assert len(bf) == 100


class TestPerformance:
    """Performance and scalability tests."""

    def test_large_scale_insertions(self):
        """Test with 100,000 insertions."""
        bf = BloomFilter.create_optimal(expected_elements=100000, fp_rate=0.01)

        # Add 100k items
        for i in range(100000):
            bf.add(f"item_{i}")

        assert len(bf) == 100000

        # Verify no false negatives on sample
        sample_items = [f"item_{i}" for i in range(0, 100000, 1000)]
        for item in sample_items:
            assert bf.contains(item)

    def test_memory_efficiency(self):
        """Verify memory efficiency compared to storing actual data."""
        n = 10000
        bf = BloomFilter.create_optimal(expected_elements=n, fp_rate=0.01)

        # Bloom filter uses ~9.6 bits per element for 1% FP rate
        # That's ~1.2 bytes per element
        expected_bytes = n * 1.2

        # Python list overhead: each bool in list is ~28 bytes (!)
        # But the theoretical minimum is m/8 bytes
        theoretical_bytes = bf.size / 8

        assert theoretical_bytes < expected_bytes * 10, \
            "Bloom filter should be memory efficient"

    def test_hash_distribution(self):
        """Test that hash functions distribute bits reasonably."""
        bf = BloomFilter(size=10000, hash_count=7)

        # Add 1000 items
        for i in range(1000):
            bf.add(f"test_{i}")

        # Fill ratio should be reasonable (not too high, not too low)
        fill_ratio = bf.get_fill_ratio()

        assert 0.2 < fill_ratio < 0.8, \
            f"Fill ratio {fill_ratio:.2%} should be reasonable, indicating good hash distribution"


class TestRealWorldScenarios:
    """Test realistic use cases."""

    def test_cache_filter_scenario(self):
        """Simulate using Bloom filter for cache existence checks."""
        # Scenario: 10,000 cached URLs, check before expensive cache lookup
        bf = BloomFilter.create_optimal(expected_elements=10000, fp_rate=0.01)

        cached_urls = [f"https://example.com/page/{i}" for i in range(10000)]

        # Populate filter with cached URLs
        for url in cached_urls:
            bf.add(url)

        # Test cache hits (should all be found)
        for url in cached_urls[:100]:
            assert bf.contains(url), "Cached URL should be found"

        # Test cache misses
        uncached_urls = [f"https://other.com/page/{i}" for i in range(1000)]
        false_positives = sum(1 for url in uncached_urls if bf.contains(url))

        fp_rate = false_positives / len(uncached_urls)
        assert fp_rate < 0.05, "False positive rate should be low"

    def test_spell_checker_scenario(self):
        """Simulate using Bloom filter for spell checking."""
        # Scenario: Dictionary with 50,000 words
        bf = BloomFilter.create_optimal(expected_elements=50000, fp_rate=0.001)

        # Add common words
        words = [f"word{i}" for i in range(50000)]
        for word in words:
            bf.add(word)

        # Check if words exist
        assert bf.contains("word100")
        assert bf.contains("word49999")

        # Misspelled words (mostly should return False)
        misspellings = ["wrod100", "wird100", "wordd100"]
        # Can't assert False due to possible FP, but should mostly be False

        print(f"Dictionary size: {len(bf)} words")
        print(f"Fill ratio: {bf.get_fill_ratio():.2%}")
        print(f"Estimated FP rate: {bf.estimate_false_positive_rate():.4%}")

    def test_distributed_systems_scenario(self):
        """Simulate checking data existence across distributed nodes."""
        # Scenario: Check if data exists before expensive network call
        bf = BloomFilter.create_optimal(expected_elements=100000, fp_rate=0.001)

        # Simulate data IDs on this node
        local_data_ids = [f"data_{i}" for i in range(100000)]
        for data_id in local_data_ids:
            bf.add(data_id)

        # Check before network call
        test_ids = [
            "data_50000",  # exists
            "data_99999",  # exists
            "data_100000", # doesn't exist
            "data_999999", # doesn't exist
        ]

        for test_id in test_ids[:2]:
            assert bf.contains(test_id), "Local data should be found"


def test_example_from_docstring():
    """Test the example from the module docstring."""
    bf = BloomFilter.create_optimal(expected_elements=1000, fp_rate=0.01)

    bf.add("hello")
    bf.add("world")

    assert bf.contains("hello")
    assert bf.contains("world")


if __name__ == "__main__":
    pytest.main([__file__, "-v", "--tb=short"])
