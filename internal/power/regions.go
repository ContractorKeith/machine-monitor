package power

import (
	"os"
	"strings"
)

// Region is a typical household electricity price for one place.
type Region struct {
	Code     string  // "US", "US-CA", "GB", ...; "custom" is reserved for a typed-in rate
	Name     string  // "California"
	Currency string  // ISO 4217
	Rate     float64 // per kWh in Currency
}

// Regions are typical household rates per kWh in local currency (approximate recent
// averages), in upstream's display order: US average, US states, Canadian provinces, then
// countries. Time-of-use and tiered plans vary a lot, so people can type the rate from
// their bill instead.
var Regions = []Region{
	{"US", "United States (average)", "USD", 0.165},
	{"US-AL", "Alabama", "USD", 0.152},
	{"US-AK", "Alaska", "USD", 0.249},
	{"US-AZ", "Arizona", "USD", 0.151},
	{"US-AR", "Arkansas", "USD", 0.126},
	{"US-CA", "California", "USD", 0.316},
	{"US-CO", "Colorado", "USD", 0.153},
	{"US-CT", "Connecticut", "USD", 0.29},
	{"US-DE", "Delaware", "USD", 0.166},
	{"US-DC", "District of Columbia", "USD", 0.183},
	{"US-FL", "Florida", "USD", 0.15},
	{"US-GA", "Georgia", "USD", 0.14},
	{"US-HI", "Hawaii", "USD", 0.415},
	{"US-ID", "Idaho", "USD", 0.114},
	{"US-IL", "Illinois", "USD", 0.165},
	{"US-IN", "Indiana", "USD", 0.156},
	{"US-IA", "Iowa", "USD", 0.139},
	{"US-KS", "Kansas", "USD", 0.142},
	{"US-KY", "Kentucky", "USD", 0.13},
	{"US-LA", "Louisiana", "USD", 0.122},
	{"US-ME", "Maine", "USD", 0.27},
	{"US-MD", "Maryland", "USD", 0.186},
	{"US-MA", "Massachusetts", "USD", 0.303},
	{"US-MI", "Michigan", "USD", 0.193},
	{"US-MN", "Minnesota", "USD", 0.153},
	{"US-MS", "Mississippi", "USD", 0.133},
	{"US-MO", "Missouri", "USD", 0.13},
	{"US-MT", "Montana", "USD", 0.134},
	{"US-NE", "Nebraska", "USD", 0.12},
	{"US-NV", "Nevada", "USD", 0.153},
	{"US-NH", "New Hampshire", "USD", 0.245},
	{"US-NJ", "New Jersey", "USD", 0.194},
	{"US-NM", "New Mexico", "USD", 0.15},
	{"US-NY", "New York", "USD", 0.25},
	{"US-NC", "North Carolina", "USD", 0.139},
	{"US-ND", "North Dakota", "USD", 0.119},
	{"US-OH", "Ohio", "USD", 0.161},
	{"US-OK", "Oklahoma", "USD", 0.13},
	{"US-OR", "Oregon", "USD", 0.146},
	{"US-PA", "Pennsylvania", "USD", 0.18},
	{"US-RI", "Rhode Island", "USD", 0.29},
	{"US-SC", "South Carolina", "USD", 0.144},
	{"US-SD", "South Dakota", "USD", 0.13},
	{"US-TN", "Tennessee", "USD", 0.129},
	{"US-TX", "Texas", "USD", 0.153},
	{"US-UT", "Utah", "USD", 0.122},
	{"US-VT", "Vermont", "USD", 0.226},
	{"US-VA", "Virginia", "USD", 0.149},
	{"US-WA", "Washington", "USD", 0.126},
	{"US-WV", "West Virginia", "USD", 0.151},
	{"US-WI", "Wisconsin", "USD", 0.179},
	{"US-WY", "Wyoming", "USD", 0.133},
	{"CA-AB", "Alberta", "CAD", 0.25},
	{"CA-BC", "British Columbia", "CAD", 0.12},
	{"CA-MB", "Manitoba", "CAD", 0.11},
	{"CA-NB", "New Brunswick", "CAD", 0.14},
	{"CA-NL", "Newfoundland and Labrador", "CAD", 0.15},
	{"CA-NS", "Nova Scotia", "CAD", 0.19},
	{"CA-ON", "Ontario", "CAD", 0.15},
	{"CA-PE", "Prince Edward Island", "CAD", 0.18},
	{"CA-QC", "Quebec", "CAD", 0.08},
	{"CA-SK", "Saskatchewan", "CAD", 0.2},
	{"AU", "Australia", "AUD", 0.33},
	{"AT", "Austria", "EUR", 0.3},
	{"BE", "Belgium", "EUR", 0.35},
	{"BR", "Brazil", "BRL", 0.85},
	{"CN", "China", "CNY", 0.55},
	{"CZ", "Czechia", "CZK", 6.5},
	{"DK", "Denmark", "DKK", 2.6},
	{"FI", "Finland", "EUR", 0.2},
	{"FR", "France", "EUR", 0.25},
	{"DE", "Germany", "EUR", 0.39},
	{"HK", "Hong Kong", "HKD", 1.5},
	{"IN", "India", "INR", 8},
	{"IE", "Ireland", "EUR", 0.37},
	{"IL", "Israel", "ILS", 0.65},
	{"IT", "Italy", "EUR", 0.3},
	{"JP", "Japan", "JPY", 31},
	{"MX", "Mexico", "MXN", 1.5},
	{"NL", "Netherlands", "EUR", 0.32},
	{"NZ", "New Zealand", "NZD", 0.35},
	{"NO", "Norway", "NOK", 1.4},
	{"PH", "Philippines", "PHP", 12},
	{"PL", "Poland", "PLN", 1},
	{"PT", "Portugal", "EUR", 0.22},
	{"SG", "Singapore", "SGD", 0.3},
	{"ZA", "South Africa", "ZAR", 3.5},
	{"KR", "South Korea", "KRW", 150},
	{"ES", "Spain", "EUR", 0.24},
	{"SE", "Sweden", "SEK", 2},
	{"CH", "Switzerland", "CHF", 0.32},
	{"TW", "Taiwan", "TWD", 3},
	{"AE", "United Arab Emirates", "AED", 0.3},
	{"GB", "United Kingdom", "GBP", 0.25},
}

