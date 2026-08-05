package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hepmjerenja/app/templates"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
)

// Handler holds shared dependencies for HTTP handlers.
type Handler struct {
	db             *DB
	logger         zerolog.Logger
	hepClient      *HepClient
	hepCredentials HepCredentials
	tokens         *tokenCache               // shared with the workers; see tokenCache
	manualFetchCh  chan<- ManualFetchRequest // submits one-off collection requests to the fetch worker
}

// render bridges Echo with templ component rendering.
func render(c echo.Context, component templ.Component) error {
	return component.Render(c.Request().Context(), c.Response())
}

// redirectWithMessage sends the user back to path with a success or error message
// carried in the query string. With no session store there is nowhere to keep a
// flash message, and a query param survives the redirect just as well.
func redirectWithMessage(c echo.Context, path, key, msg string) error {
	return c.Redirect(http.StatusFound, path+"?"+key+"="+url.QueryEscape(msg))
}

// messages reads the success and error messages set by redirectWithMessage.
func messages(c echo.Context) (successMsg, errorMsg string) {
	return c.QueryParam("ok"), c.QueryParam("err")
}

// selectedCode returns the metering point the request refers to, falling back to
// the first known point when the "code" query param is absent.
func (h *Handler) selectedCode(c echo.Context) (string, error) {
	if code := c.QueryParam("code"); code != "" {
		return code, nil
	}
	points, err := GetMeteringPoints(c.Request().Context(), h.db)
	if err != nil || len(points) == 0 {
		return "", err
	}
	return points[0].Code, nil
}

// ── Views ──────────────────────────────────────

func (h *Handler) handleIndex(c echo.Context) error {
	ctx := c.Request().Context()

	points, err := GetMeteringPoints(ctx, h.db)
	if err != nil {
		h.logger.Error().Err(err).Msg("Failed to get metering points")
	}

	// Determine selected metering point.
	selectedCode := c.QueryParam("code")
	if selectedCode == "" && len(points) > 0 {
		selectedCode = points[0].Code
	}

	now := time.Now()
	selectedYear, selectedMonthNum := now.Year(), int(now.Month())
	if p := c.QueryParam("month"); p != "" {
		if t, err := time.Parse("2006-01", p); err == nil {
			selectedYear, selectedMonthNum = t.Year(), int(t.Month())
		}
	}

	var availableMonths []time.Time
	if selectedCode != "" {
		availableMonths, err = GetAvailableMonths(ctx, h.db, selectedCode)
		if err != nil {
			h.logger.Error().Err(err).Msg("Failed to get available months")
		}
	}
	// Always include the current month so the user can navigate to it even
	// when no readings have been stored yet (data may arrive later in the day).
	currentMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	hasCurrentMonth := false
	for _, m := range availableMonths {
		if m.Year() == currentMonthStart.Year() && m.Month() == currentMonthStart.Month() {
			hasCurrentMonth = true
			break
		}
	}
	if !hasCurrentMonth {
		availableMonths = append([]time.Time{currentMonthStart}, availableMonths...)
	}
	if len(availableMonths) == 0 {
		availableMonths = []time.Time{currentMonthStart}
	}

	// Convert to template-friendly struct.
	tplPoints := make([]templates.Point, len(points))
	for i, p := range points {
		tplPoints[i] = templates.Point{Code: p.Code, Address: p.Address}
	}

	selectedMonth := time.Date(selectedYear, time.Month(selectedMonthNum), 1, 0, 0, 0, 0, time.UTC)
	return render(c, templates.Index(templates.IndexProps{
		SelectedMonth:   selectedMonth,
		AvailableMonths: availableMonths,
		Points:          tplPoints,
		SelectedCode:    selectedCode,
		HepConfigured:   h.hepCredentials.IsSet(),
		LastFetchError:  FetchState.LastError(),
	}))
}

