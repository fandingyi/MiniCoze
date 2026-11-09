CREATE TABLE "AgentModelRound" (
  "runId" TEXT NOT NULL REFERENCES "AgentRun"("id") ON DELETE CASCADE ON UPDATE CASCADE,
  "round" INTEGER NOT NULL CHECK ("round" BETWEEN 1 AND 5),
  "durationMs" INTEGER NOT NULL CHECK ("durationMs" BETWEEN 0 AND 120000),
  "finishReason" TEXT NOT NULL CHECK ("finishReason" IN ('stop','length','tool_calls')),
  "usage" JSONB,
  CONSTRAINT "AgentModelRound_pkey" PRIMARY KEY ("runId","round")
);
