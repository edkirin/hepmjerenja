package main

import (
	"context"
	"fmt"
	"time"
)

// ──────────────────────────────────────────────
// SQLite conventions used throughout this file
//
//   * meter_readings.timestamp and
//     metering_points.last_meter_reading_collection are INTEGER unix epoch
//     seconds in UTC.
//   * daily_aggregates.date and daily_insolation.date are TEXT 'YYYY-MM-DD' in
//     local time, so a month is matched with substr(date,1,7) and a year with
//     substr(date,1,4).
//   * localTS / localMonth / localDay / localHour below convert a stored epoch to
//     local time via the 'localtime' modifier, which reads the process TZ. main()
//     pins that to AppTimezone, so these are Europe/Zagreb regardless of the host.
// ──────────────────────────────────────────────

const (
	// localTS is the reading timestamp as local wall-clock text.
	localTS = `datetime(mr.timestamp,'unixepoch','localtime')`

	// localMonthNum buckets a reading by local month number, for yearly views.
	localMonthNum = `CAST(strftime('%m', ` + localTS + `) AS INTEGER)`

	// localFields is the projection every reading query starts from: it converts a
	// row to local time exactly once. Converting inside each expression instead
	// (day, hour, offset) multiplied the work by five, which is expensive because
	// SQLite's 'localtime' goes through the driver's libc — cheap on glibc, slow in
	// the pure-Go emulation used on Windows.
	//
	// offset_seconds is the local UTC offset: 3600 in CET, 7200 in CEST.
	// strftime('%s', <local wall-clock text>) re-reads that text as if it were UTC,
	// so subtracting the true epoch yields the offset. It replaces the PostgreSQL
	// "ts AT TIME ZONE 'Europe/Zagreb' - ts AT TIME ZONE 'UTC'" trick.
	localFields = localTS + ` AS local,
		       (CAST(strftime('%s', ` + localTS + `) AS INTEGER) - mr.timestamp) AS offset_seconds`

	// isVTFromLocal flags the high tariff (VT) window, which shifts with DST:
	//   winter (CET, offset 3600): VT 07:00–21:00
	//   summer (CEST, offset 7200): VT 08:00–22:00
	// It reads the columns produced by localFields, so no further conversion.
	isVTFromLocal = `CASE WHEN offset_seconds = 7200
	                     THEN local_hour >= 8 AND local_hour < 22
	                     ELSE local_hour >= 7 AND local_hour < 21
	                END`

	// readingsInRange selects a half-open epoch range for one metering point. The
	// bounds come from monthRange/yearRange, computed in Go, so the filter is a
	// plain integer comparison that uses the (metering_point_code, timestamp)
	// index instead of converting every row in the table to local time.
	readingsInRange = `mr.metering_point_code = ? AND mr.timestamp >= ? AND mr.timestamp < ?`
)

// monthRange returns the half-open [from, to) epoch range covering one local
// calendar month. Building it from local time makes it DST-correct: a March range
// starts in CET and ends in CEST, and is exactly one hour shorter than the naive
// arithmetic would suggest.
func monthRange(year, month int) (int64, int64) {
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, zagrebLocation)
	return start.Unix(), start.AddDate(0, 1, 0).Unix()
}

// yearRange returns the half-open [from, to) epoch range covering one local year.
func yearRange(year int) (int64, int64) {
	start := time.Date(year, time.January, 1, 0, 0, 0, 0, zagrebLocation)
	return start.Unix(), start.AddDate(1, 0, 0).Unix()
}

// monthKey and yearKey format a prefix of the TEXT 'YYYY-MM-DD' dates stored in
// daily_aggregates and daily_insolation, matched with substr(). Those tables hold
// one row per day, so scanning them costs nothing worth optimising — unlike
// meter_readings, which is filtered by epoch range instead.
func monthKey(year, month int) string {
	return fmt.Sprintf("%04d-%02d", year, month)
}

