// Package export writes a device's metric history as CSV or JSON, from this machine's
// history directory or from a hub's GET /api/history/:id (machine-monitor export).
//
// Rows are the stored 1-minute averages and peaks, oldest first. Times are RFC 3339; a
// metric nobody measured is an empty CSV cell or a JSON null, never 0.
package export

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/history"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// Source reads one device's history between two times.
type Source interface {
	// Points returns id's points with from <= T <= to, oldest first. It returns an
	// *UnknownDeviceError when the source has never heard of id.
	Points(ctx context.Context, id string, from, to time.Time) ([]history.Point, error)
}

// UnknownDeviceError means the source has no history at all for the device.
type UnknownDeviceError struct {
	ID    string
	Where string // e.g. "on this machine", "on the hub at http://…"
}

func (e *UnknownDeviceError) Error() string {
	return fmt.Sprintf("There is no device %q %s.", e.ID, e.Where)
}

// Options is one export.
type Options struct {
	ID       string
	From, To time.Time
	Format   string         // "csv" or "json"
	Loc      *time.Location // zone for printed times; nil means UTC
}

// Run reads o.ID's history from src and writes it to w in o.Format. A range with no points
// is an error, so a script never mistakes an empty file for a quiet machine.
func Run(ctx context.Context, src Source, o Options, w io.Writer) error {
	if o.Format != "csv" && o.Format != "json" {
		return sentence("%q is not a format; use csv or json.", o.Format)
	}
	if !report.ValidID(o.ID) {
		return sentence("%q is not a device ID; IDs are up to 64 letters, digits, dots, colons, dashes and underscores.", o.ID)
	}
	if o.Loc == nil {
		o.Loc = time.UTC
	}
	pts, err := src.Points(ctx, o.ID, o.From, o.To)
	if err != nil {
		return err
	}
	if len(pts) == 0 {
		return sentence("Device %q has no history between %s and %s.", o.ID, o.From.In(o.Loc).Format(time.RFC3339), o.To.In(o.Loc).Format(time.RFC3339))
	}
	if o.Format == "csv" {
		return writeCSV(w, pts, o.Loc)
	}
	return writeJSON(w, o, pts)
}

// columns is the CSV header, in a fixed order that later versions only append to.
var columns = []string{
	"time",
	"cpu_pct", "gpu_pct", "mem_pct",
	"cpu_temp_c", "gpu_temp_c",
	"gpu_power_w", "power_w",
	"net_rx_bytes_per_s", "net_tx_bytes_per_s",
	"disk_read_bytes_per_s", "disk_write_bytes_per_s",
	"cpu_max_pct", "gpu_max_pct", "cpu_temp_max_c", "gpu_temp_max_c", "power_max_w",
	"samples",
}

// values returns p's metrics in columns order (after time).
func values(p *history.Point) []*float64 {
	return []*float64{
		p.CPU, p.GPU, p.Mem,
		p.CPUTemp, p.GPUTemp,
		p.GPUPower, p.Power,
		p.RX, p.TX,
		p.DR, p.DW,
		p.CPUMax, p.GPUMax, p.CPUTempMax, p.GPUTempMax, p.PowerMax,
	}
}

// formatTime renders a Unix-ms point time as RFC 3339, with milliseconds only when present.
func formatTime(ms float64, loc *time.Location) string {
	return time.UnixMilli(int64(ms)).In(loc).Format(time.RFC3339Nano)
}

func writeCSV(w io.Writer, pts []history.Point, loc *time.Location) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(columns); err != nil {
		return err
	}
	row := make([]string, len(columns))
	for i := range pts {
		p := &pts[i]
		row[0] = formatTime(p.T, loc)
		for j, v := range values(p) {
			row[j+1] = ""
			if v != nil {
				row[j+1] = strconv.FormatFloat(*v, 'f', -1, 64)
			}
		}
		row[len(row)-1] = ""
		if p.N > 0 { // 0: not recorded (raw samples, buckets, rows from before upstream counted)
			row[len(row)-1] = strconv.Itoa(p.N)
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// jsonPoint is one exported point: an RFC 3339 time, then the hub's own point fields
// (t in Unix ms, metrics with null for unknown, n when recorded).
type jsonPoint struct {
	Time string `json:"time"`
	history.Point
}

type jsonExport struct {
	Device string      `json:"device"`
	From   string      `json:"from"`
	To     string      `json:"to"`
	Points []jsonPoint `json:"points"`
}

func writeJSON(w io.Writer, o Options, pts []history.Point) error {
	out := jsonExport{
		Device: o.ID,
		From:   o.From.In(o.Loc).Format(time.RFC3339Nano),
		To:     o.To.In(o.Loc).Format(time.RFC3339Nano),
		Points: make([]jsonPoint, len(pts)),
	}
	for i, p := range pts {
		out.Points[i] = jsonPoint{Time: formatTime(p.T, o.Loc), Point: p}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// sentenceError is an error printed to the user as is: a full sentence, capitalized and
// punctuated, which Go's error-string convention (and staticcheck ST1005) would forbid.
type sentenceError string

func (e sentenceError) Error() string { return string(e) }

// sentence formats a user-facing error.
func sentence(format string, a ...any) error { return sentenceError(fmt.Sprintf(format, a...)) }
