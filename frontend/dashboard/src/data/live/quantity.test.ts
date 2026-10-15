import { describe, expect, it } from 'vitest';
import goQuantities from '../../domain/__fixtures__/go-quantities.json';
import { parseMemoryQuantity } from './fromCrd';

describe('Kubernetes Quantity Value() bytes contract', () => {
  it.each([
    ['1e6', 1000000], ['1E+6', 1000000], ['1m', 1], ['100m', 1], ['1Pi', 1125899906842624],
    ['512Mi', 536870912], ['1536Mi', 1610612736], ['1.5Gi', 1610612736],
    ['0.1', 1], ['1.1', 2], ['.5', 1], ['1.', 1], ['+1', 1], ['0', 0], ['-0', 0],
    ['1n', 1], ['1u', 1], ['1e-100', 1], ['0e100000', 0], ['1.25Ki', 1280],
    ['9007199254740991', Number.MAX_SAFE_INTEGER], ['9007199254740990.1', Number.MAX_SAFE_INTEGER],
  ])('%s => %s bytes', (wire, expected) => {
    expect(parseMemoryQuantity(wire, 'policy.spec.filter.minimumAvailableMemory')).toBe(expected);
  });
  it.each(['8Pi', '1Ei', '1E', '9007199254740992', '9007199254740991.1', '1e100000', '-1', '-0.1', 'NaN', 'Infinity', '1MiB', '1K', ' 1', '1 ', '', '.', '1e', '1e1.5'])('rejects unsafe or invalid %s with a field path', wire => {
    expect(() => parseMemoryQuantity(wire, 'spec.filter.minimumAvailableMemory')).toThrow(/spec.filter.minimumAvailableMemory/);
  });
});

// Values come from Kubernetes resource.ParseQuantity(...).Value(), not a TS oracle.
describe('actual Kubernetes Quantity cross-language vectors', () => {
  for (const q of goQuantities) it(q.input || '(empty)', () => {
    // Reject the Go parser's digitless "." quirk explicitly at the UI boundary.
    if (q.invalid || q.input === '.' || BigInt(q.bytes ?? '0') > BigInt(Number.MAX_SAFE_INTEGER)) {
      expect(() => parseMemoryQuantity(q.input, 'quantity')).toThrow(/quantity/);
    } else {
      expect(parseMemoryQuantity(q.input, 'quantity')).toBe(Number(q.bytes));
    }
  });
});