func yearKey(year int) string {
	return fmt.Sprintf("%04d", year)
}

// UpsertMeteringPoint inserts a new metering point or updates an existing one
// based on the unique code. The metering point data comes from the HEP login
// response (HepOmm struct). On conflict (same code already exists), we update
// the mutable fields (address, flags, date range) to keep them current.
func UpsertMeteringPoint(ctx context.Context, db *DB, omm HepOmm) error {
	// Parse the available date range from HEP's ISO datetime format and store the
	// date part only. Errors are silently ignored — dates are optional metadata.
	var availableFrom, availableTo any
	if t, err := time.Parse("2006-01-02T15:04:05", omm.MjesecOd); err == nil {
		availableFrom = t.Format("2006-01-02")
	}
	if t, err := time.Parse("2006-01-02T15:04:05", omm.MjesecDo); err == nil {
		availableTo = t.Format("2006-01-02")
	}

	_, err := db.Exec(ctx, `
		INSERT INTO metering_points (code, address, oib, consumer, producer, available_from, available_to)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (code) DO UPDATE SET
			address = excluded.address,
			oib = excluded.oib,
			consumer = excluded.consumer,
			producer = excluded.producer,
			available_from = excluded.available_from,
			available_to = excluded.available_to
	`, omm.Sifra, omm.Adresa, omm.Oib, omm.Potrosac, omm.Proizvodjac,
		availableFrom, availableTo)
	if err != nil {
		return fmt.Errorf("upsert metering point %s: %w", omm.Sifra, err)
	}
	return nil
}

