# HEPMjerenja

## Project description

Single-user application that periodically collects electricity meter data from https://mjerenje.hep.hr and stores it in a local SQLite database.

It runs locally for one HEP account. There is no login, registration, session handling or email sending — the HEP credentials come from `HEP_USERNAME` / `HEP_PASSWORD` in the environment, and every page is public. Nothing in the schema is owned by a user.

## Technology stack

- Go (`app/` package)
- Echo framework (HTTP server)
- templ (template rendering — run `templ generate` before building)
- SQLite (via modernc.org/sqlite — pure Go, so the Docker image stays CGO-free)
- goose (migrations in `migrations/` folder)
- zerolog (logging to console + JSON file)
- godotenv (parses `config.ini` / `.env`)
- air (live reload in development via `make run`)

## Project structure

```
app/
  main.go         - Entry point: Echo setup, routes, starts worker goroutines
  config.go       - Settings file + env vars (DB_PATH, HEP_USERNAME, …); DSN(); AppTimezone
  logger.go       - zerolog setup (console + file)
  models.go       - All structs: HepCredentials, MeteringPoint, MeterReading, HEP API models
  db.go           - SQLite handle (single connection) + Query/QueryRow/Exec helpers with SQL echo
  const.go        - Collection schedule and concurrency constants
  hep_client.go   - HEP API HTTP client (login, fetch readings)
  geocoder.go     - Address → coordinates (Photon / Nominatim)
  openmeteo.go    - Daily weather and radiation data
  repository.go   - All DB queries and inserts
  worker.go       - Background data collection loop + in-memory token cache / fetch state
  handlers.go     - HTTP handlers
  templates/      - templ sources and generated Go files (edit .templ, not _templ.go)
migrations/
  *.sql           - goose migration files (embedded; applied on startup)
Dockerfile         - two-stage build; static CGO-free binary, runs as UID 33
```

## Database schema

SQLite, created by a single baseline migration
(`migrations/20260805000000_baseline.sql`) that replaced the 21 PostgreSQL
migrations preceding it. There is no `users` table — authentication was removed
before the migration.

### Storage conventions
- **Timestamps** — `INTEGER` unix epoch seconds, UTC.
- **Dates** — `TEXT` `'YYYY-MM-DD'`, **local** time; a month is matched with
  `substr(date,1,7)`, a year with `substr(date,1,4)`.
- **Times** — `TEXT` `'HH:MM'` (insolation sunrise/sunset).
- **Enums** — `TEXT` + `CHECK`; SQLite has no enum type.
- **Numerics** — `REAL`. Stored daily aggregates are rounded to 4 decimals, which
  is what the old `NUMERIC(14,4)` column did.

### Local time in SQL
Queries convert a stored epoch with `datetime(ts,'unixepoch','localtime')`, which
reads the process `TZ` — `pinTimezone()` in main.go sets it to `AppTimezone`
(Europe/Zagreb) so the result never depends on the host. The reusable fragments
live at the top of `repository.go`: `localTS`, `localMonthKey`, `localYearKey`,
`localDayNum`, `localMonthNum`, `localHourNum`, `localOffsetSeconds`, `isVT`.

`localOffsetSeconds` is the replacement for PostgreSQL's
`ts AT TIME ZONE 'Europe/Zagreb' - ts AT TIME ZONE 'UTC'`: re-reading the local
wall-clock text as UTC and subtracting the epoch yields 3600 (CET) or 7200 (CEST),
which is what `isVT` keys the tariff window off.

### `metering_points`
- `code` TEXT PK — HEP "Šifra"
- `address`, `oib`
- `consumer`, `producer` — INTEGER booleans
- `available_from`, `available_to` — TEXT dates from HEP
- `latitude`, `longitude` — geocoded from the address, used for weather data
- `last_meter_reading_collection` — NULL or epoch of last successful collection
- `tariff_model` — `plavi`, `bijeli` or `crveni`

### `meter_readings`
- `id` INTEGER PK AUTOINCREMENT
- `metering_point_code` FK → metering_points.code
- `type` — `CONSUMPTION` or `PRODUCTION`
- `timestamp` INTEGER — unix epoch, UTC
- `value` REAL — kWh
- `status` INTEGER
- Unique on `(metering_point_code, timestamp, type)`

Derived tables: `daily_aggregates` (daily kWh per point and type),
`daily_insolation` (Open-Meteo weather per point and day),
`skipped_metering_months` (months the HEP API has no data for).

## HTTP routes

All routes are public — there is no authentication.

- `GET /` — redirects to `/mjesecno`
- `GET /mjesecno` — monthly view (charts + widgets)
- `GET /godisnje` — yearly view (monthly aggregated charts + widgets)
- `GET /postavke` — settings page (HEP status, connection test, tariff models)
- `POST /postavke/mjerno-mjesto/:code/tarifa` — set a metering point's tariff model
- `GET /api/readings` — daily and 15-min readings for a month (JSON)
- `GET /api/readings/year` — monthly aggregated readings for a year (JSON)
- `GET /api/readings/hourly` — average kWh per hour-of-day for a month (JSON)
- `GET /api/readings/calendar` — daily totals for a year (JSON)
- `GET /api/insolation` / `GET /api/insolation/year` — weather data (JSON)
- `POST /api/hep-test` — verify the configured HEP credentials
- `POST /api/fetch/now` — trigger an immediate collection, blocks until done

Settings-page feedback is passed back through `?ok=` / `?err=` query params
(`redirectWithMessage` in handlers.go) — there is no session store for flashes.

## Background worker

`StartWorker` (periodic) runs two jobs: (1) every minute — backfill metering points
with `last_meter_reading_collection = NULL`, then re-collect points older than
`StaleReadingThreshold`; (2) once per day after
`DailyCollectionHour:DailyCollectionMinute` — refresh the current month for every
metering point.

