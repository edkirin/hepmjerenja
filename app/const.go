package main

import "time"

const (
	// DailyCollectionHour and DailyCollectionMinute define when the daily
	// collection job runs (00:30 local time) to pick up the previous day's
	// finalized meter readings.
	DailyCollectionHour   = 6
	DailyCollectionMinute = 0

	// StaleReadingThreshold is how old last_meter_reading_collection must be
	// before the minute-tick staleness check triggers a re-collection.
	// Set to 6 h so that if HEP publishes previous-day readings after the
	// daily job ran, they are picked up within the same morning.
	StaleReadingThreshold = 6 * time.Hour

	// GeocodingBackoff is how long to wait before retrying a failed geocoding
	// attempt for a metering point.
	GeocodingBackoff = 1 * time.Hour

	// HepConcurrentFetches is the maximum number of HEP API month-fetch requests
	// that may run in parallel for a single metering point during backfill.
	HepConcurrentFetches = 3

	// WorkerBatchSize is the number of metering points the background worker
	// fetches concurrently within each batch. Batches run sequentially to
	// avoid hammering the HEP server with a large parallel spike.
	WorkerBatchSize = 3

)
