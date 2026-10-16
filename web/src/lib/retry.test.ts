import { describe, expect, it } from "vitest";
import { ApiError } from "./api";
import { shouldRetry } from "./retry";

describe("shouldRetry", () => {
  it("tries a failed query once more", () => {
    expect(shouldRetry(0, new ApiError(500, "internal", "boom"))).toBe(true);
    expect(shouldRetry(0, new Error("network"))).toBe(true);
    expect(shouldRetry(1, new Error("network"))).toBe(false);
  });

  it("does not retry a refused token, which the sign-in dialog resolves", () => {
    expect(shouldRetry(0, new ApiError(401, "unauthorized", "missing or invalid bearer token"))).toBe(false);
  });
});
