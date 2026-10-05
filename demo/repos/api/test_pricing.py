import unittest
from pricing import apply_coupon

class CouponTests(unittest.TestCase):
    def test_discount(self):
        self.assertEqual(apply_coupon(10000, 20), 8000)
    def test_floor_cents(self):
        self.assertEqual(apply_coupon(999, 15), 849)
    def test_bounds(self):
        self.assertEqual(apply_coupon(500, 0), 500)
        self.assertEqual(apply_coupon(500, 100), 0)
    def test_invalid(self):
        for total, pct in [(-1, 10), (100, -1), (100, 101), (1.5, 10), (100, True)]:
            with self.assertRaises(ValueError):
                apply_coupon(total, pct)

if __name__ == "__main__":
    unittest.main()
