package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// ──────────────────────────────────────────────
// Token cache — stores the HEP JWT so we don't
// re-authenticate on every collection cycle.
// ──────────────────────────────────────────────

// ErrAuthFailed is returned by loginAndCache when the HEP login endpoint rejects
// the credentials (wrong username/password). Distinct from ErrUnauthorized, which
// signals an expired JWT during a readings fetch.
var ErrAuthFailed = fmt.Errorf("authentication failed")

// tokenCache is a concurrent-safe holder for the HEP API JWT token. The token is
// reused across collection cycles and only refreshed when it expires (detected
// via HTTP 401 responses) or when no token has been obtained yet.
//
// A single cache is shared by the periodic worker and the manual-fetch worker.
// HEP permits one active session per account, so a second login would invalidate
// the session the other worker is using — sharing the cache keeps them on one
// session, and loginMu collapses concurrent refreshes into a single login.
type tokenCache struct {
	mu    sync.RWMutex
	token string

	loginMu sync.Mutex
}

func newTokenCache() *tokenCache {
	return &tokenCache{}
}

// Get returns the cached token and true if one is present.
func (tc *tokenCache) Get() (string, bool) {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	return tc.token, tc.token != ""
}

// Set stores or replaces the cached token.
func (tc *tokenCache) Set(token string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.token = token
}

// Clear discards the cached token, forcing a fresh login on the next attempt.
func (tc *tokenCache) Clear() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.token = ""
}

// ──────────────────────────────────────────────
// Fetch state — the outcome of the most recent
// collection attempt, surfaced in the web UI.
// ──────────────────────────────────────────────

// fetchState records why the last collection attempt failed. Single-user app:
// one value in memory is enough, so nothing is persisted to the database.
type fetchState struct {
	mu      sync.RWMutex
	lastErr string
}

// FetchState holds the collection status shared by the worker and the handlers.
var FetchState = &fetchState{}

// Set stores err as the last collection failure; a nil err clears it.
func (f *fetchState) Set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		f.lastErr = ""
		return
	}
	f.lastErr = err.Error()
}

// LastError returns the last collection failure message, or nil if the most
// recent attempt succeeded (or none has run yet).
func (f *fetchState) LastError() *string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.lastErr == "" {
		return nil
	}
	msg := f.lastErr
	return &msg
}

// ──────────────────────────────────────────────
// Worker — background goroutine that periodically
// collects meter readings from the HEP API.
// ──────────────────────────────────────────────

// collectMode selects which metering points a collection run processes.
type collectMode int

const (
	// modePending processes points that have never been collected — a full
	// historical backfill from the point's available_from date.
	modePending collectMode = iota
	// modeStale processes points whose last successful collection is older than
	// StaleReadingThreshold (safety net for a missed daily job).
	modeStale
	// modeAll processes every metering point.
	modeAll
)

// StartManualFetchWorker consumes one-off fetch requests submitted by HTTP
// handlers and runs a collection for each. It runs independently of the periodic
// background worker, whose only job is scheduled collection.
func StartManualFetchWorker(ctx context.Context, db *DB, hepClient *HepClient, cache *tokenCache, creds HepCredentials, logger zerolog.Logger, manualFetchCh <-chan ManualFetchRequest) {
	log := logger.With().Str("component", "manual-fetch").Logger()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("shutting down")
			return

		case req := <-manualFetchCh:
			// The result is sent back on req.ResultCh so the HTTP handler can
			// return a real success/error response.
			refTime := req.RefTime
			if refTime.IsZero() {
				refTime = time.Now()
			}
			log.Info().Time("ref_time", refTime).Msg("manual fetch triggered")
			go func(resultCh chan error, refTime time.Time) {
				err := runCollection(ctx, db, hepClient, cache, creds, modeAll, true, refTime, log)
				if err != nil {
					log.Error().Err(err).Msg("manual fetch failed")
				}
				// Record the outcome like a scheduled run does, so a failure is
				// also explained on the monthly view and a success clears it.
				FetchState.Set(err)
				resultCh <- err
			}(req.ResultCh, refTime)
		}
	}
}

