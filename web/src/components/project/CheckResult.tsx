import { useState } from "react";
import { checkProject, type Project } from "@/lib/api";
import { checkPassed } from "@/lib/project";
import { errorText } from "../QueryState";

/** What a project's Check last found; `at` is the server's time of the check, when it gave one. */
export type CheckState = { running: true } | { running: false; good: boolean; message: string; at?: string };

const toneClass = {
  good: "border-emerald-300 bg-emerald-50 text-emerald-900 dark:border-emerald-800 dark:bg-emerald-950/40 dark:text-emerald-200",
  bad: "border-red-200 bg-red-50 text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300",
};

/** Whether the project changed after the check ran, which makes the result out of date. */
const outdated = (p: Project, c: Extract<CheckState, { running: false }>): boolean => {
  const changed = Date.parse(p.updated_at);
  const checked = c.at ? Date.parse(c.at) : NaN;
  return !Number.isNaN(changed) && !Number.isNaN(checked) && changed > checked;
};

/** The result sentence of a check, green when the project is ready to use and red when it is not. */
export function CheckNote({ result }: { result: Extract<CheckState, { running: false }> }) {
  return (
    <p role="status" className={`rounded-md border p-2.5 text-sm ${result.good ? toneClass.good : toneClass.bad}`}>
      {result.message}
    </p>
  );
}

/** The results of checking projects, kept by project id while the page is open. */
export function useProjectChecks() {
  const [checks, setChecks] = useState<Record<string, CheckState>>({});

  const check = async (p: Project) => {
    setChecks((c) => ({ ...c, [p.id]: { running: true } }));
    try {
      const result = await checkProject(p.id);
      setChecks((c) => ({ ...c, [p.id]: { running: false, good: checkPassed(result), message: result.message, at: result.checked_at } }));
    } catch (err) {
      setChecks((c) => ({ ...c, [p.id]: { running: false, good: false, message: `Could not check the project: ${errorText(err)}` } }));
    }
  };

  const forget = (id: string) => setChecks((c) => Object.fromEntries(Object.entries(c).filter(([key]) => key !== id)));

  /** The project's finished result, unless the project changed after it ran. */
  const shownFor = (p: Project) => {
    const result = checks[p.id];
    return result && !result.running && !outdated(p, result) ? result : undefined;
  };

  return { running: (id: string) => !!checks[id]?.running, check, forget, shownFor };
}
