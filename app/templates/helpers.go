package templates

import (
	"fmt"
	"strings"
	"time"
)

// AvailableMonthsJSON returns a JavaScript array literal of "YYYY-MM" strings
// for embedding directly in a <script> tag.
func AvailableMonthsJSON(months []time.Time) string {
	quoted := make([]string, len(months))
	for i, m := range months {
		quoted[i] = `"` + m.Format("2006-01") + `"`
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// AvailableYearsJSON returns a JS array literal of year integers,
// excluding excludeYear, for embedding in a <script> tag or data attribute.
func AvailableYearsJSON(years []int, excludeYear int) string {
	parts := make([]string, 0, len(years))
	for _, y := range years {
		if y != excludeYear {
			parts = append(parts, fmt.Sprintf("%d", y))
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}