// StartWorker runs the background data collection loop with two jobs:
//
//  1. Every minute: backfill metering points that have never been collected, and
//     re-collect points whose last collection is older than StaleReadingThreshold.
//
//  2. Daily at DailyCollectionHour:DailyCollectionMinute: refresh current-month
//     data for every metering point to pick up finalized readings. Uses a date
//     guard so it fires exactly once per calendar day.
//
// The token cache persists across ticks. The function blocks until ctx is
// cancelled (application shutdown).
func StartWorker(ctx context.Context, db *DB, hepClient *HepClient, cache *tokenCache, creds HepCredentials, logger zerolog.Logger) {
	log := logger.With().Str("component", "worker").Logger()

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// lastDailyDate tracks which calendar date the daily job last ran on,
	// preventing duplicate runs if the ticker fires multiple times after the
	// collection time. Initialised to today if we're already past that time so
	// app restarts during the day don't trigger an extra daily run. The
	// stale-readings check (every minute) handles any resulting data gap.
	lastDailyDate := ""
	{
		now := time.Now()
		isAfterDailyTime := now.Hour() > DailyCollectionHour ||
			(now.Hour() == DailyCollectionHour && now.Minute() >= DailyCollectionMinute)
		if isAfterDailyTime {
			lastDailyDate = now.Format("2006-01-02")
		}
	}

	// Run immediately on start: discover metering points and backfill anything
	// that has never been collected.
	runAndReport(ctx, db, hepClient, cache, creds, modePending, true, log)

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("shutting down")
			return

		case <-ticker.C:
			// Every minute: backfill points that have never been collected, then
			// re-collect stale ones. Neither contacts HEP when there is no work.
			runAndReport(ctx, db, hepClient, cache, creds, modePending, false, log)
			runAndReport(ctx, db, hepClient, cache, creds, modeStale, false, log)

			// Daily: refresh current-month data for every metering point.
			// The date guard ensures this runs at most once per calendar day.
			now := time.Now()
			isCollectionTime := now.Hour() > DailyCollectionHour || (now.Hour() == DailyCollectionHour && now.Minute() >= DailyCollectionMinute)
			today := now.Format("2006-01-02")
			if isCollectionTime && today != lastDailyDate {
				lastDailyDate = today
				log.Info().Str("date", today).Msg("running daily collection")
				runAndReport(ctx, db, hepClient, cache, creds, modeAll, true, log)
			}
		}
	}
}

// runAndReport runs a collection and records the outcome in FetchState so the
// web interface can show the last failure. refTime is always yesterday: readings
// are finalized with a delay, so the previous day is the newest useful reference.
func runAndReport(ctx context.Context, db *DB, hepClient *HepClient, cache *tokenCache, creds HepCredentials, mode collectMode, discover bool, log zerolog.Logger) {
	err := runCollection(ctx, db, hepClient, cache, creds, mode, discover, time.Now().AddDate(0, 0, -1), log)
	if err != nil {
		log.Error().Err(err).Msg("collection failed")
	}
	FetchState.Set(err)
}

