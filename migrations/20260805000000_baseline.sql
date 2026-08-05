-- +goose Up

-- SQLite baseline. This replaces the 21 PostgreSQL migrations that preceded it:
-- their end state is exactly the schema below, and they could not be replayed here
-- anyway (CREATE TYPE, uuid defaults, ALTER TABLE ... DROP CONSTRAINT).
--
-- Conventions:
--   * timestamps  — INTEGER, unix epoch seconds, UTC. Queries convert to local
--                   time with datetime(ts,'unixepoch','localtime'); the process
--                   pins TZ to Europe/Zagreb (see Config.AppTimezone).
--   * dates       — TEXT 'YYYY-MM-DD' in local time.
--   * times       — TEXT 'HH:MM' in local time.
--   * enums       — TEXT with a CHECK constraint; SQLite has no enum type.
--   * numerics    — REAL; the Go layer has always used float64 for these.

CREATE TABLE metering_points
(
    code                          TEXT    NOT NULL PRIMARY KEY,
    address                       TEXT,
    oib                           TEXT    NOT NULL,
    consumer                      INTEGER NOT NULL DEFAULT 1,
    producer                      INTEGER NOT NULL DEFAULT 1,
    available_from                TEXT,
    available_to                  TEXT,
    latitude                      REAL,
    longitude                     REAL,
    last_meter_reading_collection INTEGER,
    tariff_model                  TEXT    NOT NULL DEFAULT 'bijeli'
        CHECK (tariff_model IN ('plavi', 'bijeli', 'crveni'))
);

CREATE TABLE meter_readings
(
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    metering_point_code TEXT    NOT NULL REFERENCES metering_points (code),
    type                TEXT    NOT NULL CHECK (type IN ('CONSUMPTION', 'PRODUCTION')),
    timestamp           INTEGER NOT NULL,
    value               REAL    NOT NULL,
    status              INTEGER NOT NULL DEFAULT 0,
    UNIQUE (metering_point_code, timestamp, type)
);

-- Serves the month/year range scans in the reading queries. The UNIQUE
-- constraint above already indexes (code, timestamp, type), so this covers the
-- narrower lookups without a second full index.
CREATE INDEX meter_readings_code_ts ON meter_readings (metering_point_code, timestamp);

CREATE TABLE daily_aggregates
(
    metering_point_code TEXT NOT NULL REFERENCES metering_points (code),
    date                TEXT NOT NULL,
    type                TEXT NOT NULL CHECK (type IN ('CONSUMPTION', 'PRODUCTION')),
    kwh                 REAL NOT NULL,
    PRIMARY KEY (metering_point_code, date, type)
);

CREATE TABLE daily_insolation
(
    metering_point_code TEXT    NOT NULL REFERENCES metering_points (code),
    date                TEXT    NOT NULL,
    weathercode         INTEGER NOT NULL,
    radiation_mj        REAL    NOT NULL,
    sunrise             TEXT,
    sunset              TEXT,
    temperature_max     REAL,
    temperature_min     REAL,
    PRIMARY KEY (metering_point_code, date)
);

CREATE TABLE skipped_metering_months
(
    metering_point_code TEXT    NOT NULL REFERENCES metering_points (code) ON DELETE CASCADE,
    year                INTEGER NOT NULL,
    month               INTEGER NOT NULL,
    direction           TEXT    NOT NULL,
    reason              TEXT    NOT NULL,
    created_at          INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    PRIMARY KEY (metering_point_code, year, month, direction)
);

-- +goose Down

DROP TABLE skipped_metering_months;
DROP TABLE daily_insolation;
DROP TABLE daily_aggregates;
DROP INDEX meter_readings_code_ts;
DROP TABLE meter_readings;
DROP TABLE metering_points;
