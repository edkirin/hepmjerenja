package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// zagrebLocation is the timezone used to interpret timestamps from the HEP API.
// The API returns naive datetime strings (no timezone info) in Croatian local time,
// so we must parse them with the Europe/Zagreb location to get correct UTC values
// before storing in the database's timestamptz columns.
var zagrebLocation *time.Location

func init() {
	var err error
	zagrebLocation, err = time.LoadLocation("Europe/Zagreb")
	if err != nil {
		panic("failed to load Europe/Zagreb timezone: " + err.Error())
	}
}

// HepClient is an HTTP client for the HEP mjerenje.hep.hr API.
// It handles authentication (login) and fetching meter readings
// for consumption and production data.
type HepClient struct {
	client  *http.Client
	baseURL string
	logger  zerolog.Logger
}

// NewHepClient creates a new HEP API client with a 30-second request timeout.
func NewHepClient(logger zerolog.Logger) *HepClient {
	return &HepClient{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		baseURL: "https://mjerenje.hep.hr/mjerenja/v1.1/api",
		logger:  logger.With().Str("component", "hep").Logger(),
	}
}

// logRequest logs the HTTP request/response.
// Non-2xx: always logged at warn; if debug is enabled, a second entry with full
// request/response detail is also emitted at debug level.
// 2xx: debug only.
func (h *HepClient) logRequest(req *http.Request, reqBody []byte, resp *http.Response, respBody []byte, username, meteringPointCode string) {
	isError := resp.StatusCode < 200 || resp.StatusCode >= 300
	debugEnabled := h.logger.Debug().Enabled()

	if !isError && !debugEnabled {
		return
	}

	reqHeaders := make(map[string]string, len(req.Header))
	for k, v := range req.Header {
		reqHeaders[k] = strings.Join(v, ", ")
	}
	resHeaders := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		resHeaders[k] = strings.Join(v, ", ")
	}

	attach := func(e *zerolog.Event, full bool) *zerolog.Event {
		e = e.Str("method", req.Method).
			Str("url", req.URL.String()).
			Str("username", username).
			Int("status", resp.StatusCode)
		if meteringPointCode != "" {
			e = e.Str("metering_point", meteringPointCode)
		}
		if full {
			e = e.Any("request_headers", reqHeaders).
				Any("response_headers", resHeaders).
				Str("response_body", string(respBody))
			if len(reqBody) > 0 {
				e = e.Str("request_body", string(reqBody))
			}
		}
		return e
	}

	if isError {
		// Warn: key fields always visible.
		attach(h.logger.Warn(), false).Msg("hep request error")
		// Debug: full request/response detail when debug is enabled.
		if debugEnabled {
			attach(h.logger.Debug(), true).Msg("hep request error detail")
		}
	} else {
		attach(h.logger.Debug(), true).Msg("hep request")
	}
}

// logSend logs the outgoing request at info level before it is sent.
func (h *HepClient) logSend(method, url, username, meteringPointCode string) {
	e := h.logger.Info().Str("method", method).Str("url", url).Str("username", username)
	if meteringPointCode != "" {
		e = e.Str("metering_point", meteringPointCode)
	}
	e.Msg("hep sending request")
}

// Login authenticates a user against the HEP API using their credentials.
// On success, returns a HepLoginResponse containing the JWT bearer token
// (extracted from the accessToken Set-Cookie header) and the customer list
// parsed from the response body.
func (h *HepClient) Login(ctx context.Context, username, password string) (*HepLoginResponse, error) {
	body, err := json.Marshal(HepLoginRequest{
		Username: username,
		Password: password,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.baseURL+"/user/login", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create login request: %w", err)
	}

	// Set headers to mimic a browser request, as the HEP API may validate these.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", "https://mjerenje.hep.hr")
	req.Header.Set("Referer", "https://mjerenje.hep.hr/mjerenja/login")

	h.logSend(req.Method, req.URL.String(), username, "")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read login response: %w", err)
	}
	h.logRequest(req, body, resp, respBody, username, "")

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("login failed: status=%d url=%s body=%s", resp.StatusCode, req.URL.String(), string(respBody))
	}

	// The body is now the bare KupacList array — no wrapper, no token field.
	var kupacList []HepKupac
	if err := json.Unmarshal(respBody, &kupacList); err != nil {
		return nil, fmt.Errorf("decode login response: %w", err)
	}

	// JWT is delivered as the `accessToken` Set-Cookie header.
	var token string
	for _, c := range resp.Cookies() {
		if c.Name == "accessToken" {
			token = c.Value
			break
		}
	}
	if token == "" {
		return nil, fmt.Errorf("login response missing accessToken cookie")
	}

	return &HepLoginResponse{Token: token, KupacList: kupacList}, nil
}