// InsertMeterReadings inserts meter readings inside a single transaction using a
// prepared statement. Uses ON CONFLICT DO NOTHING to gracefully handle duplicate
// readings — meter_readings is unique on (metering_point_code, timestamp, type),
// so re-inserting the same reading for a given time slot is silently skipped.
// Returns the number of newly inserted rows.
//
// A row-per-Exec loop in one transaction is used rather than one giant multi-row
// INSERT: a full month is 2,976 readings, which as a single statement would bind
// ~15,000 parameters, and SQLite is faster this way regardless.
func InsertMeterReadings(ctx context.Context, db *DB, readings []MeterReading) (int64, error) {
	if len(readings) == 0 {
		return 0, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO meter_readings (metering_point_code, type, timestamp, value, status)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (metering_point_code, timestamp, type) DO NOTHING
	`)
	if err != nil {
		return 0, fmt.Errorf("prepare insert meter readings: %w", err)
	}
	defer stmt.Close()

	var inserted int64
	for _, r := range readings {
		res, err := stmt.ExecContext(ctx, r.MeteringPointCode, r.Type, r.Timestamp.Unix(), r.Value, r.Status)
		if err != nil {
			return 0, fmt.Errorf("insert meter reading: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("rows affected: %w", err)
		}
		inserted += n
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit meter readings: %w", err)
	}
	return inserted, nil
}

// UpdateLastCollection sets a metering point's last_meter_reading_collection
// to now. Called after all months for that point have been processed so the
// worker knows where to resume on the next run.
func UpdateLastCollection(ctx context.Context, db *DB, code string) error {
	_, err := db.Exec(ctx, `
		UPDATE metering_points
		SET last_meter_reading_collection = strftime('%s','now')
		WHERE code = ?
	`, code)
	return err
}

// UpsertDailyAggregates recomputes the daily kWh totals for a single
// (metering_point_code, type, year, month) combination from the raw meter_readings
// table and upserts the results into daily_aggregates. Existing rows for the same
// (code, date, type) are overwritten so the aggregates stay in sync after each
// collection cycle.
func UpsertDailyAggregates(ctx context.Context, db *DB, code, meteringType string, year, month int) error {
	from, to := monthRange(year, month)
	// The WHERE clause before ON CONFLICT is required: SQLite cannot parse an
	// upsert clause directly after a SELECT source.
	_, err := db.Exec(ctx, `
		INSERT INTO daily_aggregates (metering_point_code, date, type, kwh)
		SELECT metering_point_code,
		       local_date,
		       type,
		       -- 4 decimals matches the NUMERIC(14,4) column this replaced, so stored
		       -- aggregates keep the same precision they always had.
		       ROUND(SUM(value * 0.25), 4)
		FROM (
			SELECT mr.metering_point_code                    AS metering_point_code,
			       strftime('%Y-%m-%d', `+localTS+`) AS local_date,
			       mr.type                                   AS type,
			       mr.value                                  AS value
			FROM meter_readings mr
			WHERE `+readingsInRange+`
			  AND mr.type = ?
		)
		GROUP BY local_date
		ON CONFLICT (metering_point_code, date, type) DO UPDATE SET kwh = excluded.kwh
	`, code, from, to, meteringType)
	if err != nil {
		return fmt.Errorf("upsert daily aggregates %s/%s %d-%02d: %w", code, meteringType, year, month, err)
	}
	return nil
}

// GetAvailableMonths returns a list of distinct months (as the first of the month)
// for which daily aggregates exist for the given metering point, ordered newest
// first.
func GetAvailableMonths(ctx context.Context, db *DB, code string) ([]time.Time, error) {
	rows, err := db.Query(ctx, `
		SELECT DISTINCT substr(date, 1, 7) AS month
		FROM daily_aggregates
		WHERE metering_point_code = ?
		ORDER BY month DESC
	`, code)
	if err != nil {
		return nil, fmt.Errorf("query available months: %w", err)
	}
	defer rows.Close()

	var months []time.Time
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan month: %w", err)
		}
		// Parsed as UTC midnight on the first of the month, matching what
		// DATE_TRUNC('month', …) produced before.
		t, err := time.ParseInLocation("2006-01", m, time.UTC)
		if err != nil {
			return nil, fmt.Errorf("parse month %q: %w", m, err)
		}
		months = append(months, t)
	}
	return months, rows.Err()
}

// GetDailyReadingsForMonth returns daily kWh totals for the given metering point,
// year, and month, read from the pre-computed daily_aggregates table.
// Results are split into consumption and production slices.
func GetDailyReadingsForMonth(ctx context.Context, db *DB, code string, year, month int) (consumption, production []DailyReading, err error) {
	rows, err := db.Query(ctx, `
		SELECT
			CAST(substr(date, 9, 2) AS INTEGER),
			type,
			kwh
		FROM daily_aggregates
		WHERE metering_point_code = ?
		  AND substr(date, 1, 7) = ?
		ORDER BY date
	`, code, monthKey(year, month))
	if err != nil {
		return nil, nil, fmt.Errorf("query daily readings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var day int
		var typ string
		var value float64
		if err := rows.Scan(&day, &typ, &value); err != nil {
			return nil, nil, fmt.Errorf("scan daily reading: %w", err)
		}
		r := DailyReading{Day: fmt.Sprintf("%d", day), Value: value}
		if typ == MeteringTypeConsumption {
			consumption = append(consumption, r)
		} else {
			production = append(production, r)
		}
	}
	return consumption, production, rows.Err()
}

// GetMeteringPointTariff returns the tariff_model for a single metering point.
func GetMeteringPointTariff(ctx context.Context, db *DB, code string) (string, error) {
	var tariff string
	err := db.QueryRow(ctx, `
		SELECT tariff_model FROM metering_points WHERE code = ?
	`, code).Scan(&tariff)
	if err != nil {
		return "", fmt.Errorf("get tariff model: %w", err)
	}
	return tariff, nil
}

// GetDailyVTNTForMonth returns daily totals split into VT (high tariff) and NT
// (low tariff) for the given metering type and month. VT/NT boundaries follow
// the Europe/Zagreb DST schedule:
//   - Winter (CET, UTC+1): VT 07:00–21:00, NT 21:00–07:00
//   - Summer (CEST, UTC+2): VT 08:00–22:00, NT 22:00–08:00
func GetDailyVTNTForMonth(ctx context.Context, db *DB, code, meteringType string, year, month int) (vt, nt []DailyReading, err error) {
	from, to := monthRange(year, month)
	rows, err := db.Query(ctx, `
		SELECT day,
		       SUM(CASE WHEN is_vt THEN value * 0.25 ELSE 0 END),
		       SUM(CASE WHEN NOT is_vt THEN value * 0.25 ELSE 0 END)
		FROM (
			SELECT day, value, `+isVTFromLocal+` AS is_vt
			FROM (
				SELECT CAST(strftime('%d', local) AS INTEGER) AS day,
				       CAST(strftime('%H', local) AS INTEGER) AS local_hour,
				       offset_seconds,
				       value
				FROM (
					SELECT `+localFields+`,
					       mr.value AS value
					FROM meter_readings mr
					WHERE `+readingsInRange+`
					  AND mr.type = ?
				)
			)
		)
		GROUP BY day
		ORDER BY day
	`, code, from, to, meteringType)
	if err != nil {
		return nil, nil, fmt.Errorf("query VT/NT readings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var day int
		var vtVal, ntVal float64
		if err := rows.Scan(&day, &vtVal, &ntVal); err != nil {
			return nil, nil, fmt.Errorf("scan VT/NT reading: %w", err)
		}
		d := fmt.Sprintf("%d", day)
		vt = append(vt, DailyReading{Day: d, Value: vtVal})
		nt = append(nt, DailyReading{Day: d, Value: ntVal})
	}
	return vt, nt, rows.Err()
}

// GetMonthlyVTNTForYear returns monthly kWh totals split into VT (high tariff) and NT
// (low tariff) for the given metering type and year. VT/NT boundaries follow the
// Europe/Zagreb DST schedule. The Day field of each DailyReading holds the month number (1–12).
func GetMonthlyVTNTForYear(ctx context.Context, db *DB, code, meteringType string, year int) (vt, nt []DailyReading, err error) {
	from, to := yearRange(year)
	rows, err := db.Query(ctx, `
		SELECT month,
		       SUM(CASE WHEN is_vt THEN value * 0.25 ELSE 0 END),
		       SUM(CASE WHEN NOT is_vt THEN value * 0.25 ELSE 0 END)
		FROM (
			SELECT month, value, `+isVTFromLocal+` AS is_vt
			FROM (
				SELECT CAST(strftime('%m', local) AS INTEGER) AS month,
				       CAST(strftime('%H', local) AS INTEGER) AS local_hour,
				       offset_seconds,
				       value
				FROM (
					SELECT `+localFields+`,
					       mr.value AS value
					FROM meter_readings mr
					WHERE `+readingsInRange+`
					  AND mr.type = ?
				)
			)
		)
		GROUP BY month
		ORDER BY month
	`, code, from, to, meteringType)
	if err != nil {
		return nil, nil, fmt.Errorf("query monthly VT/NT readings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var month int
		var vtVal, ntVal float64
		if err := rows.Scan(&month, &vtVal, &ntVal); err != nil {
			return nil, nil, fmt.Errorf("scan monthly VT/NT reading: %w", err)
		}
		m := fmt.Sprintf("%d", month)
		vt = append(vt, DailyReading{Day: m, Value: vtVal})
		nt = append(nt, DailyReading{Day: m, Value: ntVal})
	}
	return vt, nt, rows.Err()
}

// GetMonthlyReadingsForYear returns kWh totals aggregated by month for the given
// metering point and year, read from the pre-computed daily_aggregates table.
// Results are split into consumption and production slices. The Day field of each
// DailyReading holds the month number (1–12).
func GetMonthlyReadingsForYear(ctx context.Context, db *DB, code string, year int) (consumption, production []DailyReading, err error) {
	rows, err := db.Query(ctx, `
		SELECT
			CAST(substr(date, 6, 2) AS INTEGER) AS month,
			type,
			-- Lossless: the stored daily values have at most 4 decimals, so their
			-- sum does too; rounding only removes binary-float noise.
			ROUND(SUM(kwh), 6)
		FROM daily_aggregates
		WHERE metering_point_code = ?
		  AND substr(date, 1, 4) = ?
		GROUP BY month, type
		ORDER BY month
	`, code, yearKey(year))
	if err != nil {
		return nil, nil, fmt.Errorf("query monthly readings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var month int
		var typ string
		var value float64
		if err := rows.Scan(&month, &typ, &value); err != nil {
			return nil, nil, fmt.Errorf("scan monthly reading: %w", err)
		}
		r := DailyReading{Day: fmt.Sprintf("%d", month), Value: value}
		if typ == MeteringTypeConsumption {
			consumption = append(consumption, r)
		} else {
			production = append(production, r)
		}
	}
	return consumption, production, rows.Err()
}

// GetAllReadingsForMonth returns all individual 15-minute readings for the given
// metering point, year and month, split into consumption and production slices
// ordered by timestamp.
func GetAllReadingsForMonth(ctx context.Context, db *DB, code string, year, month int) (consumption, production []ReadingPoint, err error) {
	from, to := monthRange(year, month)
	rows, err := db.Query(ctx, `
		SELECT
			mr.timestamp,
			mr.type,
			mr.value
		FROM meter_readings mr
		WHERE `+readingsInRange+`
		ORDER BY mr.timestamp
	`, code, from, to)
	if err != nil {
		return nil, nil, fmt.Errorf("query all readings: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var epoch int64
		var typ string
		var value float64
		if err := rows.Scan(&epoch, &typ, &value); err != nil {
			return nil, nil, fmt.Errorf("scan reading: %w", err)
		}
		// Label formatting stays in Go, where the timezone is explicit.
		ts := time.Unix(epoch, 0).In(zagrebLocation)
		p := ReadingPoint{Time: ts.Format("02.01. 15:04"), Value: value * 0.25}
		if typ == MeteringTypeConsumption {
			consumption = append(consumption, p)
		} else {
			production = append(production, p)
		}
	}
	return consumption, production, rows.Err()
}

// GetHourlyAverageForMonth returns the average kWh per hour-of-day (0–23) for the
// given month, computed by first summing all four 15-min slots within each clock
// hour and then averaging that hourly total across all days in the month that have
// data. Results are split into consumption and production slices; the Day field of
// each DailyReading holds the hour number as a string ("0"–"23").
func GetHourlyAverageForMonth(ctx context.Context, db *DB, code string, year, month int) (consumption, production []DailyReading, err error) {
	from, to := monthRange(year, month)
	rows, err := db.Query(ctx, `
		SELECT hour, type, AVG(hourly_kwh) AS avg_kwh
		FROM (
			SELECT CAST(substr(local, 12, 2) AS INTEGER) AS hour,
			       substr(local, 1, 13)                  AS hour_ts,
			       type,
			       SUM(value * 0.25)                     AS hourly_kwh
			FROM (
				SELECT `+localTS+` AS local,
				       mr.type            AS type,
				       mr.value           AS value
				FROM meter_readings mr
				WHERE `+readingsInRange+`
			)
			GROUP BY hour, hour_ts, type
		)
		GROUP BY hour, type
		ORDER BY hour
	`, code, from, to)
	if err != nil {
		return nil, nil, fmt.Errorf("query hourly averages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var hour int
		var typ string
		var value float64
		if err := rows.Scan(&hour, &typ, &value); err != nil {
			return nil, nil, fmt.Errorf("scan hourly average: %w", err)
		}
		r := DailyReading{Day: fmt.Sprintf("%d", hour), Value: value}
		if typ == MeteringTypeConsumption {
			consumption = append(consumption, r)
		} else {
			production = append(production, r)
		}
	}
	return consumption, production, rows.Err()
}

// GetDailyTotalsForYear returns daily consumption and production totals for every
// day in the given year, read from the pre-computed daily_aggregates table.
// Used by the calendar heatmap on the yearly view. Days with no data are omitted.
func GetDailyTotalsForYear(ctx context.Context, db *DB, code string, year int) ([]CalendarDay, error) {
	rows, err := db.Query(ctx, `
		SELECT date, type, kwh
		FROM daily_aggregates
		WHERE metering_point_code = ?
		  AND substr(date, 1, 4) = ?
		ORDER BY date
	`, code, yearKey(year))
	if err != nil {
		return nil, fmt.Errorf("query daily totals for year: %w", err)
	}
	defer rows.Close()

	byDate := make(map[string]*CalendarDay)
	var order []string
	for rows.Next() {
		var date, typ string
		var kwh float64
		if err := rows.Scan(&date, &typ, &kwh); err != nil {
			return nil, fmt.Errorf("scan daily total: %w", err)
		}
		if _, ok := byDate[date]; !ok {
			byDate[date] = &CalendarDay{Date: date}
			order = append(order, date)
		}
		if typ == MeteringTypeConsumption {
			byDate[date].Consumption = kwh
		} else {
			byDate[date].Production = kwh
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]CalendarDay, len(order))
	for i, d := range order {
		result[i] = *byDate[d]
	}
	return result, nil
}

// UpsertDailyInsolation upserts Open-Meteo weather data for a single
// (metering_point_code, year, month). Existing rows are overwritten so data
// stays current when the same month is re-fetched.
func UpsertDailyInsolation(ctx context.Context, db *DB, code string, days []InsolationDay, year, month int) error {
	if len(days) == 0 {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO daily_insolation (metering_point_code, date, weathercode, radiation_mj,
		                              sunrise, sunset, temperature_max, temperature_min)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (metering_point_code, date) DO UPDATE
			SET weathercode     = excluded.weathercode,
			    radiation_mj    = excluded.radiation_mj,
			    sunrise         = excluded.sunrise,
			    sunset          = excluded.sunset,
			    temperature_max = excluded.temperature_max,
			    temperature_min = excluded.temperature_min
	`)
	if err != nil {
		return fmt.Errorf("prepare upsert daily insolation: %w", err)
	}
	defer stmt.Close()

	for _, d := range days {
		date := fmt.Sprintf("%04d-%02d-%02d", year, month, d.Day)
		var sunriseVal, sunsetVal any
		if d.Sunrise != "" {
			sunriseVal = d.Sunrise
		}
		if d.Sunset != "" {
			sunsetVal = d.Sunset
		}
		if _, err := stmt.ExecContext(ctx, code, date, d.Weathercode, d.Radiation,
			sunriseVal, sunsetVal, d.TempMax, d.TempMin); err != nil {
			return fmt.Errorf("upsert daily insolation %s %s: %w", code, date, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit daily insolation: %w", err)
	}
	return nil
}

// CountInsolationForMonth returns the number of stored daily_insolation rows for
// the given metering point code and calendar month. Used to skip re-fetching a
// past month from Open-Meteo only once every day of that month is present — a
// month first fetched while the Open-Meteo archive still lagged is incomplete
// and must be re-fetched so the now-available tail days fill in.
func CountInsolationForMonth(ctx context.Context, db *DB, code string, year, month int) (int, error) {
	var count int
	err := db.QueryRow(ctx, `
		SELECT COUNT(*) FROM daily_insolation
		WHERE metering_point_code = ?
		  AND substr(date, 1, 7) = ?
	`, code, monthKey(year, month)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count insolation: %w", err)
	}
	return count, nil
}

// GetDailyInsolationForMonth returns stored insolation data for the given
// metering point and calendar month, ordered by date.
func GetDailyInsolationForMonth(ctx context.Context, db *DB, code string, year, month int) ([]InsolationDay, error) {
	rows, err := db.Query(ctx, `
		SELECT CAST(substr(date, 9, 2) AS INTEGER), weathercode, radiation_mj,
		       sunrise, sunset, temperature_max, temperature_min
		FROM daily_insolation
		WHERE metering_point_code = ?
		  AND substr(date, 1, 7) = ?
		ORDER BY date
	`, code, monthKey(year, month))
	if err != nil {
		return nil, fmt.Errorf("query daily insolation: %w", err)
	}
	defer rows.Close()

	var result []InsolationDay
	for rows.Next() {
		var d InsolationDay
		var sunrise, sunset *string
		var tempMax, tempMin *float64
		if err := rows.Scan(&d.Day, &d.Weathercode, &d.Radiation,
			&sunrise, &sunset, &tempMax, &tempMin); err != nil {
			return nil, fmt.Errorf("scan insolation day: %w", err)
		}
		if sunrise != nil {
			d.Sunrise = *sunrise
		}
		if sunset != nil {
			d.Sunset = *sunset
		}
		if tempMax != nil {
			d.TempMax = *tempMax
		}
		if tempMin != nil {
			d.TempMin = *tempMin
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// GetMonthlyInsolationForYear returns aggregated insolation data per month for
// the given metering point and calendar year. Each row holds the monthly
// radiation sum and average max/min temperatures.
func GetMonthlyInsolationForYear(ctx context.Context, db *DB, code string, year int) ([]MonthlyInsolation, error) {
	rows, err := db.Query(ctx, `
		SELECT CAST(substr(date, 6, 2) AS INTEGER) AS month,
		       COALESCE(SUM(radiation_mj), 0),
		       COALESCE(AVG(temperature_max), 0),
		       COALESCE(AVG(temperature_min), 0)
		FROM daily_insolation
		WHERE metering_point_code = ?
		  AND substr(date, 1, 4) = ?
		GROUP BY month
		ORDER BY month
	`, code, yearKey(year))
	if err != nil {
		return nil, fmt.Errorf("query monthly insolation: %w", err)
	}
	defer rows.Close()

	var result []MonthlyInsolation
	for rows.Next() {
		var m MonthlyInsolation
		if err := rows.Scan(&m.Month, &m.Radiation, &m.TempMax, &m.TempMin); err != nil {
			return nil, fmt.Errorf("scan monthly insolation: %w", err)
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// UpdateMeteringPointLocation stores geocoded WGS-84 coordinates for a metering point.
func UpdateMeteringPointLocation(ctx context.Context, db *DB, code string, lat, lon float64) error {
	_, err := db.Exec(ctx, `
		UPDATE metering_points SET latitude = ?, longitude = ? WHERE code = ?
	`, lat, lon, code)
	if err != nil {
		return fmt.Errorf("update metering point location %s: %w", code, err)
	}
	return nil
}

// GetMeteringPoints returns all known metering points, ordered by code so the
// web interface and the worker always see them in the same order.
func GetMeteringPoints(ctx context.Context, db *DB) ([]MeteringPoint, error) {
	rows, err := db.Query(ctx, `
		SELECT code, address, oib, consumer, producer, available_from, available_to,
		       latitude, longitude, last_meter_reading_collection, tariff_model
		FROM metering_points
		ORDER BY code
	`)
	if err != nil {
		return nil, fmt.Errorf("query metering points: %w", err)
	}
	defer rows.Close()

	var points []MeteringPoint
	for rows.Next() {
		var (
			p             MeteringPoint
			address       *string
			availableFrom *string
			availableTo   *string
			lastCollected *int64
		)
		if err := rows.Scan(&p.Code, &address, &p.OIB,
			&p.Consumer, &p.Producer, &availableFrom, &availableTo,
			&p.Latitude, &p.Longitude, &lastCollected, &p.TariffModel); err != nil {
			return nil, fmt.Errorf("scan metering point: %w", err)
		}
		if address != nil {
			p.Address = *address
		}
		p.AvailableFrom = parseDate(availableFrom)
		p.AvailableTo = parseDate(availableTo)
		if lastCollected != nil {
			t := time.Unix(*lastCollected, 0)
			p.LastMeterReadingCollection = &t
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// parseDate converts a stored 'YYYY-MM-DD' value into a time.Time at local
// midnight, or nil when the column is NULL or unparseable.
func parseDate(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02", *s, zagrebLocation)
	if err != nil {
		return nil
	}
	return &t
}

// ScheduleForceFetch resets last_meter_reading_collection to the start of the
// given month for every metering point so the worker re-fetches that month's
// data on the next tick. Returns the number of metering points affected.
func ScheduleForceFetch(ctx context.Context, db *DB, month time.Time) (int64, error) {
	monthStart := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)

	res, err := db.Exec(ctx, `
		UPDATE metering_points
		SET last_meter_reading_collection = ?
	`, monthStart.Unix())
	if err != nil {
		return 0, fmt.Errorf("schedule force fetch: %w", err)
	}
	return res.RowsAffected()
}

// UpdateMeteringPointTariff sets the tariff model for a metering point.
func UpdateMeteringPointTariff(ctx context.Context, db *DB, code, tariffModel string) error {
	_, err := db.Exec(ctx, `
		UPDATE metering_points SET tariff_model = ? WHERE code = ?
	`, tariffModel, code)
	return err
}

// IsMonthSkipped reports whether a (code, year, month, direction) combination has
// been permanently marked as having no data on the HEP API.
func IsMonthSkipped(ctx context.Context, db *DB, code string, year, month int, direction string) (bool, error) {
	var exists bool
	err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM skipped_metering_months
			WHERE metering_point_code = ? AND year = ? AND month = ? AND direction = ?
		)
	`, code, year, month, direction).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check skipped month: %w", err)
	}
	return exists, nil
}

// MonthAlreadyCollected reports whether the stored readings for a month look like
// a completed fetch, so the HEP API call for a past month can be skipped.
//
// It deliberately counts rather than testing for existence. A month's HEP response
// includes the first slot of the *next* month as an end-of-interval mark, so
// fetching April stores one row that falls in May; an existence test would then
// treat May as collected and skip it entirely, losing the month. That race is real
// — it costs whichever month follows one that is fetched earlier in the same run.
//
// The threshold is half the month's 15-minute slots: generous enough for a
// genuinely complete month, which can be short a few slots because the DST
// spring-forward day has 92 instead of 96, and because trailing zero readings are
// trimmed before storing (overnight production, in particular).
func MonthAlreadyCollected(ctx context.Context, db *DB, code string, year, month int, meteringType string) (bool, error) {
	from, to := monthRange(year, month)
	var count int
	err := db.QueryRow(ctx, `
		SELECT COUNT(*) FROM meter_readings mr
		WHERE `+readingsInRange+`
		  AND mr.type = ?
	`, code, from, to, meteringType).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("count readings for month: %w", err)
	}

	daysInMonth := time.Date(year, time.Month(month+1), 0, 0, 0, 0, 0, time.UTC).Day()
	expectedSlots := daysInMonth * 96
	return count*2 >= expectedSlots, nil
}

// MarkMonthSkipped records that a (code, year, month, direction) combination
// permanently has no data on the HEP API. Subsequent collection runs will skip it.
func MarkMonthSkipped(ctx context.Context, db *DB, code string, year, month int, direction, reason string) error {
	_, err := db.Exec(ctx, `
		INSERT INTO skipped_metering_months (metering_point_code, year, month, direction, reason)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, code, year, month, direction, reason)
	if err != nil {
		return fmt.Errorf("mark month skipped: %w", err)
	}
	return nil
}
