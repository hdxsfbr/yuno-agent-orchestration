// Shared test setup (vite.config.ts `setupFiles`). The CI runner is several times slower
// than a workstation, so the asynchronous testing-library helpers (findBy*, waitFor) get a
// longer deadline than their one-second default; a test that is truly stuck still fails,
// at vitest's test timeout.
import { configure } from "@testing-library/react";

configure({ asyncUtilTimeout: 8000 });