// FetchReadings retrieves meter readings for a specific metering point, month, and direction.
// The direction parameter controls what type of data is returned:
//   - "P" (Potrošnja) = consumption readings (energy drawn from grid)
//   - "R" (Reakcija)   = production readings (energy fed back to grid, e.g. solar)
//
// The API returns readings in 15-minute intervals for the entire requested month,
// including future time slots padded with zero values.
// Returns ErrUnauthorized if the token has expired (HTTP 401), allowing the caller
// to re-authenticate and retry.
func (h *HepClient) FetchReadings(ctx context.Context, token, code string, month, year int, direction, username string) ([]HepReading, error) {
	// URL format: /data/omm/{code}/krivulja/mjesec/{MM}.{YYYY}/smjer/{direction}
	// Month must be zero-padded (e.g. "03" not "3") for the API to accept it.
	url := fmt.Sprintf("%s/data/omm/%s/krivulja/mjesec/%02d.%d/smjer/%s",
		h.baseURL, code, month, year, direction)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create readings request: %w", err)
	}

	// Authenticate using the JWT obtained from Login(). The HEP API now sets the
	// JWT as an `accessToken` cookie at login time, so we send it back as a cookie
	// here; the legacy Authorization header is kept for backwards compatibility.
	req.Header.Set("Authorization", "Bearer "+token)
	req.AddCookie(&http.Cookie{Name: "accessToken", Value: token})
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", "https://mjerenje.hep.hr")
	req.Header.Set("Referer", "https://mjerenje.hep.hr/mjerenja/mjerni")

	h.logSend(req.Method, url, username, code)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("readings request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read readings response: %w", err)
	}
	h.logRequest(req, nil, resp, respBody, username, code)

	// Return a sentinel error on 401 so the caller can re-authenticate and retry.
	// Also treat "Neispravni podaci prijave" as an auth failure whatever the status
	// code: HEP allows a single active session per account, so any newer login
	// (another device, the connection test, a browser) invalidates the session this
	// JWT refers to. The API reports that as 400 — and has used 404 in the past —
	// while the JWT itself is still within its expiry window.
	if resp.StatusCode == http.StatusUnauthorized ||
		strings.Contains(string(respBody), "Neispravni podaci prijave") {
		return nil, ErrUnauthorized
	}

	// Treat 404 responses that indicate permanently missing data as ErrNoDataForMonth.
	// These messages mean the metering point has no readings for this period and
	// retrying will never help.
	if resp.StatusCode == http.StatusNotFound {
		body := string(respBody)
		if strings.Contains(body, "ne sadrži podatke za datum") ||
			strings.Contains(body, "ne ulazi u dozvoljeni raspon") {
			return nil, ErrNoDataForMonth
		}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("readings request failed: status=%d url=%s body=%s", resp.StatusCode, url, string(respBody))
	}

	var readings []HepReading
	if err := json.Unmarshal(respBody, &readings); err != nil {
		return nil, fmt.Errorf("decode readings response: %w", err)
	}

	return readings, nil
}

// ErrUnauthorized is a sentinel error returned by FetchReadings when the API
// responds with HTTP 401, indicating the JWT token has expired or is invalid.
var ErrUnauthorized = fmt.Errorf("unauthorized")

// ErrNoDataForMonth is returned by FetchReadings when the API responds with HTTP 404
// and the body indicates that no data exists for the requested month — either the
// metering point has no readings for that period or the date is outside its allowed
// range. This is a permanent condition; the caller should skip the month in future runs.
var ErrNoDataForMonth = fmt.Errorf("no data for month")

// parseHepValue converts a HEP API value string to a float64. The API uses
// comma as the decimal separator (European format, e.g. "0,59600000"),
// so we replace it with a dot before parsing.
func parseHepValue(s string) (float64, error) {
	s = strings.ReplaceAll(s, ",", ".")
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}

// parseHepTimestamp parses a HEP API datetime string into a time.Time value.
// The naive strings have no timezone info and are interpreted as Europe/Zagreb time.
func parseHepTimestamp(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02T15:04:05", s, zagrebLocation)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t, nil
}
