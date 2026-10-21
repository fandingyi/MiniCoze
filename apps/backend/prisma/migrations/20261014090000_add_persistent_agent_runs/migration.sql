CREATE TYPE "AgentRunStatus" AS ENUM ('CREATED', 'RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED');

CREATE TABLE "AgentRun" (
  "id" TEXT NOT NULL PRIMARY KEY,
  "conversationId" TEXT NOT NULL REFERENCES "Conversation"("id") ON DELETE CASCADE ON UPDATE CASCADE,
  "agentId" TEXT NOT NULL,
  "userId" TEXT NOT NULL,
  "workspaceId" TEXT NOT NULL,
  "requestKey" TEXT NOT NULL,
  "requestHash" TEXT NOT NULL,
  "mode" TEXT NOT NULL CHECK ("mode" IN ('draft', 'published')),
  "agentVersionId" TEXT,
  "snapshot" JSONB NOT NULL,
  "history" JSONB NOT NULL,
  "inputText" TEXT NOT NULL,
  "status" "AgentRunStatus" NOT NULL DEFAULT 'CREATED',
  "usage" JSONB,
  "errorCode" TEXT,
  "createdAt" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
  "updatedAt" TIMESTAMP(3) NOT NULL,
  "startedAt" TIMESTAMP(3),
  "finishedAt" TIMESTAMP(3),
  "deadlineAt" TIMESTAMP(3) NOT NULL,
  CONSTRAINT "AgentRun_terminal_time_check" CHECK (
    ("status" IN ('COMPLETED','FAILED','CANCELLED')) = ("finishedAt" IS NOT NULL)
  )
);
CREATE UNIQUE INDEX "AgentRun_userId_requestKey_key" ON "AgentRun"("userId","requestKey");
CREATE INDEX "AgentRun_conversationId_createdAt_idx" ON "AgentRun"("conversationId","createdAt");
CREATE INDEX "AgentRun_status_deadlineAt_idx" ON "AgentRun"("status","deadlineAt");
CREATE UNIQUE INDEX "AgentRun_active_conversation_key" ON "AgentRun"("conversationId") WHERE "status" IN ('CREATED','RUNNING');

ALTER TABLE "Message" ADD COLUMN "runId" TEXT;
ALTER TABLE "Message" ADD CONSTRAINT "Message_runId_fkey" FOREIGN KEY ("runId") REFERENCES "AgentRun"("id") ON DELETE SET NULL ON UPDATE CASCADE;
CREATE UNIQUE INDEX "Message_runId_role_key" ON "Message"("runId","role");

CREATE FUNCTION "guard_agent_run_update"() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF ROW(NEW."conversationId", NEW."agentId", NEW."userId", NEW."workspaceId", NEW."requestKey", NEW."requestHash", NEW."mode", NEW."agentVersionId", NEW."snapshot", NEW."history", NEW."inputText", NEW."deadlineAt")
    IS DISTINCT FROM ROW(OLD."conversationId", OLD."agentId", OLD."userId", OLD."workspaceId", OLD."requestKey", OLD."requestHash", OLD."mode", OLD."agentVersionId", OLD."snapshot", OLD."history", OLD."inputText", OLD."deadlineAt") THEN
    RAISE EXCEPTION 'run input and snapshot are immutable';
  END IF;
  IF OLD."status" IN ('COMPLETED','FAILED','CANCELLED') AND NEW IS DISTINCT FROM OLD THEN
    RAISE EXCEPTION 'run terminal state is immutable';
  END IF;
  IF OLD."status" = 'RUNNING' AND NEW."status" = 'CREATED' THEN
    RAISE EXCEPTION 'run cannot move backwards';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER "AgentRun_guard_update" BEFORE UPDATE ON "AgentRun" FOR EACH ROW EXECUTE FUNCTION "guard_agent_run_update"();
