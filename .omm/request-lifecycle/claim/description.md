ClaimQueuedRun moves the oldest queued run to running for this worker with FOR UPDATE SKIP LOCKED; a run paused at an approval node comes back through the same queue when decided.
