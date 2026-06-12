/**
 * Per-key async serialisation (plan §3.7(2) — "no torn JSON").
 *
 * Atomic `rename` already guarantees a reader never sees a half-written file.
 * This lock adds the second half of the guarantee: a read-modify-write mutation
 * (read `task.json`, apply a patch, write it back) is serialised per task id so
 * two concurrent mutations cannot both read the same base and lose one update.
 * Distinct keys run fully in parallel.
 */

/** A keyed FIFO mutex. Each key serialises its critical sections. */
export class KeyedMutex {
  private tails = new Map<string, Promise<unknown>>();

  /** Runs `fn` once all earlier acquisitions of `key` have settled. */
  async run<T>(key: string, fn: () => Promise<T>): Promise<T> {
    const prev = this.tails.get(key) ?? Promise.resolve();
    // Chain onto the tail regardless of whether the prior critical section
    // resolved or rejected.
    const result = prev.then(fn, fn);
    // The tail later callers await: never rejects (so one failure does not
    // poison the chain) and settles only after `result` does.
    const tail = result.then(
      () => {},
      () => {},
    );
    this.tails.set(key, tail);
    try {
      return await result;
    } finally {
      // If nobody chained after us, drop the entry to bound memory.
      if (this.tails.get(key) === tail) this.tails.delete(key);
    }
  }
}
