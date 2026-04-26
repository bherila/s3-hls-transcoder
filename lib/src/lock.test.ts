import { describe, expect, it } from "vitest";
import { computeBudgetSeconds, computeLockTtlSeconds } from "./lock.js";

describe("computeLockTtlSeconds", () => {
  it("multiplies and ceils", () => {
    expect(computeLockTtlSeconds(900, 1.5)).toBe(1350);
    expect(computeLockTtlSeconds(900, 1)).toBe(900);
    expect(computeLockTtlSeconds(100, 1.05)).toBe(105);
  });

  it("never returns less than maxRuntime when multiplier=1", () => {
    expect(computeLockTtlSeconds(3600, 1)).toBe(3600);
  });
});

describe("computeBudgetSeconds", () => {
  it("multiplies and floors", () => {
    expect(computeBudgetSeconds(900, 0.75)).toBe(675);
    expect(computeBudgetSeconds(3600, 0.75)).toBe(2700);
  });

  it("returns 0 if multiplier is 0", () => {
    expect(computeBudgetSeconds(900, 0)).toBe(0);
  });
});