// runCollection performs one collection pass:
//
//  1. Read the known metering points and select those matching mode.
//  2. Log in to HEP when there is work to do (or when discover is set and no
//     metering points are known yet) — the login response is also what tells us
//     which metering points exist.
//  3. Fetch the outstanding months for each selected point, in sequential
//     batches of WorkerBatchSize with concurrency inside each batch.
//
// Returns an error only when the whole run could not proceed (login failure or a
// database error); per-point failures are logged and skipped.
func runCollection(ctx context.Context, db *DB, hepClient *HepClient, cache *tokenCache, creds HepCredentials, mode collectMode, discover bool, refTime time.Time, log zerolog.Logger) error {
	if !creds.IsSet() {
		return fmt.Errorf("HEP credentials are not configured (set HEP_USERNAME and HEP_PASSWORD)")
	}

	points, err := GetMeteringPoints(ctx, db)
	if err != nil {
		return fmt.Errorf("get metering points: %w", err)
	}

	targets := selectPoints(points, mode)
	// With no metering points stored yet we must log in to discover them.
	needsDiscovery := discover && len(points) == 0
	if len(targets) == 0 && !needsDiscovery {
		return nil
	}

	// Log in (also upserts the metering points from the login response and
	// geocodes new ones) unless a valid token is already cached. Discovery always
	// logs in, because the login response is what lists the metering points.
	if _, ok := cache.Get(); !ok || needsDiscovery {
		if _, err := login(ctx, db, hepClient, cache, creds, log); err != nil {
			return err
		}
		// Re-read: the login response may have added or updated points.
		points, err = GetMeteringPoints(ctx, db)
		if err != nil {
			return fmt.Errorf("get metering points: %w", err)
		}
		targets = selectPoints(points, mode)
	}

	if len(targets) == 0 {
		return nil
	}

	log.Info().Int("points", len(targets)).Msg("collecting metering points")

	// Per-point failures are collected and returned together so a caller (the
	// manual fetch handler, FetchState) reports a failure instead of a success
	// that stored nothing.
	var (
		errMu  sync.Mutex
		errs   []error
		failed int
	)

	totalBatches := (len(targets) + WorkerBatchSize - 1) / WorkerBatchSize
	for i := 0; i < len(targets); i += WorkerBatchSize {
		end := min(i+WorkerBatchSize, len(targets))
		batchNum := i/WorkerBatchSize + 1
		log.Info().Int("batch", batchNum).Int("total_batches", totalBatches).Int("items", end-i).Msg("processing batch")
		var wg sync.WaitGroup
		for _, point := range targets[i:end] {
			wg.Add(1)
			go func(p MeteringPoint) {
				defer wg.Done()
				if err := collectForMeteringPoint(ctx, db, hepClient, cache, creds, p, log, refTime); err != nil {
					errMu.Lock()
					errs = append(errs, fmt.Errorf("metering point %s: %w", p.Code, err))
					failed++
					errMu.Unlock()
				}
			}(point)
		}
		wg.Wait()
		log.Info().Int("batch", batchNum).Int("total_batches", totalBatches).Msg("batch done")
	}

	if len(errs) > 0 {
		log.Error().Int("points", len(targets)).Int("failed", failed).Msg("collection finished with errors")
		return errors.Join(errs...)
	}

	log.Info().Int("points", len(targets)).Msg("collection complete")
	return nil
}

// selectPoints filters metering points according to the collection mode.
func selectPoints(points []MeteringPoint, mode collectMode) []MeteringPoint {
	var selected []MeteringPoint
	for _, p := range points {
		switch mode {
		case modePending:
			if p.LastMeterReadingCollection == nil {
				selected = append(selected, p)
			}
		case modeStale:
			if p.LastMeterReadingCollection != nil &&
				time.Since(*p.LastMeterReadingCollection) > StaleReadingThreshold {
				selected = append(selected, p)
			}
		case modeAll:
			selected = append(selected, p)
		}
	}
	return selected
}

// ensureToken returns a usable token, logging in only when necessary. Pass the
// token that was just rejected as stale (or "" when there was none): if another
// goroutine has already replaced it, that fresh token is returned instead of
// performing a second login, which HEP would answer by invalidating the first.
func ensureToken(ctx context.Context, db *DB, hepClient *HepClient, cache *tokenCache, creds HepCredentials, stale string, log zerolog.Logger) (string, error) {
	cache.loginMu.Lock()
	defer cache.loginMu.Unlock()

	if cur, ok := cache.Get(); ok && cur != stale {
		log.Debug().Msg("token already refreshed by another collection, reusing")
		return cur, nil
	}
	return login(ctx, db, hepClient, cache, creds, log)
}