func (h *Handler) handleYearlyView(c echo.Context) error {
	ctx := c.Request().Context()

	points, err := GetMeteringPoints(ctx, h.db)
	if err != nil {
		h.logger.Error().Err(err).Msg("Failed to get metering points")
	}

	selectedCode := c.QueryParam("code")
	if selectedCode == "" && len(points) > 0 {
		selectedCode = points[0].Code
	}

	// Derive available years from months with data.
	var availableMonths []time.Time
	if selectedCode != "" {
		availableMonths, err = GetAvailableMonths(ctx, h.db, selectedCode)
		if err != nil {
			h.logger.Error().Err(err).Msg("Failed to get available months")
		}
	}
	seen := make(map[int]bool)
	var availableYears []int
	for _, m := range availableMonths {
		y := m.Year()
		if !seen[y] {
			seen[y] = true
			availableYears = append(availableYears, y)
		}
	}
	// Always include the current year even when no data has been stored yet.
	currentYear := time.Now().Year()
	if !seen[currentYear] {
		availableYears = append([]int{currentYear}, availableYears...)
	}
	if len(availableYears) == 0 {
		availableYears = []int{currentYear}
	}

	selectedYear := availableYears[0] // availableMonths is ordered DESC so first year is newest
	if p := c.QueryParam("year"); p != "" {
		if y := 0; len(p) == 4 {
			fmt.Sscanf(p, "%d", &y)
			if y > 0 {
				selectedYear = y
			}
		}
	}

	tplPoints := make([]templates.Point, len(points))
	for i, p := range points {
		tplPoints[i] = templates.Point{Code: p.Code, Address: p.Address}
	}

	return render(c, templates.Yearly(templates.YearlyProps{
		SelectedYear:   selectedYear,
		AvailableYears: availableYears,
		Points:         tplPoints,
		SelectedCode:   selectedCode,
	}))
}

func (h *Handler) handleSettings(c echo.Context) error {
	ctx := c.Request().Context()

	points, err := GetMeteringPoints(ctx, h.db)
	if err != nil {
		points = nil
	}
	tplPoints := make([]templates.SettingsMeteringPoint, len(points))
	for i, p := range points {
		tplPoints[i] = templates.SettingsMeteringPoint{
			Code:        p.Code,
			Address:     p.Address,
			TariffModel: p.TariffModel,
		}
	}

	successMsg, errorMsg := messages(c)
	return render(c, templates.Settings(templates.SettingsProps{
		HepUsername:    h.hepCredentials.Username,
		HepConfigured:  h.hepCredentials.IsSet(),
		MeteringPoints: tplPoints,
		SuccessMsg:     successMsg,
		ErrorMsg:       errorMsg,
	}))
}

func (h *Handler) handleUpdateMeteringPointTariff(c echo.Context) error {
	code := c.Param("code")
	tariffModel := c.FormValue("tariff_model")

	valid := map[string]bool{
		templates.TariffModelPlavi:  true,
		templates.TariffModelBijeli: true,
		templates.TariffModelCrveni: true,
	}
	if !valid[tariffModel] {
		return redirectWithMessage(c, "/postavke", "err", "Nepoznati tarifni model")
	}

	if err := UpdateMeteringPointTariff(c.Request().Context(), h.db, code, tariffModel); err != nil {
		h.logger.Error().Err(err).Str("code", code).Msg("Failed to save tariff model")
		return redirectWithMessage(c, "/postavke", "err", "Greška pri spremanju tarifnog modela")
	}
	return redirectWithMessage(c, "/postavke", "ok", "Tarifni model uspješno spremljen")
}

// handleHepTest verifies the HEP credentials from the configuration by performing
// a login against the HEP API. The resulting token replaces the one in the shared
// cache: HEP invalidates the previous session on every login, so handing the new
// token to the workers keeps them from having to re-authenticate afterwards.
func (h *Handler) handleHepTest(c echo.Context) error {
	if !h.hepCredentials.IsSet() {
		return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": "HEP pristupni podaci nisu postavljeni u config.ini datoteci"})
	}
	resp, err := h.hepClient.Login(c.Request().Context(), h.hepCredentials.Username, h.hepCredentials.Password)
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
	}
	h.tokens.Set(resp.Token)
	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// ── JSON API ───────────────────────────────────

