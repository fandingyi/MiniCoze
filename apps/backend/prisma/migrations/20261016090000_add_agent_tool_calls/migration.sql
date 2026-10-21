CREATE TABLE "AgentToolCall" (
  "runId" TEXT NOT NULL REFERENCES "AgentRun"("id") ON DELETE CASCADE ON UPDATE CASCADE,
  "callId" TEXT NOT NULL,
  "name" TEXT NOT NULL,
  "requestHash" TEXT NOT NULL,
  "arguments" JSONB NOT NULL,
  "status" TEXT NOT NULL CHECK ("status" IN ('SUCCESS','FAILED')),
  "output" JSONB NOT NULL,
  "errorCode" TEXT NOT NULL,
  "createdAt" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT "AgentToolCall_pkey" PRIMARY KEY ("runId","callId")
);