// login authenticates against the HEP API, caches the resulting JWT token, and
// upserts the metering points from the login response. On failure the cached
// token is removed to ensure a clean retry on the next cycle. Prefer
// ensureToken, which avoids redundant logins.
func login(ctx context.Context, db *DB, hepClient *HepClient, cache *tokenCache, creds HepCredentials, log zerolog.Logger) (string, error) {
	loginResp, err := hepClient.Login(ctx, creds.Username, creds.Password)
	if err != nil {
		// Clear any stale token on login failure to avoid retrying with bad credentials.
		cache.Clear()
		return "", fmt.Errorf("%w: %w", ErrAuthFailed, err)
	}

	cache.Set(loginResp.Token)
	log.Info().Str("user", creds.Username).Msg("logged in")

	// The login response includes the metering points — upsert them to keep our
	// database in sync with HEP's data (new points, updated flags, etc.).
	for _, kupac := range loginResp.KupacList {
		for _, omm := range kupac.OmmList {
			if err := UpsertMeteringPoint(ctx, db, omm); err != nil {
				log.Error().Err(err).Str("code", omm.Sifra).Msg("error upserting metering point")
			}
		}
	}

	// Geocode any metering points that don't yet have coordinates.
	// This is a one-time operation per point; subsequent calls check the DB first
	// and return immediately if coordinates are already stored.
	for _, kupac := range loginResp.KupacList {
		for _, omm := range kupac.OmmList {
			geocodePointIfNeeded(ctx, db, omm.Sifra, omm.Adresa, log)
		}
	}

	return loginResp.Token, nil
}

// monthYear is a helper struct representing a single month to fetch readings for.
type monthYear struct {
	Month int
	Year  int
}

// getMonthsToCollect returns a list of months that need readings collected for a
// given metering point. If the point has never been collected before
// (LastMeterReadingCollection is nil), it returns all months from AvailableFrom
// up to the current month — a full historical backfill. Otherwise it returns all
// months from the last collection month up to the current month so that any
// readings published late (e.g. end-of-month data) are always picked up.
func getMonthsToCollect(point MeteringPoint, refTime time.Time) []monthYear {
	now := refTime
	currentMonth := monthYear{Month: int(now.Month()), Year: now.Year()}

	if point.LastMeterReadingCollection != nil {
		last := *point.LastMeterReadingCollection
		lastMonth := monthYear{Month: int(last.Month()), Year: last.Year()}

		// Last collection is at or after the reference month — just fetch the reference
		// month. Covers the normal same-month case and the manual-fetch case where
		// refTime is yesterday but the DB already has a collection from today.
		lastAfterRef := lastMonth.Year > currentMonth.Year ||
			(lastMonth.Year == currentMonth.Year && lastMonth.Month >= currentMonth.Month)
		if lastAfterRef {
			return []monthYear{currentMonth}
		}

		// Last collection was in a previous month: re-fetch from that month forward
		// so that any readings published after that run (e.g. end-of-month data) are caught.
		var months []monthYear
		cursor := last
		for {
			m := monthYear{Month: int(cursor.Month()), Year: cursor.Year()}
			months = append(months, m)
			if m == currentMonth {
				break
			}
			cursor = time.Date(cursor.Year(), cursor.Month()+1, 1, 0, 0, 0, 0, cursor.Location())
			if cursor.After(now) {
				break
			}
		}
		return months
	}

	// First-time collection: backfill from the metering point's available_from date.
	// If available_from is not set, fall back to the current month only.
	if point.AvailableFrom == nil {
		return []monthYear{currentMonth}
	}

	var months []monthYear
	cursor := *point.AvailableFrom

	// Iterate month by month from available_from until we reach or pass the current month.
	for {
		m := monthYear{Month: int(cursor.Month()), Year: cursor.Year()}
		months = append(months, m)

		// Stop once we've reached the current month.
		if m.Year == currentMonth.Year && m.Month == currentMonth.Month {
			break
		}

		// Advance to the first day of the next month.
		cursor = time.Date(cursor.Year(), cursor.Month()+1, 1, 0, 0, 0, 0, cursor.Location())

		// Safety: stop if we've gone past the current month (shouldn't happen, but just in case).
		if cursor.After(now) {
			break
		}
	}

	return months
}