func (h *Handler) handleAPIReadings(c echo.Context) error {
	ctx := c.Request().Context()

	code, err := h.selectedCode(c)
	if err != nil || code == "" {
		return c.JSON(http.StatusOK, map[string]any{
			"consumption": []DailyReading{}, "production": []DailyReading{},
			"all_consumption": []ReadingPoint{}, "all_production": []ReadingPoint{},
			"consumption_vt": []DailyReading{}, "consumption_nt": []DailyReading{},
			"production_vt": []DailyReading{}, "production_nt": []DailyReading{},
			"tariff_model": "",
		})
	}

	now := time.Now()
	year, month := now.Year(), int(now.Month())
	if p := c.QueryParam("month"); p != "" {
		if t, err := time.Parse("2006-01", p); err == nil {
			year, month = t.Year(), int(t.Month())
		}
	}

	consumption, production, err := GetDailyReadingsForMonth(ctx, h.db, code, year, month)
	if err != nil {
		h.logger.Error().Err(err).Msg("Failed to get daily readings")
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "query failed"})
	}
	if consumption == nil {
		consumption = []DailyReading{}
	}
	if production == nil {
		production = []DailyReading{}
	}

	allConsumption, allProduction, err := GetAllReadingsForMonth(ctx, h.db, code, year, month)
	if err != nil {
		h.logger.Error().Err(err).Msg("Failed to get all readings")
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "query failed"})
	}
	if allConsumption == nil {
		allConsumption = []ReadingPoint{}
	}
	if allProduction == nil {
		allProduction = []ReadingPoint{}
	}

	tariffModel, err := GetMeteringPointTariff(ctx, h.db, code)
	if err != nil {
		h.logger.Warn().Err(err).Str("code", code).Msg("Failed to get tariff model")
	}

	consumptionVT := []DailyReading{}
	consumptionNT := []DailyReading{}
	productionVT := []DailyReading{}
	productionNT := []DailyReading{}
	if tariffModel == templates.TariffModelBijeli || tariffModel == templates.TariffModelCrveni {
		cvt, cnt, err := GetDailyVTNTForMonth(ctx, h.db, code, MeteringTypeConsumption, year, month)
		if err != nil {
			h.logger.Warn().Err(err).Str("code", code).Msg("Failed to get VT/NT consumption readings")
		} else {
			if cvt != nil {
				consumptionVT = cvt
			}
			if cnt != nil {
				consumptionNT = cnt
			}
		}
		pvt, pnt, err := GetDailyVTNTForMonth(ctx, h.db, code, MeteringTypeProduction, year, month)
		if err != nil {
			h.logger.Warn().Err(err).Str("code", code).Msg("Failed to get VT/NT production readings")
		} else {
			if pvt != nil {
				productionVT = pvt
			}
			if pnt != nil {
				productionNT = pnt
			}
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"consumption":     consumption,
		"production":      production,
		"all_consumption": allConsumption,
		"all_production":  allProduction,
		"consumption_vt":  consumptionVT,
		"consumption_nt":  consumptionNT,
		"production_vt":   productionVT,
		"production_nt":   productionNT,
		"tariff_model":    tariffModel,
	})
}

