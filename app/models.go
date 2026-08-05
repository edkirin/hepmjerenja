package main

import (
	"time"
)

// ──────────────────────────────────────────────
// Database models — correspond to PostgreSQL tables
// defined in the migrations/ directory.
// ──────────────────────────────────────────────

// HepCredentials holds the mjerenje.hep.hr login used for all collection. The
// application is single-user, so the credentials come from the environment
// (HEP_USERNAME / HEP_PASSWORD) rather than from the database.
type HepCredentials struct {
	Username string
	Password string
}

// IsSet reports whether both the username and the password are configured.
func (c HepCredentials) IsSet() bool {
	return c.Username != "" && c.Password != ""
}

// MeteringPoint represents a row in the "metering_points" table. Each metering
// point is an electricity meter identified by a unique code (HEP's "Šifra").
// A metering point can be both a consumer (draws power) and a producer
// (feeds power back, e.g. solar panels).
type MeteringPoint struct {
	Code                       string
	Address                    string
	OIB                        string // Croatian personal identification number (OIB)
	Consumer                   bool   // true if this point consumes electricity
	Producer                   bool   // true if this point produces electricity
	AvailableFrom              *time.Time
	AvailableTo                *time.Time
	Latitude                   *float64   // WGS-84 decimal degrees; nil until geocoded
	Longitude                  *float64   // WGS-84 decimal degrees; nil until geocoded
	LastMeterReadingCollection *time.Time // nil means never collected
	TariffModel                string     // see templates.TariffModel* constants
}

// MeterReading represents a row in the "meter_readings" table. Each reading
// is a single 15-minute interval measurement from a metering point, with a
// type indicating whether it's consumption or production.
type MeterReading struct {
	ID                int64
	MeteringPointCode string // FK to metering_points.code
	Type              string // "CONSUMPTION" or "PRODUCTION" (metering_type_enum)
	Timestamp         time.Time
	Value             float64 // energy value in kWh
	Status            int16   // HEP status code (0 = normal)
}

// DailyReading is an aggregated view of meter readings summed per day,
// used to populate charts on the index page.
type DailyReading struct {
	Day   string  `json:"day"`   // day-of-month as string, e.g. "1", "15"
	Value float64 `json:"value"` // total kWh for that day
}

// ReadingPoint is a single 15-minute interval measurement used for the
// detailed curve chart on the index page.
type ReadingPoint struct {
	Time  string  `json:"time"`  // formatted as "02.01. 15:04"
	Value float64 `json:"value"` // kWh
}

// CalendarDay holds daily totals for the calendar heatmap on the yearly view.
// Date is formatted as "YYYY-MM-DD".
type CalendarDay struct {
	Date        string  `json:"date"`
	Consumption float64 `json:"consumption"`
	Production  float64 `json:"production"`
}

// InsolationDay holds Open-Meteo daily weather data for one day.
// Used to render weather icons and summaries on the daily bar chart.
type InsolationDay struct {
	Day         int     `json:"day"`         // day-of-month (1–31)
	Weathercode int     `json:"weathercode"` // WMO weather interpretation code
	Radiation   float64 `json:"radiation"`   // shortwave radiation sum in MJ/m²
	Sunrise     string  `json:"sunrise"`     // local time "HH:MM", empty if unavailable
	Sunset      string  `json:"sunset"`      // local time "HH:MM", empty if unavailable
	TempMax     float64 `json:"temp_max"`    // maximum temperature at 2 m (°C)
	TempMin     float64 `json:"temp_min"`    // minimum temperature at 2 m (°C)
}

// MonthlyInsolation holds aggregated Open-Meteo data for one calendar month.
// Used by the yearly insolation chart.
type MonthlyInsolation struct {
	Month     int     `json:"month"`     // month number (1–12)
	Radiation float64 `json:"radiation"` // sum of daily shortwave radiation (MJ/m²)
	TempMax   float64 `json:"temp_max"`  // average daily max temperature (°C)
	TempMin   float64 `json:"temp_min"`  // average daily min temperature (°C)
}

// Metering type constants matching the PostgreSQL metering_type_enum values.
const (
	MeteringTypeConsumption = "CONSUMPTION"
	MeteringTypeProduction  = "PRODUCTION"
)

// ──────────────────────────────────────────────
// HEP API models — represent JSON request/response
// structures from the mjerenje.hep.hr API.
// ──────────────────────────────────────────────

// HepLoginRequest is the JSON body sent to the HEP login endpoint.
type HepLoginRequest struct {
	Username string `json:"Username"`
	Password string `json:"Password"`
}

// HepLoginResponse holds the result of a successful HEP login. As of 2026,
// the API returns the customer list as the top-level JSON array (no wrapper),
// and the JWT is delivered in an `accessToken` Set-Cookie header rather than
// in the response body. We populate Token from the cookie and KupacList from
// the body so the rest of the code can use them uniformly.
type HepLoginResponse struct {
	Token     string     // JWT bearer token extracted from the accessToken cookie
	KupacList []HepKupac // list of customers (Kupac = customer)
}

// HepKupac represents a customer entry in the login response.
// "Kupac" is Croatian for "customer".
type HepKupac struct {
	OmmList []HepOmm `json:"OmmList"` // list of metering points (OMM = obračunsko mjerno mjesto)
	Naziv   string   `json:"Naziv"`   // customer name
	Oib     string   `json:"Oib"`     // customer OIB
}

// HepOmm represents a metering point in the HEP API response.
// "OMM" stands for "Obračunsko Mjerno Mjesto" (billing metering point).
type HepOmm struct {
	Adresa      string `json:"Adresa"`      // address of the metering point
	Oib         string `json:"Oib"`         // OIB associated with this point
	Sifra       string `json:"Sifra"`       // unique metering point code → metering_points.code
	Potrosac    bool   `json:"Potrosac"`    // true if consumer → metering_points.consumer
	Proizvodjac bool   `json:"Proizvodjac"` // true if producer → metering_points.producer
	Obracun     bool   `json:"Obracun"`     // whether billing is active
	MjesecOd    string `json:"MjesecOd"`    // data available from (ISO datetime) → metering_points.available_from
	MjesecDo    string `json:"MjesecDo"`    // data available to (ISO datetime) → metering_points.available_to
}

// ManualFetchRequest is a one-off collection request submitted by the "Osvježi
// podatke" button. ResultCh receives the collection error (or nil on success);
// the HTTP handler blocks on it so the response reflects the actual outcome.
type ManualFetchRequest struct {
	// RefTime is the reference month for the collection. getMonthsToCollect keys
	// off its month/year, so pointing it at a given month fetches that month.
	RefTime  time.Time
	ResultCh chan error
}

// HepReading represents a single meter reading from the HEP readings API.
// The API returns readings in 15-minute intervals for the entire month.
type HepReading struct {
	Status string `json:"Status"` // status code as string (e.g. "0")
	Sifra  string `json:"Sifra"`  // metering point code
	Obis   string `json:"Obis"`   // OBIS code (e.g. "LP: A+_T0" for consumption, "LP: A-_T0" for production)
	Datum  string `json:"Datum"`  // timestamp in "2006-01-02T15:04:05" format (Croatian local time, no timezone)
	Value  string `json:"Value"`  // energy value as string with comma decimal separator (e.g. "0,59600000")
}
