import { Link } from "react-router";
import { buttonClass } from "@/components/Button";

export default function NotFoundPage() {
  return (
    <div className="space-y-3">
      <h1 className="text-xl font-semibold">Page not found</h1>
      <p className="text-sm text-zinc-500">There is nothing at this address.</p>
      <Link to="/agents" className={buttonClass("secondary")}>
        Go to agents
      </Link>
    </div>
  );
}