func (h *Handler) handleAPIReadingsYear(c echo.Context) error {
	ctx := c.Request().Context()

	code, err := h.selectedCode(c)
	if err != nil || code == "" {
		return c.JSON(http.StatusOK, map[string]any{
			"consumption":    []DailyReading{},
			"production":     []DailyReading{},
			"consumption_vt": []DailyReading{},
			"consumption_nt": []DailyReading{},
			"production_vt":  []DailyReading{},
			"production_nt":  []DailyReading{},
			"tariff_model":   "",
		})
	}

	year := time.Now().Year()
	if p := c.QueryParam("year"); len(p) == 4 {
		fmt.Sscanf(p, "%d", &year)
	}

	consumption, production, err := GetMonthlyReadingsForYear(ctx, h.db, code, year)
	if err != nil {
		h.logger.Error().Err(err).Msg("Failed to get monthly readings")
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "query failed"})
	}
	if consumption == nil {
		consumption = []DailyReading{}
	}
	if production == nil {
		production = []DailyReading{}
	}

	tariffModel, err := GetMeteringPointTariff(ctx, h.db, code)
	if err != nil {
		h.logger.Warn().Err(err).Str("code", code).Msg("Failed to get tariff model")
	}

	consumptionVT := []DailyReading{}
	consumptionNT := []DailyReading{}
	productionVT := []DailyReading{}
	productionNT := []DailyReading{}
	if tariffModel == templates.TariffModelBijeli || tariffModel == templates.TariffModelCrveni {
		cvt, cnt, err := GetMonthlyVTNTForYear(ctx, h.db, code, MeteringTypeConsumption, year)
		if err != nil {
			h.logger.Warn().Err(err).Str("code", code).Msg("Failed to get yearly VT/NT consumption")
		} else {
			if cvt != nil {
				consumptionVT = cvt
			}
			if cnt != nil {
				consumptionNT = cnt
			}
		}
		pvt, pnt, err := GetMonthlyVTNTForYear(ctx, h.db, code, MeteringTypeProduction, year)
		if err != nil {
			h.logger.Warn().Err(err).Str("code", code).Msg("Failed to get yearly VT/NT production")
		} else {
			if pvt != nil {
				productionVT = pvt
			}
			if pnt != nil {
				productionNT = pnt
			}
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"consumption":    consumption,
		"production":     production,
		"consumption_vt": consumptionVT,
		"consumption_nt": consumptionNT,
		"production_vt":  productionVT,
		"production_nt":  productionNT,
		"tariff_model":   tariffModel,
	})
}

func (h *Handler) handleAPIReadingsHourly(c echo.Context) error {
	ctx := c.Request().Context()

	code, err := h.selectedCode(c)
	if err != nil || code == "" {
		return c.JSON(http.StatusOK, map[string]any{
			"consumption": []DailyReading{}, "production": []DailyReading{},
		})
	}

	now := time.Now()
	year, month := now.Year(), int(now.Month())
	if p := c.QueryParam("month"); p != "" {
		if t, err := time.Parse("2006-01", p); err == nil {
			year, month = t.Year(), int(t.Month())
		}
	}

	consumption, production, err := GetHourlyAverageForMonth(ctx, h.db, code, year, month)
	if err != nil {
		h.logger.Error().Err(err).Msg("Failed to get hourly averages")
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "query failed"})
	}
	if consumption == nil {
		consumption = []DailyReading{}
	}
	if production == nil {
		production = []DailyReading{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"consumption": consumption,
		"production":  production,
	})
}

func (h *Handler) handleAPIReadingsCalendar(c echo.Context) error {
	ctx := c.Request().Context()

	code, err := h.selectedCode(c)
	if err != nil || code == "" {
		return c.JSON(http.StatusOK, map[string]any{"days": []CalendarDay{}})
	}

	year := time.Now().Year()
	if p := c.QueryParam("year"); len(p) == 4 {
		fmt.Sscanf(p, "%d", &year)
	}

	days, err := GetDailyTotalsForYear(ctx, h.db, code, year)
	if err != nil {
		h.logger.Error().Err(err).Msg("Failed to get calendar data")
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "query failed"})
	}
	if days == nil {
		days = []CalendarDay{}
	}
	return c.JSON(http.StatusOK, map[string]any{"days": days})
}