// tzRegions maps a time zone to its most likely region. Zones shared by many US states fall
// back to the US average.
var tzRegions = map[string]string{
	"America/Los_Angeles": "US-CA",
	"America/Denver":      "US-CO",
	"America/Phoenix":     "US-AZ",
	"America/Boise":       "US-ID",
	"America/Detroit":     "US-MI",
	"America/Anchorage":   "US-AK",
	"America/Juneau":      "US-AK",
	"America/Nome":        "US-AK",
	"America/Sitka":       "US-AK",
	"Pacific/Honolulu":    "US-HI",
	"America/Toronto":     "CA-ON",
	"America/Vancouver":   "CA-BC",
	"America/Edmonton":    "CA-AB",
	"America/Winnipeg":    "CA-MB",
	"America/Regina":      "CA-SK",
	"America/Halifax":     "CA-NS",
	"America/Moncton":     "CA-NB",
	"America/St_Johns":    "CA-NL",
	"America/Montreal":    "CA-QC",
	"Europe/London":       "GB",
	"Europe/Dublin":       "IE",
	"Europe/Berlin":       "DE",
	"Europe/Paris":        "FR",
	"Europe/Madrid":       "ES",
	"Europe/Rome":         "IT",
	"Europe/Amsterdam":    "NL",
	"Europe/Brussels":     "BE",
	"Europe/Vienna":       "AT",
	"Europe/Zurich":       "CH",
	"Europe/Lisbon":       "PT",
	"Europe/Warsaw":       "PL",
	"Europe/Prague":       "CZ",
	"Europe/Stockholm":    "SE",
	"Europe/Oslo":         "NO",
	"Europe/Copenhagen":   "DK",
	"Europe/Helsinki":     "FI",
	"Pacific/Auckland":    "NZ",
	"Asia/Tokyo":          "JP",
	"Asia/Seoul":          "KR",
	"Asia/Singapore":      "SG",
	"Asia/Kolkata":        "IN",
	"Asia/Calcutta":       "IN",
	"Asia/Shanghai":       "CN",
	"Asia/Hong_Kong":      "HK",
	"Asia/Taipei":         "TW",
	"America/Sao_Paulo":   "BR",
	"America/Mexico_City": "MX",
	"Africa/Johannesburg": "ZA",
	"Asia/Dubai":          "AE",
	"Asia/Jerusalem":      "IL",
	"Asia/Manila":         "PH",
}

// tzPrefixes catch whole families of zones.
var tzPrefixes = [][2]string{
	{"America/Indiana/", "US-IN"}, {"America/Kentucky/", "US-KY"},
	{"America/North_Dakota/", "US-ND"}, {"Australia/", "AU"},
}

// LookupRegion returns the region with code, or false.
func LookupRegion(code string) (Region, bool) {
	for _, r := range Regions {
		if r.Code == code {
			return r, true
		}
	}
	return Region{}, false
}

// GuessRegion picks the most likely region code from an IANA time zone ("Europe/Berlin")
// and a locale ("en_GB.UTF-8" or "en-GB"), falling back to "US" as upstream does.
func GuessRegion(tz, locale string) string {
	if code, ok := tzRegions[tz]; ok {
		return code
	}
	for _, p := range tzPrefixes {
		if strings.HasPrefix(tz, p[0]) {
			return p[1]
		}
	}
	if country := localeCountry(locale); country != "" {
		if _, ok := LookupRegion(country); ok {
			return country
		}
	}
	return "US"
}

// DefaultRegion guesses this machine's region from its time zone and locale.
func DefaultRegion() Region {
	r, _ := LookupRegion(GuessRegion(localTimeZone(), localLocale()))
	return r
}

// DefaultSettings is the price used until the user picks one: the guessed region's rate.
func DefaultSettings() Settings {
	r := DefaultRegion()
	return Settings{Region: r.Code, Rate: r.Rate, Currency: r.Currency}
}

// localeCountry is the upper-cased second part of a locale: "en_GB.UTF-8" → "GB".
// Upstream reads navigator.language ("en-GB").
func localeCountry(locale string) string {
	locale, _, _ = strings.Cut(locale, ".")
	locale, _, _ = strings.Cut(locale, "@")
	parts := strings.FieldsFunc(locale, func(r rune) bool { return r == '_' || r == '-' })
	if len(parts) < 2 {
		return ""
	}
	return strings.ToUpper(parts[1])
}

// localTimeZone is this machine's IANA zone name, or "" when it can't be told (Windows,
// whose zone names aren't IANA). TZ wins, then the /etc/localtime symlink on Linux and macOS.
func localTimeZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if i := strings.Index(tz, "zoneinfo/"); i >= 0 {
			tz = tz[i+len("zoneinfo/"):]
		}
		return tz
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			return link[i+len("zoneinfo/"):]
		}
	}
	return ""
}

// localLocale is the user's locale from the usual POSIX variables.
func localLocale() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" && v != "C" && v != "POSIX" {
			return v
		}
	}
	return ""
}
