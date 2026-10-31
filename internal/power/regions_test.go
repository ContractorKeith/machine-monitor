package power

import (
	"reflect"
	"testing"
)

func TestRegionsMatchUpstream(t *testing.T) {
	_, e := load(t)
	if !reflect.DeepEqual(Regions, e.PriceRegions) {
		t.Fatalf("Regions differ from upstream PRICE_REGIONS (%d vs %d entries)", len(Regions), len(e.PriceRegions))
	}
	if !reflect.DeepEqual(tzRegions, e.TZRegions) {
		t.Fatal("tzRegions differ from upstream TZ_REGIONS")
	}
	for tz, code := range tzRegions {
		if _, ok := LookupRegion(code); !ok {
			t.Errorf("%s maps to unknown region %s", tz, code)
		}
	}
	for _, p := range tzPrefixes {
		if _, ok := LookupRegion(p[1]); !ok {
			t.Errorf("prefix %s maps to unknown region %s", p[0], p[1])
		}
	}
}

func TestGuessRegion(t *testing.T) {
	for _, c := range []struct{ tz, locale, want string }{
		{"Europe/Berlin", "en_US.UTF-8", "DE"}, // zone beats locale
		{"America/Los_Angeles", "", "US-CA"},
		{"Asia/Calcutta", "", "IN"},           // legacy alias
		{"America/Indiana/Knox", "", "US-IN"}, // prefix
		{"Australia/Perth", "", "AU"},
		{"America/New_York", "en_US.UTF-8", "US"}, // shared zone, US average
		{"America/Chicago", "es_MX.UTF-8", "MX"},  // unknown zone, locale decides
		{"", "en-GB", "GB"},                       // navigator.language style
		{"", "de_DE@euro", "DE"},
		{"", "en_ZZ.UTF-8", "US"}, // unknown country
		{"", "C", "US"},
		{"", "", "US"},
	} {
		if got := GuessRegion(c.tz, c.locale); got != c.want {
			t.Errorf("GuessRegion(%q, %q) = %q, want %q", c.tz, c.locale, got, c.want)
		}
	}
}

func TestDefaultRegionFromTZ(t *testing.T) {
	t.Setenv("TZ", "Europe/Paris")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANG", "en_US.UTF-8")
	if r := DefaultRegion(); r.Code != "FR" || r.Currency != "EUR" || r.Rate != 0.25 {
		t.Fatalf("DefaultRegion = %+v, want FR", r)
	}
	t.Setenv("TZ", ":/usr/share/zoneinfo/Asia/Tokyo")
	if s := DefaultSettings(); s.Region != "JP" || s.Currency != "JPY" || s.Rate != 31 || s.TOU != nil {
		t.Fatalf("DefaultSettings = %+v, want JP", s)
	}
}