func (h *Handler) handleAPIInsolation(c echo.Context) error {
	ctx := c.Request().Context()

	code, err := h.selectedCode(c)
	if err != nil || code == "" {
		return c.JSON(http.StatusOK, map[string]any{"days": []InsolationDay{}})
	}

	now := time.Now()
	year, month := now.Year(), int(now.Month())
	if p := c.QueryParam("month"); p != "" {
		if t, err := time.Parse("2006-01", p); err == nil {
			year, month = t.Year(), int(t.Month())
		}
	}

	days, err := GetDailyInsolationForMonth(ctx, h.db, code, year, month)
	if err != nil {
		h.logger.Warn().Err(err).Str("code", code).Msg("Failed to get insolation data")
		return c.JSON(http.StatusOK, map[string]any{"days": []InsolationDay{}})
	}
	if days == nil {
		days = []InsolationDay{}
	}
	return c.JSON(http.StatusOK, map[string]any{"days": days})
}

func (h *Handler) handleAPIInsolationYear(c echo.Context) error {
	ctx := c.Request().Context()

	code, err := h.selectedCode(c)
	if err != nil || code == "" {
		return c.JSON(http.StatusOK, map[string]any{"months": []MonthlyInsolation{}})
	}

	year := time.Now().Year()
	if p := c.QueryParam("year"); len(p) == 4 {
		fmt.Sscanf(p, "%d", &year)
	}

	months, err := GetMonthlyInsolationForYear(ctx, h.db, code, year)
	if err != nil {
		h.logger.Warn().Err(err).Str("code", code).Msg("Failed to get yearly insolation data")
		return c.JSON(http.StatusOK, map[string]any{"months": []MonthlyInsolation{}})
	}
	if months == nil {
		months = []MonthlyInsolation{}
	}
	return c.JSON(http.StatusOK, map[string]any{"months": months})
}

// ── Manual fetch ───────────────────────────────

// fetchRefTime returns the reference month for a manual fetch. When the request
// carries a valid "month" query param (format "2006-01", sent by the monthly view
// for the month currently being viewed) it points at that month; otherwise it
// defaults to the current month. The day is fixed at the 15th so the reference
// sits safely inside the month regardless of length.
func fetchRefTime(c echo.Context) time.Time {
	if p := c.QueryParam("month"); p != "" {
		if t, err := time.Parse("2006-01", p); err == nil {
			return time.Date(t.Year(), t.Month(), 15, 12, 0, 0, 0, time.Local)
		}
	}
	return time.Now()
}

// handleForceFetch triggers an immediate collection using the HEP credentials
// from the configuration and blocks until the fetch worker reports back.
func (h *Handler) handleForceFetch(c echo.Context) error {
	if !h.hepCredentials.IsSet() {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "HEP pristupni podaci nisu postavljeni. Postavite HEP_USERNAME i HEP_PASSWORD u config.ini datoteci."})
	}

	resultCh := make(chan error, 1)
	req := ManualFetchRequest{
		RefTime:  fetchRefTime(c),
		ResultCh: resultCh,
	}

	select {
	case h.manualFetchCh <- req:
	default:
		return c.JSON(http.StatusTooManyRequests, map[string]string{"error": "Dohvaćanje je već u tijeku. Pokušajte za trenutak."})
	}

	select {
	case fetchErr := <-resultCh:
		if fetchErr != nil {
			msg := "Greška pri dohvaćanju podataka."
			if strings.Contains(fetchErr.Error(), "unauthorized") || strings.Contains(fetchErr.Error(), "login") ||
				strings.Contains(fetchErr.Error(), ErrAuthFailed.Error()) {
				msg = "Neispravno korisničko ime ili lozinka. Provjerite HEP pristupne podatke u config.ini datoteci."
			}
			return c.JSON(http.StatusBadGateway, map[string]string{"error": msg})
		}
		return c.JSON(http.StatusOK, map[string]string{"message": "Podaci su uspješno dohvaćeni."})
	case <-c.Request().Context().Done():
		return c.JSON(http.StatusGatewayTimeout, map[string]string{"error": "Dohvaćanje je predugo trajalo."})
	}
}