// collectForMeteringPoint fetches all required months of readings for a single
// metering point and updates its last_meter_reading_collection timestamp.
// Months within a point are fetched with up to HepConcurrentFetches in parallel.
// It returns an error when any month failed; in that case
// last_meter_reading_collection is left untouched so the next run retries the
// point instead of treating the gap as collected.
func collectForMeteringPoint(ctx context.Context, db *DB, hepClient *HepClient, cache *tokenCache, creds HepCredentials, point MeteringPoint, log zerolog.Logger, refTime time.Time) error {
	start := time.Now()
	log.Info().Str("code", point.Code).Msg("collecting metering point")

	months := getMonthsToCollect(point, refTime)

	if len(months) == 0 {
		log.Info().Str("code", point.Code).Msg("nothing to fetch")
		return nil
	}

	if point.LastMeterReadingCollection == nil && len(months) > 1 {
		log.Info().Int("months", len(months)).Str("code", point.Code).Msg("backfilling metering point")
	}

	sem := make(chan struct{}, HepConcurrentFetches)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var errs []error

	record := func(err error, my monthYear, direction string) {
		errMu.Lock()
		errs = append(errs, fmt.Errorf("%02d.%d %s: %w", my.Month, my.Year, direction, err))
		errMu.Unlock()
	}

	for _, my := range months {
		sem <- struct{}{}
		wg.Add(1)
		go func(my monthYear) {
			defer wg.Done()
			defer func() { <-sem }()

			log.Info().Str("code", point.Code).Int("month", my.Month).Int("year", my.Year).Msg("fetching month")

			var insolWg sync.WaitGroup
			insolWg.Add(1)
			go func(yr, mo int) {
				defer insolWg.Done()
				fetchAndStoreInsolation(ctx, db, point, yr, mo, log)
			}(my.Year, my.Month)

			if point.Consumer {
				if err := fetchAndStoreReadings(ctx, db, hepClient, cache, creds, point.Code, my.Month, my.Year, "P", MeteringTypeConsumption, log, refTime); err != nil {
					log.Error().Err(err).Str("code", point.Code).Int("month", my.Month).Int("year", my.Year).Msg("error fetching consumption")
					record(err, my, "consumption")
				}
			}
			if point.Producer {
				if err := fetchAndStoreReadings(ctx, db, hepClient, cache, creds, point.Code, my.Month, my.Year, "R", MeteringTypeProduction, log, refTime); err != nil {
					log.Error().Err(err).Str("code", point.Code).Int("month", my.Month).Int("year", my.Year).Msg("error fetching production")
					record(err, my, "production")
				}
			}

			insolWg.Wait()
		}(my)
	}
	wg.Wait()

	if len(errs) > 0 {
		log.Error().Str("code", point.Code).Int("failed_months", len(errs)).Dur("elapsed", time.Since(start)).Msg("metering point finished with errors, last collection timestamp not updated")
		return errors.Join(errs...)
	}

	if err := UpdateLastCollection(ctx, db, point.Code); err != nil {
		log.Error().Err(err).Str("code", point.Code).Msg("failed to update last collection timestamp")
	}
	log.Info().Str("code", point.Code).Dur("elapsed", time.Since(start)).Msg("metering point done")
	return nil
}

