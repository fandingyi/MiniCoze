package platform

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PublicRateLimitError is deliberately distinct from admission control: the
// former is a durable per-publish-channel quota, while admission protects the
// runtime from concurrently active work.
type PublicRateLimitError struct {
	RetryAfter time.Duration
}

func (e *PublicRateLimitError) Error() string { return "public channel rate limit exceeded" }

type publicQuotaBucket struct {
	Name       string
	Limit      int
	RetryAfter time.Duration
}

func publicQuotaBuckets(access *PublicAccess, now time.Time) []publicQuotaBucket {
	if access == nil || access.APIHash == "" {
		return nil
	}
	minuteEnd := now.UTC().Truncate(time.Minute).Add(time.Minute)
	dayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	return []publicQuotaBucket{
		{Name: "minute", Limit: access.RateLimitPerMinute, RetryAfter: minuteEnd.Sub(now)},
		{Name: "day", Limit: access.RateLimitPerDay, RetryAfter: dayStart.AddDate(0, 0, 1).Sub(now)},
	}
}

// acquirePublicQuota inserts or increments one UTC bucket atomically. It runs
// in BeginRun's transaction, so retries, validation failures, and later insert
// failures never consume a quota slot.
func acquirePublicQuota(ctx context.Context, tx *sql.Tx, access *PublicAccess) error {
	if access == nil || access.APIHash == "" {
		return nil
	}
	// Database time keeps every Go instance on the same bucket boundary and
	// lets Retry-After describe the remaining part of that actual window.
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT CURRENT_TIMESTAMP AT TIME ZONE 'UTC'`).Scan(&now); err != nil {
		return fmt.Errorf("read public quota clock: %w", err)
	}
	for _, bucket := range publicQuotaBuckets(access, now) {
		if bucket.Limit == 0 {
			continue
		}
		var count int
		err := tx.QueryRowContext(ctx, `INSERT INTO "PublishChannelQuota"("channelId","bucket","bucketStart","count")
			VALUES($1,$2,date_trunc($2,$3),1)
			ON CONFLICT ("channelId","bucket","bucketStart") DO UPDATE
			SET "count"="PublishChannelQuota"."count"+1
			WHERE "PublishChannelQuota"."count" < $4
			RETURNING "count"`, access.ChannelID, bucket.Name, now, bucket.Limit).Scan(&count)
		if err == sql.ErrNoRows {
			return &PublicRateLimitError{RetryAfter: bucket.RetryAfter}
		}
		if err != nil {
			return fmt.Errorf("acquire public %s quota: %w", bucket.Name, err)
		}
	}
	return nil
}

// PrunePublicQuotas retains enough history for operational diagnosis without
// letting the high-cardinality minute buckets grow indefinitely.
func (p Postgres) PrunePublicQuotas(ctx context.Context) (int64, error) {
	result, err := p.DB.ExecContext(ctx, `DELETE FROM "PublishChannelQuota"
		WHERE ("bucket"='minute' AND "bucketStart" < date_trunc('day', CURRENT_TIMESTAMP AT TIME ZONE 'UTC') - interval '2 days')
		   OR ("bucket"='day' AND "bucketStart" < date_trunc('day', CURRENT_TIMESTAMP AT TIME ZONE 'UTC') - interval '90 days')`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
