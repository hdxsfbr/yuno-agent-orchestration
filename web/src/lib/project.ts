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
