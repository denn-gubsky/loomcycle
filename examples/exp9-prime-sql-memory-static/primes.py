#!/usr/bin/env python3
"""
Sieve of Eratosthenes — prints all primes up to N (inclusive), one per line.
Usage: python3 primes.py [N]   (default N=500)

Designed to be consumed via stdio by a loomcycle agent (exp9).
Each prime is emitted immediately so the agent can ingest the stream.
"""
import sys

def sieve(limit: int):
    if limit < 2:
        return
    is_prime = bytearray([1]) * (limit + 1)
    is_prime[0] = is_prime[1] = 0
    for i in range(2, int(limit ** 0.5) + 1):
        if is_prime[i]:
            is_prime[i * i :: i] = bytearray(len(is_prime[i * i :: i]))
    for n, flag in enumerate(is_prime):
        if flag:
            print(n, flush=True)

if __name__ == "__main__":
    limit = int(sys.argv[1]) if len(sys.argv) > 1 else 500
    sieve(limit)