// fetchAndStoreReadings fetches readings from the HEP API for a single metering
// point and direction (consumption or production), then filters and stores them
// in the database.
//
// The function handles:
//   - Token expiry: if the API returns 401, it re-authenticates via loginAndCache
//     and retries the request once. The refreshed token lands in the shared cache
//     so subsequent calls in the same collection cycle use it.
//   - Future readings: the API returns data for the entire month including future
//     time slots — these are filtered out.
//   - Trailing zeros: the API pads future/unavailable time slots with zero values.
//     Continuous zeros at the end of the readings are trimmed since they don't
//     represent real measurements.
//   - Duplicate readings: the database has a unique constraint on
//     (metering_point_code, timestamp, type), and we use ON CONFLICT DO NOTHING
//     to silently skip already-stored readings.
func fetchAndStoreReadings(ctx context.Context, db *DB, hepClient *HepClient, cache *tokenCache, creds HepCredentials, code string, month, year int, direction, meteringType string, log zerolog.Logger, refTime time.Time) error {
	// Skip months that are permanently marked as having no data.
	skipped, err := IsMonthSkipped(ctx, db, code, year, month, direction)
	if err != nil {
		log.Warn().Err(err).Str("code", code).Int("month", month).Int("year", year).Str("direction", direction).Msg("failed to check skipped month, proceeding anyway")
	} else if skipped {
		return nil
	}

	// For completed past months, skip the HEP API call if readings already exist.
	// Uses refTime (not time.Now()) so manual fetches with yesterday as reference
	// treat the previous month as "current" and always re-fetch it.
	isPastMonth := year < refTime.Year() || (year == refTime.Year() && month < int(refTime.Month()))
	if isPastMonth {
		has, err := MonthAlreadyCollected(ctx, db, code, year, month, meteringType)
		if err != nil {
			log.Warn().Err(err).Str("code", code).Int("month", month).Int("year", year).Msg("failed to check whether month was collected, proceeding anyway")
		} else if has {
			log.Debug().Str("code", code).Int("month", month).Int("year", year).Str("type", meteringType).Msg("readings already exist, skipping")
			return nil
		}
	}

	// Get token from cache; re-authenticate if missing.
	// Each goroutine holds its own local copy so concurrent calls don't race.
	token, ok := cache.Get()
	if !ok {
		token, err = ensureToken(ctx, db, hepClient, cache, creds, "", log)
		if err != nil {
			return err
		}
	}

	hepReadings, err := hepClient.FetchReadings(ctx, token, code, month, year, direction, creds.Username)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			// Token expired, or the session it refers to was invalidated by a
			// newer login — re-authenticate and retry the request once.
			token, err = ensureToken(ctx, db, hepClient, cache, creds, token, log)
			if err != nil {
				return err
			}
			hepReadings, err = hepClient.FetchReadings(ctx, token, code, month, year, direction, creds.Username)
			if err != nil {
				return err
			}
		} else if errors.Is(err, ErrNoDataForMonth) {
			// The API confirmed there is no data for this month — record it so we
			// never ask again. Logged at warn (not error) since it's a known condition.
			reason := fmt.Sprintf("no data: %02d.%d direction=%s", month, year, direction)
			if markErr := MarkMonthSkipped(ctx, db, code, year, month, direction, reason); markErr != nil {
				log.Error().Err(markErr).Str("code", code).Int("month", month).Int("year", year).Msg("failed to mark month as skipped")
			}
			log.Warn().Str("code", code).Int("month", month).Int("year", year).Str("direction", direction).Msg("no data for month, skipping permanently")
			return nil
		} else {
			return err
		}
	}

	// Convert HEP API readings to our database model, filtering out invalid
	// and future readings.
	now := time.Now()
	var readings []MeterReading

	for _, hr := range hepReadings {
		// Parse the naive datetime string using Europe/Zagreb timezone.
		ts, err := parseHepTimestamp(hr.Datum)
		if err != nil {
			log.Warn().Str("datum", hr.Datum).Err(err).Msg("skipping reading with bad timestamp")
			continue
		}

		// Skip readings with timestamps in the future — the API returns the entire
		// month's data including not-yet-measured time slots.
		if ts.After(now) {
			continue
		}

		// Convert the comma-decimal value string (e.g. "0,59600000") to float64.
		val, err := parseHepValue(hr.Value)
		if err != nil {
			log.Warn().Str("value", hr.Value).Err(err).Msg("skipping reading with bad value")
			continue
		}

		// Parse the status code from string to int16 (e.g. "0" → 0).
		var status int16
		if hr.Status != "" {
			if v, err := strconv.ParseInt(hr.Status, 10, 16); err == nil {
				status = int16(v)
			}
		}

		readings = append(readings, MeterReading{
			MeteringPointCode: hr.Sifra,
			Type:              meteringType,
			Timestamp:         ts,
			Value:             val,
			Status:            status,
		})
	}

	// Sort by timestamp so the trim below reliably removes the most-recent zeros
	// regardless of the order the HEP API returned them.
	sort.Slice(readings, func(i, j int) bool {
		return readings[i].Timestamp.Before(readings[j].Timestamp)
	})

	// Trim trailing continuous zero values from the end of the readings slice.
	// Meter readings are available up until midnight of the previous day. The API
	// pads the remaining time slots (from midnight today through end of month) with
	// zeros. These aren't real measurements, so we strip them to avoid storing
	// misleading data. Legitimate zero readings that occur before the trailing
	// padding are preserved.
	for len(readings) > 0 && readings[len(readings)-1].Value == 0 {
		readings = readings[:len(readings)-1]
	}

	if len(readings) > 0 {
		inserted, err := InsertMeterReadings(ctx, db, readings)
		if err != nil {
			return err
		}
		log.Info().Int64("inserted", inserted).Int("total", len(readings)).Str("type", meteringType).Str("code", code).Msg("stored readings")

		// Refresh the aggregates for every month the stored readings actually touch,
		// not just the requested one. A month's HEP response ends with the first slot
		// of the following month, so writing month N extends day 1 of month N+1;
		// recomputing only month N leaves that day one 15-minute slot short until
		// something else happens to refresh it.
		for _, my := range monthsTouched(readings) {
			if err := UpsertDailyAggregates(ctx, db, code, meteringType, my.Year, my.Month); err != nil {
				log.Error().Err(err).Str("code", code).Str("type", meteringType).Int("month", my.Month).Int("year", my.Year).Msg("failed to update daily aggregates")
			}
		}
	}

	return nil
}

