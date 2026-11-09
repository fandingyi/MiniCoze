CREATE TABLE "PublishChannelQuota" (
  "channelId" TEXT NOT NULL REFERENCES "PublishChannel"("id") ON DELETE CASCADE ON UPDATE CASCADE,
  "bucket" TEXT NOT NULL CHECK ("bucket" IN ('minute', 'day')),
  "bucketStart" TIMESTAMP(3) NOT NULL,
  "count" INTEGER NOT NULL DEFAULT 0 CHECK ("count" >= 0),
  CONSTRAINT "PublishChannelQuota_pkey" PRIMARY KEY ("channelId", "bucket", "bucketStart")
);

CREATE INDEX "PublishChannelQuota_bucket_bucketStart_idx" ON "PublishChannelQuota"("bucket", "bucketStart");
