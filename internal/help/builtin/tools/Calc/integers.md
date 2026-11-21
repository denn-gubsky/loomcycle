---
name: Calc/integers
kind: section
description: "Calc integer functions — factorial, binomial and permutations, Fibonacci, gcd and lcm, primes and factoring, modular arithmetic, bitwise operations and number bases, with the size limits of each."
---
Functions on exact integers. Every result here is exact. An argument must be
an exact integer: a float such as `log2(1024)` is refused (`type`), because
a float never becomes exact again — compute the argument exactly instead.
`fact`, `choose`, `nCr` and `comb` are not names here: write `factorial` and
`binomial`.

## Combinatorics

### factorial(n)
`n!` for an integer `n >= 0`; also written `n!`. Refused (`too_large`) when
the result passes 16,384 bits (about `n` = 1,700).

Example: `factorial(10)` gives `3628800`.

### binomial(n, k)
`n` choose `k`: the number of `k`-element subsets of `n` items. `0` when `k`
is negative or above `n`.

Example: `binomial(49, 6)` gives `13983816`.

### permutations(n, k)
`n!/(n-k)!`: the number of ordered arrangements of `k` of `n` items. `0` when
`k` is above `n`.

Example: `permutations(10, 3)` gives `720`.

### fib(n)
The `n`-th Fibonacci number, `fib(0)` = 0, `fib(1)` = 1.

Example: `fib(90)` gives `2880067194370816120`.

## Number theory

### gcd(a, b, ...) or gcd(list)
The greatest common divisor of the arguments, or of one list.

Example: `gcd(1071, 462)` gives `21`.

### lcm(a, b, ...) or lcm(list)
The least common multiple.

Example: `lcm([4, 6, 10])` gives `60`.

### isprime(n)
Whether `n` is prime. Certain below 2^64; above, the chance of a wrong
`true` is under 4^-20. `n` must be below 2^1024.

Example: `isprime(2^61 - 1)` gives `true`.

### nextprime(n)
The smallest prime greater than `n`. `n` must be below 2^1023.

Example: `nextprime(1000)` gives `1009`.

### factor(n)
The prime factors of `n` with multiplicity, ascending; a negative `n` starts
with `-1`. `|n|` must be below 2^64; `factor(0)` is refused.

Example: `factor(360)` gives `[2, 2, 2, 3, 3, 5]`.

### divisors(n)
All positive divisors of `n`, ascending. `|n|` must be below 2^64;
`divisors(0)` is refused.

Example: `divisors(28)` gives `[1, 2, 4, 7, 14, 28]`.

### modpow(b, e, m)
`b^e mod m` without the huge intermediate power. A negative `e` uses the
modular inverse. `m` must be positive; `e` and `m` are limited to 4,096 bits.

Example: `modpow(2, 100, 1000000007)` gives `976371285`.

### modinv(a, m)
The `x` with `a*x = 1 (mod m)`. Refused when `a` and `m` are not coprime.

Example: `modinv(3, 11)` gives `4`.

## Bits and bases

### bitand(a, b)
Bitwise and. Negative numbers act as two's complement of infinite width.

Example: `bitand(12, 10)` gives `8`.

### bitor(a, b)
Bitwise or.

Example: `bitor(12, 10)` gives `14`.

### bitxor(a, b)
Bitwise exclusive or.

Example: `bitxor(12, 10)` gives `6`.

### bitnot(a)
Bitwise not: `-a - 1`.

Example: `bitnot(5)` gives `-6`.

### shl(a, n)
`a` shifted left `n` bits: `a * 2^n`. `n` must not be negative.

Example: `shl(1, 10)` gives `1024`.

### shr(a, n)
`a` shifted right `n` bits: `floor(a / 2^n)`.

Example: `shr(-8, 1)` gives `-4`.

### base(x, radix)
The integer `x` written in radix 2 to 36, as a string with lowercase
letters.

Example: `base(255, 16)` gives `ff`.

### frombase(s, radix)
The integer that the string `s` of radix 2 to 36 digits stands for. You can
also write hex, binary and octal literals directly: `0xff`, `0b1010`, `0o17`.

Example: `frombase("777", 8)` gives `511`.

## Examples

Lottery odds, 6 numbers out of 49:

```json
{"expr": "binomial(49, 6)"}
```

Result: 13983816

The probability of a full house in a 5-card hand, as an exact fraction (`fraction` is `"6/4165"`):

```json
{"expr": "13 * binomial(4, 3) * 12 * binomial(4, 2) / binomial(52, 5)"}
```

Result: 0.0014405762304922

Is a Mersenne number prime, and what divides one that is not:

```json
{"expr": "[isprime(2^31 - 1), factor(2^32 - 1)]"}
```

Result: [true, [3, 5, 17, 257, 65537]]

A float is refused where an exact integer is needed, even a whole one:

```json
{"expr": "factor(log2(1024))"}
```

Result: error type
