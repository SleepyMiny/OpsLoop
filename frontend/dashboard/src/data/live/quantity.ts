/** Nonnegative Kubernetes Quantity -> Value() bytes, without floating point loss.
 * DecimalSI (n/u/m/k/M/G/T/P/E), BinarySI (Ki..Ei), DecimalExponent are accepted.
 * Kubernetes rounds positive fractional bytes away from zero. Values that the
 * dashboard's number model cannot represent as safe integers are explicit errors.
 */
export function parseMemoryQuantity(value: string, path: string): number {
  const invalid = () => new Error(`${path} must be a nonnegative Kubernetes memory quantity`);
  const unsafe = () => new Error(`${path} exceeds the dashboard's safe integer byte range`);
  if (typeof value !== 'string' || value.length > 4096) throw invalid();
  const m = /^([+-]?)(\d+(?:\.\d*)?|\.\d+)([eE][+-]?\d+|[numkMGTPE]|[KMGTPE]i)?$/.exec(value);
  if (!m) throw invalid();
  const decimal = m[2]!;
  const fraction = decimal.includes('.') ? decimal.length - decimal.indexOf('.') - 1 : 0;
  let numerator = BigInt(decimal.replace('.', ''));
  if (numerator === 0n) return 0;
  if (m[1] === '-') throw invalid();
  const suffix = m[3] ?? '';
  let exponent = -BigInt(fraction);
  if (suffix.endsWith('i')) {
    numerator *= 1n << BigInt(10 * ('KMGTPE'.indexOf(suffix[0]!) + 1));
  } else if (/^[eE][+-]?\d+$/.test(suffix)) {
    exponent += BigInt(suffix.slice(1));
  } else {
    const scale: Record<string, number> = { '': 0, n: -9, u: -6, m: -3, k: 3, M: 6, G: 9, T: 12, P: 15, E: 18 };
    exponent += BigInt(scale[suffix]!);
  }
  // Bound the exponent before exponentiation: even adversarial e1000000000 is
  // constant-space here. Every nonzero sub-byte positive value rounds to one.
  const digits = BigInt(numerator.toString().length);
  if (digits + exponent > 16n) throw unsafe();
  if (digits + exponent <= 0n) return 1;
  let bytes: bigint;
  if (exponent >= 0n) bytes = numerator * 10n ** exponent;
  else {
    const divisor = 10n ** -exponent;
    bytes = (numerator + divisor - 1n) / divisor;
  }
  if (bytes > BigInt(Number.MAX_SAFE_INTEGER)) throw unsafe();
  return Number(bytes);
}
