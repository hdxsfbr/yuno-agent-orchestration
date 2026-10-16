import { ApiError } from "./api";

/** React Query's retry rule: one more try, except for a refused token, which fails the same way again. */
export const shouldRetry = (failures: number, error: unknown): boolean => failures < 1 && !(error instanceof ApiError && error.status === 401);