// fetchAndStoreInsolation fetches daily weather codes and radiation sums from
// Open-Meteo for the given metering point and month, then upserts them into
// daily_insolation. Skipped silently when the point has no geocoded coordinates.
// For completed past months, skipped when data already exists in the database.
func fetchAndStoreInsolation(ctx context.Context, db *DB, point MeteringPoint, year, month int, log zerolog.Logger) {
	if point.Latitude == nil || point.Longitude == nil {
		return
	}

	// Skip the Open-Meteo call for a past month only once every day of that month
	// is already stored. A month first fetched while the Open-Meteo archive still
	// lagged (~5 days behind real time) is stored incomplete; re-fetching lets the
	// now-available tail days fill in (upsert is idempotent).
	now := time.Now()
	isPastMonth := year < now.Year() || (year == now.Year() && month < int(now.Month()))
	if isPastMonth {
		daysInMonth := time.Date(year, time.Month(month+1), 0, 0, 0, 0, 0, time.UTC).Day()
		count, err := CountInsolationForMonth(ctx, db, point.Code, year, month)
		if err != nil {
			log.Warn().Err(err).Str("code", point.Code).Int("year", year).Int("month", month).Msg("failed to check insolation existence")
		} else if count >= daysInMonth {
			return
		}
	}

	days, err := FetchInsolation(ctx, *point.Latitude, *point.Longitude, year, month)
	if err != nil {
		log.Warn().Err(err).Str("code", point.Code).Int("year", year).Int("month", month).Msg("failed to fetch insolation")
		return
	}
	if len(days) == 0 {
		return
	}

	if err := UpsertDailyInsolation(ctx, db, point.Code, days, year, month); err != nil {
		log.Error().Err(err).Str("code", point.Code).Int("year", year).Int("month", month).Msg("failed to store insolation")
		return
	}
	log.Info().Str("code", point.Code).Int("year", year).Int("month", month).Int("days", len(days)).Msg("stored insolation data")
}

// monthsTouched returns the distinct local calendar months the readings fall in,
// in ascending order. Readings are stored with UTC epochs but bucketed by local
// date everywhere, so the aggregation has to follow the local month.
func monthsTouched(readings []MeterReading) []monthYear {
	seen := make(map[monthYear]struct{})
	var months []monthYear
	for _, r := range readings {
		lt := r.Timestamp.In(zagrebLocation)
		my := monthYear{Month: int(lt.Month()), Year: lt.Year()}
		if _, ok := seen[my]; ok {
			continue
		}
		seen[my] = struct{}{}
		months = append(months, my)
	}
	sort.Slice(months, func(i, j int) bool {
		if months[i].Year != months[j].Year {
			return months[i].Year < months[j].Year
		}
		return months[i].Month < months[j].Month
	})
	return months
}
