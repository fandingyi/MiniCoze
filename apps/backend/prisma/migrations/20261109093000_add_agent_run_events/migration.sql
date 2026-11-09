-- Events are an immutable, ordered SSE delivery log. They intentionally live
-- with AgentRun so deleting a conversation removes its reconnect history too.
CREATE TABLE "AgentRunEvent" (
  "runId" TEXT NOT NULL REFERENCES "AgentRun"("id") ON DELETE CASCADE ON UPDATE CASCADE,
  "sequence" INTEGER NOT NULL CHECK ("sequence" > 0),
  "type" TEXT NOT NULL CHECK (char_length("type") BETWEEN 1 AND 64),
  "payload" JSONB NOT NULL,
  "createdAt" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY ("runId", "sequence")
);

CREATE INDEX "AgentRunEvent_runId_createdAt_idx" ON "AgentRunEvent"("runId", "createdAt");
