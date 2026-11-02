import { test } from 'node:test';
import assert from 'node:assert/strict';

import { estimateOffset, driftAction } from '../web/js/sync.js';

test('offset: медиана трёх образцов с наименьшим rtt, выбросы отброшены', () => {
  const samples = [
    { offset: 100, rtt: 4 },
    { offset: 102, rtt: 6 },
    { offset: 98, rtt: 5 },
    { offset: 400, rtt: 250 }, // загруженная сеть
    { offset: -300, rtt: 180 },
    { offset: 140, rtt: 40 },
  ];
  assert.deepEqual(estimateOffset(samples), { offset: 100, rtt: 5 });
});

test('offset: меньше трёх образцов — медиана того, что есть', () => {
  assert.deepEqual(estimateOffset([{ offset: 10, rtt: 2 }, { offset: 20, rtt: 3 }]), { offset: 15, rtt: 2.5 });
  assert.equal(estimateOffset([]), null);
});

test('дрейф: мёртвая зона, подстройка темпом, жёсткая перемотка', () => {
  assert.deepEqual(driftAction(0.03), { rate: 1 });
  assert.deepEqual(driftAction(-0.049), { rate: 1 });
  assert.deepEqual(driftAction(0.1), { rate: 1.01 });
  assert.deepEqual(driftAction(-0.1), { rate: 0.99 });
  assert.deepEqual(driftAction(0.5), { rate: 1.02 });   // ограничено 2 %
  assert.deepEqual(driftAction(-0.9), { rate: 0.98 });
  assert.deepEqual(driftAction(1.2), { seek: true });
  assert.deepEqual(driftAction(-3), { seek: true });
});
