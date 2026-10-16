import { Suspense, lazy } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { NavLink, Navigate, Route, Routes } from "react-router";
import Button from "./components/Button";
import { Loading } from "./components/QueryState";
import SignInDialog from "./components/SignInDialog";
import { clearToken, useToken } from "./lib/auth";
import { usePendingApprovals, useApprovalEvents } from "./lib/useApprovals";
import AgentFormPage from "./pages/AgentFormPage";
import AgentsPage from "./pages/AgentsPage";
import ApprovalsPage from "./pages/ApprovalsPage";
import ChannelsPage from "./pages/ChannelsPage";
import NotFoundPage from "./pages/NotFoundPage";
import RunDetailPage from "./pages/RunDetailPage";
import RunsPage from "./pages/RunsPage";
import SchedulesPage from "./pages/SchedulesPage";
import SkillsPage from "./pages/SkillsPage";
import StatusPage from "./pages/StatusPage";
import WorkflowsPage from "./pages/WorkflowsPage";

// The builder carries the graph library; load it only when a workflow is opened.
const WorkflowBuilderPage = lazy(() => import("./pages/WorkflowBuilderPage"));
const builder = (
  <Suspense fallback={<Loading label="Loading the builder…" />}>
    <WorkflowBuilderPage />
  </Suspense>
);

const nav = [
  { to: "/agents", label: "Agents" },
  { to: "/workflows", label: "Workflows" },
  { to: "/runs", label: "Runs" },
  { to: "/approvals", label: "Approvals" },
  { to: "/channels", label: "Channels" },
  { to: "/schedules", label: "Schedules" },
  { to: "/skills", label: "Skills" },
  { to: "/status", label: "Status" },
];

/** How many requests wait for a decision, kept current by the event stream. */
function PendingCount() {
  const pending = usePendingApprovals();
  useApprovalEvents();
  const n = pending.data?.length ?? 0;
  if (n === 0) return null;
  return (
    <span aria-label={`${n} pending`} className="ml-auto rounded-full bg-amber-500 px-2 py-0.5 text-xs font-medium tabular-nums text-white">
      {n}
    </span>
  );
}

/** Offered only while a token is stored; signing out drops what the signed-in view loaded. */
function SignOut() {
  const qc = useQueryClient();
  const token = useToken();
  if (!token) return null;
  return (
    <Button
      variant="ghost"
      className="w-full justify-start"
      onClick={() => {
        clearToken();
        void qc.resetQueries();
      }}
    >
      Sign out
    </Button>
  );
}

export default function App() {
  return (
    <div className="flex min-h-screen">
      <SignInDialog />
      <aside className="flex w-56 shrink-0 flex-col border-r border-zinc-200 bg-white p-4 dark:border-zinc-800 dark:bg-zinc-900">
        <div className="mb-6 text-lg font-semibold">Yuno Agents</div>
        <nav className="flex flex-col gap-1">
          {nav.map((n) => (
            <NavLink
              key={n.to}
              to={n.to}
              className={({ isActive }) =>
                `flex items-center gap-2 rounded px-3 py-2 text-sm ${isActive ? "bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900" : "hover:bg-zinc-100 dark:hover:bg-zinc-800"}`
              }
            >
              {n.label}
              {n.to === "/approvals" ? <PendingCount /> : null}
            </NavLink>
          ))}
        </nav>
        <div className="mt-auto pt-6">
          <SignOut />
        </div>
      </aside>
      <main className="min-w-0 flex-1 p-6">
        <Routes>
          <Route path="/" element={<Navigate to="/agents" replace />} />
          <Route path="/agents" element={<AgentsPage />} />
          <Route path="/agents/new" element={<AgentFormPage />} />
          <Route path="/agents/:id" element={<AgentFormPage />} />
          <Route path="/workflows" element={<WorkflowsPage />} />
          <Route path="/workflows/new" element={builder} />
          <Route path="/workflows/:id" element={builder} />
          <Route path="/runs" element={<RunsPage />} />
          <Route path="/runs/:id" element={<RunDetailPage />} />
          <Route path="/approvals" element={<ApprovalsPage />} />
          <Route path="/channels" element={<ChannelsPage />} />
          <Route path="/channels/:conversationId" element={<ChannelsPage />} />
          <Route path="/schedules" element={<SchedulesPage />} />
          <Route path="/skills" element={<SkillsPage />} />
          <Route path="/status" element={<StatusPage />} />
          <Route path="*" element={<NotFoundPage />} />
        </Routes>
      </main>
    </div>
  );
}
