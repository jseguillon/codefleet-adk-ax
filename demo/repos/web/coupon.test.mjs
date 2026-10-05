import { test } from 'node:test';
import assert from 'node:assert/strict';
import { previewCoupon } from './coupon.mjs';
test('discount and fractional cents', () => {
  assert.equal(previewCoupon(10000, 20), 8000);
  assert.equal(previewCoupon(999, 15), 849);
});
test('bounds and invalid inputs', () => {
  assert.equal(previewCoupon(500, 100), 0);
  for (const args of [[-1, 10], [100, 101], [1.5, 10], [100, true]])
    assert.throws(() => previewCoupon(...args));
});
