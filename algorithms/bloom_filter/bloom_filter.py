"""
Bloom Filter Implementation
===========================

A Bloom filter is a space-efficient probabilistic data structure used to test
whether an element is a member of a set. It can have false positives but never
false negatives - if it says an element is NOT in the set, it definitely isn't.

Algorithm Overview:
------------------
1. Initialize a bit array of size 'm' with all bits set to 0
2. Use 'k' different hash functions
3. To ADD an element:
   - Hash the element with each of the k hash functions
   - Set the bits at positions hash_i(element) % m to 1
4. To CHECK if an element exists:
   - Hash the element with each of the k hash functions
   - If ALL bits at positions hash_i(element) % m are 1, return "possibly in set"
   - If ANY bit is 0, return "definitely not in set"

False Positive Mathematics:
--------------------------
After inserting n elements with k hash functions into a bit array of size m:

- Probability a specific bit is still 0: (1 - 1/m)^(kn) ≈ e^(-kn/m)
- Probability of false positive (all k bits are 1 for a non-member):

  P(false positive) ≈ (1 - e^(-kn/m))^k

Optimal Parameters:
------------------
Given:
  - n = expected number of elements
  - p = desired false positive probability

Optimal bit array size:
  m = -(n * ln(p)) / (ln(2)^2)

Optimal number of hash functions:
  k = (m/n) * ln(2)

Time Complexity:
---------------
- add():      O(k) - compute k hashes and set k bits
- contains(): O(k) - compute k hashes and check k bits
- Where k is the number of hash functions (typically small, e.g., 3-10)

Space Complexity:
----------------
- O(m) bits where m is the size of the bit array
- Much more space-efficient than storing actual elements
- Example: 1% FP rate needs ~9.6 bits per element (vs 64+ bits for storing strings)

Usage Examples:
--------------
    # Create a Bloom filter for 1000 elements with 1% false positive rate
    bf = BloomFilter.create_optimal(expected_elements=1000, fp_rate=0.01)

    # Add elements
    bf.add("hello")
    bf.add("world")

    # Check membership
    bf.contains("hello")  # True (definitely or probably in set)
    bf.contains("foo")    # False (definitely not in set) or True (false positive)

    # Manual creation with specific size and hash count
    bf = BloomFilter(size=10000, hash_count=7)

Common Use Cases:
----------------
1. Database query optimization - avoid expensive disk lookups for non-existent keys
2. Web caching - check if URL has been cached before querying cache
3. Spell checkers - quickly check if word might be in dictionary
4. Network routers - packet routing decisions
5. Distributed systems - check if data exists on a node before network request
"""

import math
import hashlib
from typing import Any, Optional