`StartManualFetchWorker` serves `POST /api/fetch/now`, independently of the
periodic worker.

`runCollection(mode, discover, refTime)` is the single entry point for both;
`collectMode` is `modePending`, `modeStale` or `modeAll`. It skips the HEP API
entirely when no metering point matches the mode, except when `discover` is set and
no metering points are known yet (the login response is what reveals them).

### Collection flow
1. Get JWT from in-memory token cache, or authenticate via HEP login API
2. Upsert metering points from login response, geocode new ones
3. Determine months to fetch per metering point:
   - First-time collection (NULL): backfill all months from `available_from` to now
   - Subsequent: from the last collection month to the current month
4. For each metering point, fetch consumption (direction `P`) and/or production (direction `R`)
5. Filter out future-dated readings (API returns full month including future slots)
6. Insert with `ON CONFLICT DO NOTHING` (unique constraint handles duplicates), then
   refresh `daily_aggregates` for **every local month the stored readings touch**
   (`monthsTouched`) — a month's HEP response ends with the first slot of the next
   month, so writing month N extends day 1 of month N+1
7. Update `last_meter_reading_collection` — skipped when any month failed, so the
   gap is retried instead of looking collected

A past month's HEP call is skipped only when `MonthAlreadyCollected` finds at least
half the month's 15-minute slots stored. An existence test would be satisfied by
that single next-month boundary row and would silently skip the whole month.

### Token handling
- Single JWT cached in memory across cycles (`tokenCache` in worker.go)
- On 401 response → re-authenticate, retry once
- On failed login → clear cached token

### Fetch state
The last collection failure lives in memory (`FetchState` in worker.go) and is shown
on the monthly view. Nothing is persisted — there is no `last_fetch_error` column.

### HEP API data quirks
- Timestamps are naive strings in Europe/Zagreb timezone — parsed with `time.LoadLocation`
- Values use comma decimal separator (`"0,59600000"`) — converted to float64
- Trailing zero readings (future/unavailable slots) are skipped

## HEP API endpoints

### Login
`POST https://mjerenje.hep.hr/mjerenja/v1/api/user/login`
Body: `{"Username": "...", "Password": "..."}`
Returns JWT token + KupacList with OmmList (metering points).

### Readings
`POST https://mjerenje.hep.hr/mjerenja/v1/api/data/omm/{code}/krivulja/mjesec/{MM}.{YYYY}/smjer/{P|R}`
Returns array of `{Status, Sifra, Obis, Datum, Value}`.

## Configuration

Settings come from `config.ini` in the working directory, falling back to `.env`,
with real environment variables taking precedence over both (`loadConfigFile` in
config.go). The format is `KEY=value`; `;` comments and `[section]` headers are
tolerated and ignored, so keys stay flat.

| Variable | Description | Default |
|---|---|---|
| `DB_PATH` | SQLite database file | `./data/hepmjerenja.db` |
| `HEP_USERNAME` | mjerenje.hep.hr username | required |
| `HEP_PASSWORD` | mjerenje.hep.hr password | required |
| `PORT` | HTTP listen address | `:8000` |
| `LOG_DIR` | Log file directory | `.` |
| `LOG_LEVEL` | Logging level | `info` |
| `SQL_ECHO` | Print all SQL queries to stdout (`true`/`false`) | `false` |
| `DEBUG` | Disable HTML/CSS/JS minification | `false` |

## Releases

`.github/workflows/release.yml` runs on every push to `main`: it bumps the minor
version from the newest `vX.Y.Z` tag (first release is `v0.1.0`), cross-compiles
linux/amd64, linux/arm64, darwin/amd64, darwin/arm64 and windows/amd64 from a
single ubuntu runner, and publishes a GitHub release with archives and
`SHA256SUMS`. `[skip release]` in the merge commit message skips it.

Two things the released binaries depend on:
- `app/tzdata.go` embeds the IANA database. Without it the binary panics at init on
  any system with no `/usr/share/zoneinfo` — notably Windows.
- `CGO_ENABLED=0` is what makes one runner enough. A CGO SQLite driver would force
  native runners per platform.

`main.version` is stamped with `-ldflags -X`; unreleased builds report `dev`.

## Development

```bash
make run      # start with air (live reload)
make migrate  # run goose migrations up
```

templ files must be regenerated after editing `.templ` source files:
```bash
templ generate
```

### Docker

There is no compose file — the image is built and run directly:

```bash
make docker-build     # hepmjerenja:latest (+ version tag, + remote tags)
make docker-run       # docker run -d, port 8000
```

`make docker-run` mounts `config.ini` read-only into the container and the
`hepmjerenja-data` volume at `/app/data`. That volume is not optional: the SQLite
database lives there, so without it the data sits in the container's writable layer
and disappears with `docker rm`. The default `DB_PATH=./data/hepmjerenja.db`
resolves against WORKDIR `/app`, so the same settings file works locally and in
Docker.

The `Dockerfile` pre-creates `/app/data` and `/var/log/apps/hepmjerenja` owned by
UID 33 (the runtime user), so a named volume mounted at either path inherits that
ownership and the app can create the database plus its `-wal`/`-shm` sidecars.
The build stays `CGO_ENABLED=0` — that is why the SQLite driver must remain the
pure-Go `modernc.org/sqlite`.

Migrations can also be run from the CLI: `make migrate` / `migrate-down` /
`migrate-status`, which call `hepmjerenja migrate [up|down|status]` and reuse the
same embedded files and `Config.DSN()`. The goose CLI is no longer used, so no
`GOOSE_*` variables are needed.
