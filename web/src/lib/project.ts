// Display helpers for projects (DESIGN 5.17). Pure functions so they can be unit tested.

import type { ProjectCheck, ProjectProvider, Toolchain } from "./api";

export const PROVIDER_LABEL: Record<ProjectProvider, string> = { gitlab: "GitLab", github: "GitHub" };

/** "GitLab" or "GitHub"; an unexpected value is shown as it is. */
export const providerLabel = (provider: string): string => PROVIDER_LABEL[provider as ProjectProvider] ?? provider;

export const TOOLCHAIN_LABEL: Record<Toolchain, string> = { go: "Go", node: "Node", python: "Python", default: "default image" };

/** "Go", "Node", "Python" or "default image"; an unexpected value is shown as it is. */
export const toolchainLabel = (toolchain: string): string => TOOLCHAIN_LABEL[toolchain as Toolchain] ?? toolchain;

/** A repository URL as host and path: https://gitlab.com/group/project becomes gitlab.com/group/project. */
export function repoDisplay(url: string): string {
  try {
    const u = new URL(url);
    const path = u.pathname.replace(/\/+$/, "");
    return `${u.host}${path}`;
  } catch {
    return url;
  }
}

/** A check is good when the token was accepted, the token can push and the default branch exists; an answer that leaves the last two out does not count against it. */
export const checkPassed = (c: Pick<ProjectCheck, "ok" | "can_push" | "branch_exists">): boolean =>
  c.ok && c.can_push !== false && c.branch_exists !== false;

// ---------------------------------------------------------------------------------------
// Budgets (DESIGN 5.19). Spend and caps are USD floats in the API with up to six decimals,
// so comparisons run on whole micro-dollars.
// ---------------------------------------------------------------------------------------

/** How a window's spend stands against its cap: no cap, under 80 %, from 80 %, at or over the cap. */
export type BudgetTone = "none" | "ok" | "warning" | "danger";

const toMicros = (usd: number): number => Math.round(usd * 1_000_000);

export function budgetTone(spentUsd: number | null | undefined, capUsd: number | null | undefined): BudgetTone {
  if (capUsd == null || !Number.isFinite(capUsd)) return "none";
  const cap = toMicros(capUsd);
  const spent = toMicros(Number.isFinite(spentUsd) ? (spentUsd as number) : 0);
  if (spent >= cap) return "danger";
  if (spent * 5 >= cap * 4) return "warning";
  return "ok";
}

/** The text colour of a spend figure in each tone. */
export const BUDGET_TEXT: Record<BudgetTone, string> = {
  none: "text-zinc-500",
  ok: "",
  warning: "font-medium text-amber-700 dark:text-amber-400",
  danger: "font-medium text-red-700 dark:text-red-400",
};

/** Said on hover where a colour alone would carry the meaning. */
export const BUDGET_TONE_TITLE: Record<BudgetTone, string | undefined> = {
  none: undefined,
  ok: undefined,
  warning: "80 % or more of the cap is used",
  danger: "The cap is reached; new runs are refused",
};

/** The form text of a cap: empty when there is none. */
export const capText = (cap: number | null | undefined): string => (cap == null ? "" : String(cap));

export type CapParse = { ok: true; value: number | null } | { ok: false; message: string };

const DECIMAL = /^-?(\d+\.?\d*|\.\d+)$/;

/** A cap typed in the form: empty means no cap (null), anything else must be a number above 0. */
export function parseCap(text: string): CapParse {
  const t = text.trim();
  if (t === "") return { ok: true, value: null };
  if (!DECIMAL.test(t)) return { ok: false, message: "Enter an amount in dollars, like 5 or 12.50" };
  const n = Number(t);
  if (!Number.isFinite(n) || n <= 0) return { ok: false, message: "Must be greater than 0" };
  return { ok: true, value: n };
}