class BloomFilter:
    """
    A probabilistic data structure for set membership testing.

    Guarantees:
    - No false negatives: if contains() returns False, element is definitely not in set
    - Possible false positives: if contains() returns True, element MIGHT be in set

    Attributes:
        size (int): Size of the bit array (m)
        hash_count (int): Number of hash functions (k)
        bit_array (list): The underlying bit array
        items_added (int): Count of items added (for FP rate estimation)
    """

    def __init__(self, size: int, hash_count: int):
        """
        Initialize a Bloom filter with specified size and hash count.

        Args:
            size: Number of bits in the filter (m). Larger = fewer false positives.
            hash_count: Number of hash functions (k). Optimal is (m/n) * ln(2).

        Raises:
            ValueError: If size or hash_count is not positive.
        """
        if size <= 0:
            raise ValueError("Size must be positive")
        if hash_count <= 0:
            raise ValueError("Hash count must be positive")

        self.size = size
        self.hash_count = hash_count
        # Use a list of booleans as bit array (Python doesn't have native bit arrays)
        # For production, consider using bitarray library for memory efficiency
        self.bit_array = [False] * size
        self.items_added = 0

    @classmethod
    def create_optimal(cls, expected_elements: int, fp_rate: float) -> 'BloomFilter':
        """
        Create a Bloom filter with optimal size and hash count for given parameters.

        This factory method calculates the optimal bit array size and number of
        hash functions to achieve the desired false positive rate.

        Args:
            expected_elements: Expected number of elements to be added (n)
            fp_rate: Desired false positive probability (0 < p < 1)

        Returns:
            BloomFilter: Optimally configured Bloom filter

        Raises:
            ValueError: If parameters are invalid

        Example:
            # For 10,000 elements with 0.1% false positive rate
            bf = BloomFilter.create_optimal(10000, 0.001)
        """
        if expected_elements <= 0:
            raise ValueError("Expected elements must be positive")
        if not (0 < fp_rate < 1):
            raise ValueError("False positive rate must be between 0 and 1")

        # Optimal size formula: m = -(n * ln(p)) / (ln(2)^2)
        # Derivation: minimize m while achieving FP rate p
        size = int(-expected_elements * math.log(fp_rate) / (math.log(2) ** 2))

        # Optimal hash count formula: k = (m/n) * ln(2)
        # Derivation: minimize false positive probability for given m and n
        hash_count = int((size / expected_elements) * math.log(2))

        # Ensure at least 1 hash function
        hash_count = max(1, hash_count)

        return cls(size, hash_count)

    def _get_hash_values(self, item: Any) -> list[int]:
        """
        Generate k hash values for an item using double hashing technique.

        Instead of k independent hash functions, we use double hashing:
        h_i(x) = (h1(x) + i * h2(x)) % m

        This is mathematically proven to be as good as k independent hash functions
        for Bloom filters (Kirsch and Mitzenmacher, 2006).

        Args:
            item: The item to hash (will be converted to string)

        Returns:
            List of k hash values, each in range [0, size-1]
        """
        # Convert item to bytes for hashing
        item_bytes = str(item).encode('utf-8')

        # Use MD5 and SHA256 as our two base hash functions
        # MD5: 128-bit hash, we use first 64 bits
        # SHA256: 256-bit hash, we use first 64 bits
        # Note: We're not using these for cryptographic purposes

        h1 = int(hashlib.md5(item_bytes).hexdigest(), 16)
        h2 = int(hashlib.sha256(item_bytes).hexdigest(), 16)

        # Generate k hash values using double hashing
        # h_i(x) = (h1(x) + i * h2(x)) mod m
        hash_values = []
        for i in range(self.hash_count):
            # Combine hashes and take modulo to get bit position
            combined = (h1 + i * h2) % self.size
            hash_values.append(combined)

        return hash_values

    def add(self, item: Any) -> None:
        """
        Add an item to the Bloom filter.

        This sets k bits in the bit array to 1, where the positions are
        determined by the k hash functions.

        Time Complexity: O(k) where k is the number of hash functions
        Space Complexity: O(1) - no additional space needed

        Args:
            item: The item to add (any hashable type)

        Example:
            bf = BloomFilter(1000, 5)
            bf.add("hello")
            bf.add(42)
            bf.add(("tuple", "item"))
        """
        # Get all k hash positions for this item
        hash_values = self._get_hash_values(item)

        # Set each corresponding bit to 1
        for position in hash_values:
            self.bit_array[position] = True

        # Track number of items for false positive estimation
        self.items_added += 1

    def contains(self, item: Any) -> bool:
        """
        Check if an item might be in the set.

        Returns:
        - False: Item is DEFINITELY NOT in the set (no false negatives)
        - True: Item is PROBABLY in the set (may be a false positive)

        Time Complexity: O(k) where k is the number of hash functions
        Space Complexity: O(1)

        Args:
            item: The item to check

        Returns:
            bool: False if definitely not in set, True if possibly in set

        Example:
            bf.add("hello")
            bf.contains("hello")  # True (correct positive)
            bf.contains("world")  # False (true negative) or True (false positive)
        """
        # Get all k hash positions for this item
        hash_values = self._get_hash_values(item)

        # Check if ALL k bits are set to 1
        # If ANY bit is 0, the item was definitely never added
        for position in hash_values:
            if not self.bit_array[position]:
                return False  # Definitely not in set

        # All bits are 1, so item is probably in set
        # (could be a false positive if bits were set by other items)
        return True

    def estimate_false_positive_rate(self) -> float:
        """
        Estimate the current false positive probability.

        Formula: P(FP) ≈ (1 - e^(-kn/m))^k

        Where:
        - k = number of hash functions
        - n = number of items added
        - m = size of bit array

        This is an approximation assuming hash functions are perfectly random.

        Returns:
            float: Estimated false positive probability (0 to 1)

        Example:
            bf = BloomFilter.create_optimal(1000, 0.01)
            for i in range(1000):
                bf.add(i)
            print(f"FP rate: {bf.estimate_false_positive_rate():.4f}")  # ~0.01
        """
        if self.items_added == 0:
            return 0.0

        # P(FP) = (1 - e^(-kn/m))^k
        # Breakdown:
        # - e^(-kn/m) = probability that a specific bit is still 0
        # - 1 - e^(-kn/m) = probability that a specific bit is 1
        # - (...)^k = probability that all k bits are 1 (false positive)

        exponent = -self.hash_count * self.items_added / self.size
        probability_bit_is_one = 1 - math.exp(exponent)
        false_positive_rate = probability_bit_is_one ** self.hash_count

        return false_positive_rate

    def get_fill_ratio(self) -> float:
        """
        Get the ratio of bits that are set to 1.

        A higher fill ratio means more potential for false positives.
        Optimal fill ratio is around 50% (when k = (m/n) * ln(2)).

        Returns:
            float: Ratio of set bits (0 to 1)
        """
        set_bits = sum(self.bit_array)
        return set_bits / self.size

    def __len__(self) -> int:
        """Return the number of items that have been added."""
        return self.items_added

    def __contains__(self, item: Any) -> bool:
        """Support 'in' operator: item in bloom_filter"""
        return self.contains(item)

    def __repr__(self) -> str:
        """String representation showing filter configuration."""
        return (f"BloomFilter(size={self.size}, hash_count={self.hash_count}, "
                f"items={self.items_added}, fill_ratio={self.get_fill_ratio():.2%})")


# Additional utility functions

def calculate_optimal_size(expected_elements: int, fp_rate: float) -> int:
    """
    Calculate optimal bit array size for given parameters.

    Formula: m = -(n * ln(p)) / (ln(2)^2)

    Args:
        expected_elements: Number of elements to store
        fp_rate: Desired false positive rate

    Returns:
        int: Optimal bit array size
    """
    return int(-expected_elements * math.log(fp_rate) / (math.log(2) ** 2))


def calculate_optimal_hash_count(size: int, expected_elements: int) -> int:
    """
    Calculate optimal number of hash functions.

    Formula: k = (m/n) * ln(2)

    Args:
        size: Bit array size
        expected_elements: Number of elements to store

    Returns:
        int: Optimal number of hash functions
    """
    return max(1, int((size / expected_elements) * math.log(2)))


def bits_per_element(fp_rate: float) -> float:
    """
    Calculate the number of bits needed per element for a given FP rate.

    Formula: bits_per_element = -ln(p) / (ln(2)^2) ≈ -1.44 * log2(p)

    Examples:
    - 1% FP rate: ~9.6 bits per element
    - 0.1% FP rate: ~14.4 bits per element
    - 0.01% FP rate: ~19.2 bits per element

    Args:
        fp_rate: Desired false positive rate

    Returns:
        float: Bits needed per element
    """
    return -math.log(fp_rate) / (math.log(2) ** 2)
