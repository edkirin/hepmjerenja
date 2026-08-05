package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// rePostalCode matches a leading Croatian 5-digit postal code, e.g. "10410 ...".
var rePostalCode = regexp.MustCompile(`^\d{5}`)

// geocodeBackoff prevents hammering geocoding APIs after a failure.
// A metering point code is skipped for geocodeBackoffDuration after any failed attempt.
var (
	geocodeBackoffMu       sync.Mutex
	geocodeBackoff         = map[string]time.Time{}
	geocodeBackoffDuration = GeocodingBackoff
)

// geocodeRateMu enforces a minimum interval between outgoing geocoding requests
// across all providers (Photon + Nominatim) to stay within their rate limits.
var (
	geocodeRateMu       sync.Mutex
	geocodeLastCall     time.Time
	geocodeMinInterval  = 1100 * time.Millisecond // ≤1 req/sec
)

func geocodeRateWait() {
	geocodeRateMu.Lock()
	defer geocodeRateMu.Unlock()
	if wait := geocodeMinInterval - time.Since(geocodeLastCall); wait > 0 {
		time.Sleep(wait)
	}
	geocodeLastCall = time.Now()
}

// geocodeAddress tries three strategies in order, returning on the first success:
//  1. Photon (photon.komoot.io) with the full address — good at European/Croatian addresses
//  2. Nominatim structured query using only the postal code — sufficient for weather data (~10 km precision)
//  3. Nominatim free-text query with the full address + "Croatia"
func geocodeAddress(ctx context.Context, address string) (lat, lon float64, err error) {
	if lat, lon, err = geocodePhoton(ctx, address); err == nil {
		return lat, lon, nil
	}

	if pc := rePostalCode.FindString(address); pc != "" {
		if lat, lon, err = geocodeNominatimPostalCode(ctx, pc); err == nil {
			return lat, lon, nil
		}
	}

	return geocodeNominatimFreeText(ctx, address)
}

// geocodePhoton calls the Photon geocoder (photon.komoot.io).
// Photon is based on OpenStreetMap and handles European addresses well.
func geocodePhoton(ctx context.Context, address string) (float64, float64, error) {
	q := url.Values{}
	q.Set("q", address+", Croatia")
	q.Set("limit", "1")
	q.Set("lang", "hr")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://photon.komoot.io/api/?"+q.Encode(), nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", "hepmjerenja/1.0")

	geocodeRateWait()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("photon request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, fmt.Errorf("photon read: %w", err)
	}

	var result struct {
		Features []struct {
			Geometry struct {
				Coordinates [2]float64 `json:"coordinates"` // [lon, lat]
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, 0, fmt.Errorf("photon decode (status=%d url=%s body=%q): %w",
			resp.StatusCode, req.URL.String(), truncate(body, 500), err)
	}
	if len(result.Features) == 0 {
		return 0, 0, fmt.Errorf("photon: no result for %q", address)
	}

	coords := result.Features[0].Geometry.Coordinates
	return coords[1], coords[0], nil // lat, lon
}

// geocodeNominatimPostalCode queries Nominatim using only the postal code and
// country code. Precision is at the postal-district level (~5–10 km), which is
// sufficient for solar irradiance data.
func geocodeNominatimPostalCode(ctx context.Context, postalCode string) (float64, float64, error) {
	q := url.Values{}
	q.Set("postalcode", postalCode)
	q.Set("country", "HR")
	q.Set("format", "json")
	q.Set("limit", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://nominatim.openstreetmap.org/search?"+q.Encode(), nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", "hepmjerenja/1.0")

	geocodeRateWait()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("nominatim postal code request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, fmt.Errorf("nominatim postal code read: %w", err)
	}

	var results []struct {
		Lat string `json:"lat"`
		Lon string `json:"lon"`
	}
	if err := json.Unmarshal(body, &results); err != nil {
		return 0, 0, fmt.Errorf("nominatim postal code decode (status=%d url=%s body=%q): %w",
			resp.StatusCode, req.URL.String(), truncate(body, 500), err)
	}
	if len(results) == 0 {
		return 0, 0, fmt.Errorf("nominatim: no result for postal code %q", postalCode)
	}

	lat, err := strconv.ParseFloat(results[0].Lat, 64)
	if err != nil {
		return 0, 0, err
	}
	lon, err := strconv.ParseFloat(results[0].Lon, 64)
	if err != nil {
		return 0, 0, err
	}
	return lat, lon, nil
}

// geocodeNominatimFreeText is the original Nominatim free-text fallback.
func geocodeNominatimFreeText(ctx context.Context, address string) (float64, float64, error) {
	q := url.Values{}
	q.Set("q", address+", Croatia")
	q.Set("format", "json")
	q.Set("limit", "1")
	q.Set("accept-language", "hr")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://nominatim.openstreetmap.org/search?"+q.Encode(), nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", "hepmjerenja/1.0")

	geocodeRateWait()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("nominatim request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, fmt.Errorf("nominatim read: %w", err)
	}

	var results []struct {
		Lat string `json:"lat"`
		Lon string `json:"lon"`
	}
	if err := json.Unmarshal(body, &results); err != nil {
		return 0, 0, fmt.Errorf("nominatim decode (status=%d url=%s body=%q): %w",
			resp.StatusCode, req.URL.String(), truncate(body, 500), err)
	}
	if len(results) == 0 {
		return 0, 0, fmt.Errorf("nominatim: no result for %q", address)
	}

	lat, err := strconv.ParseFloat(results[0].Lat, 64)
	if err != nil {
		return 0, 0, err
	}
	lon, err := strconv.ParseFloat(results[0].Lon, 64)
	if err != nil {
		return 0, 0, err
	}
	return lat, lon, nil
}

// truncate returns up to n bytes of b as a string, for safe inclusion in error messages.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// geocodePointIfNeeded checks whether a metering point already has stored
// coordinates. If not, it geocodes the address and persists the result.
// Errors are only logged — missing coordinates are non-fatal and insolation
// data is simply omitted from the UI.
func geocodePointIfNeeded(ctx context.Context, db *DB, code, address string, log zerolog.Logger) {
	// Skip if a recent attempt already failed — avoid hammering geocoding APIs.
	geocodeBackoffMu.Lock()
	if t, ok := geocodeBackoff[code]; ok && time.Since(t) < geocodeBackoffDuration {
		geocodeBackoffMu.Unlock()
		return
	}
	geocodeBackoffMu.Unlock()

	var lat, lon *float64
	err := db.QueryRow(ctx,
		`SELECT latitude, longitude FROM metering_points WHERE code = ?`, code,
	).Scan(&lat, &lon)
	if err != nil {
		log.Error().Err(err).Str("code", code).Msg("failed to check metering point location")
		return
	}
	if lat != nil && lon != nil {
		return // already geocoded
	}

	newLat, newLon, err := geocodeAddress(ctx, address)
	if err != nil {
		geocodeBackoffMu.Lock()
		geocodeBackoff[code] = time.Now()
		geocodeBackoffMu.Unlock()
		log.Warn().Err(err).Str("code", code).Str("address", address).Msg("geocoding failed, will retry after 24h")
		return
	}

	geocodeBackoffMu.Lock()
	delete(geocodeBackoff, code)
	geocodeBackoffMu.Unlock()

	if err := UpdateMeteringPointLocation(ctx, db, code, newLat, newLon); err != nil {
		log.Error().Err(err).Str("code", code).Msg("failed to store metering point location")
		return
	}
	log.Info().Str("code", code).Float64("lat", newLat).Float64("lon", newLon).Msg("geocoded metering point")
}
