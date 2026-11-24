package state

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

const (
	forecastLongSeriesChampion        = "champion"
	forecastLongSeriesPlanning        = "planning"
	forecastLongSeriesLegacy          = "legacy_shadow"
	forecastLongSeriesEnergyplan      = "energyplan"
	forecastLongQuarterMS             = int64(15 * 60 * 1000)
	forecastLongMaxVector             = forecasting.MaxPoints
	forecastLongBandCold         byte = 0
	forecastLongBandEmpirical    byte = 1
)

func forecastLongKeepSeries(name string) bool {
	switch name {
	case forecastLongSeriesChampion, forecastLongSeriesPlanning, forecastLongSeriesLegacy, forecastLongSeriesEnergyplan:
		return true
	default:
		return false
	}
}

// encodeF32 writes little-endian float32 values. NaN is kept for an unknown
// weather or band value. Infinity and magnitudes float32 cannot hold are
// rejected; contract points are already finite.
func encodeF32(vals []float64) ([]byte, error) {
	if len(vals) > forecastLongMaxVector {
		return nil, errors.New("forecast archive vector is too long")
	}
	out := make([]byte, 4*len(vals))
	for i, v := range vals {
		if math.IsNaN(v) {
			binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(float32(math.NaN())))
			continue
		}
		if math.IsInf(v, 0) || v > math.MaxFloat32 || v < -math.MaxFloat32 {
			return nil, fmt.Errorf("forecast archive value %v is outside float32", v)
		}
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(float32(v)))
	}
	return out, nil
}

func decodeF32(b []byte, n int) ([]float64, error) {
	if n < 0 || n > forecastLongMaxVector || len(b) != 4*n {
		return nil, errors.New("forecast archive float32 length mismatch")
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:])))
	}
	return out, nil
}

func encodeU8(vals []byte) ([]byte, error) {
	if len(vals) > forecastLongMaxVector {
		return nil, errors.New("forecast archive vector is too long")
	}
	return append([]byte(nil), vals...), nil
}

func decodeU8(b []byte, n int) ([]byte, error) {
	if n < 0 || n > forecastLongMaxVector || len(b) != n {
		return nil, errors.New("forecast archive byte vector length mismatch")
	}
	return append([]byte(nil), b...), nil
}

// seriesLayout is the time grid of one stored series. A regular grid leaves
// starts empty. An irregular grid stores little-endian int32 offsets, in
// seconds, from startMS. When durations differ, each point is an offset and
// a duration (2n values) so gaps still round-trip.
type seriesLayout struct {
	startMS           int64
	slotMS            int64
	n                 int
	starts            []byte
	predictionStartMS int64
}

func layoutSeries(points []forecasting.Point) (seriesLayout, error) {
	if len(points) == 0 || len(points) > forecastLongMaxVector {
		return seriesLayout{}, errors.New("forecast archive series length is invalid")
	}
	for i, p := range points {
		if i > 0 && p.PredictionStartMS != 0 {
			return seriesLayout{}, errors.New("forecast archive partial point is only stored on the first interval")
		}
	}
	start := points[0].StartMS
	slot := points[0].EndMS - points[0].StartMS
	regular := true
	sameSlot := true
	for i, p := range points {
		d := p.EndMS - p.StartMS
		if d <= 0 || d > int64(60*60*1000) {
			return seriesLayout{}, errors.New("forecast archive interval is invalid")
		}
		if d != slot {
			sameSlot = false
			regular = false
		}
		if p.StartMS != start+int64(i)*slot {
			regular = false
		}
		if (p.StartMS-start)%1000 != 0 || d%1000 != 0 {
			return seriesLayout{}, errors.New("forecast archive interval is not a whole number of seconds")
		}
	}
	out := seriesLayout{startMS: start, n: len(points), predictionStartMS: points[0].PredictionStartMS}
	if regular {
		out.slotMS = slot
		return out, nil
	}
	if sameSlot {
		out.slotMS = slot
		out.starts = make([]byte, 4*len(points))
		for i, p := range points {
			off := (p.StartMS - start) / 1000
			if off > math.MaxInt32 || off < math.MinInt32 {
				return seriesLayout{}, errors.New("forecast archive time offset overflows")
			}
			binary.LittleEndian.PutUint32(out.starts[i*4:], uint32(int32(off)))
		}
		return out, nil
	}
	out.starts = make([]byte, 8*len(points))
	for i, p := range points {
		off := (p.StartMS - start) / 1000
		dur := (p.EndMS - p.StartMS) / 1000
		if off > math.MaxInt32 || off < math.MinInt32 || dur > math.MaxInt32 || dur <= 0 {
			return seriesLayout{}, errors.New("forecast archive time offset overflows")
		}
		binary.LittleEndian.PutUint32(out.starts[i*8:], uint32(int32(off)))
		binary.LittleEndian.PutUint32(out.starts[i*8+4:], uint32(int32(dur)))
	}
	return out, nil
}

func expandLayout(l seriesLayout) (starts, ends []int64, err error) {
	if l.n <= 0 || l.startMS <= 0 {
		return nil, nil, errors.New("forecast archive series grid is empty")
	}
	starts = make([]int64, l.n)
	ends = make([]int64, l.n)
	switch {
	case len(l.starts) == 0:
		if l.slotMS <= 0 {
			return nil, nil, errors.New("forecast archive regular grid has no slot")
		}
		for i := 0; i < l.n; i++ {
			starts[i] = l.startMS + int64(i)*l.slotMS
			ends[i] = starts[i] + l.slotMS
		}
	case len(l.starts) == 4*l.n:
		if l.slotMS <= 0 {
			return nil, nil, errors.New("forecast archive irregular grid has no slot")
		}
		for i := 0; i < l.n; i++ {
			off := int32(binary.LittleEndian.Uint32(l.starts[i*4:]))
			starts[i] = l.startMS + int64(off)*1000
			ends[i] = starts[i] + l.slotMS
		}
	case len(l.starts) == 8*l.n:
		for i := 0; i < l.n; i++ {
			off := int32(binary.LittleEndian.Uint32(l.starts[i*8:]))
			dur := int32(binary.LittleEndian.Uint32(l.starts[i*8+4:]))
			if dur <= 0 {
				return nil, nil, errors.New("forecast archive irregular duration is empty")
			}
			starts[i] = l.startMS + int64(off)*1000
			ends[i] = starts[i] + int64(dur)*1000
		}
	default:
		return nil, nil, errors.New("forecast archive time grid length mismatch")
	}
	return starts, ends, nil
}

func packBandMethod(p forecasting.Point) byte {
	code := func(method string) byte {
		if method == forecasting.BandMethodEmpirical {
			return forecastLongBandEmpirical
		}
		return forecastLongBandCold
	}
	return code(p.PVBand.Method) | code(p.LoadBand.Method)<<2 | code(p.NetBand.Method)<<4
}

func unpackBandMethod(v byte) (pv, load, net string) {
	name := func(code byte) string {
		if code&0x3 == forecastLongBandEmpirical {
			return forecasting.BandMethodEmpirical
		}
		return forecasting.BandMethodColdStart
	}
	return name(v), name(v >> 2), name(v >> 4)
}

func encodeBandStats(points []forecasting.Point) ([]byte, error) {
	out := make([]byte, len(points)*6*4)
	put := func(i, field, v int) error {
		if v < 0 || v > math.MaxUint32 {
			return errors.New("forecast archive band statistic overflows")
		}
		binary.LittleEndian.PutUint32(out[(i*6+field)*4:], uint32(v))
		return nil
	}
	for i, p := range points {
		stats := []int{p.PVBand.Samples, p.PVBand.Days, p.LoadBand.Samples, p.LoadBand.Days, p.NetBand.Samples, p.NetBand.Days}
		for field, v := range stats {
			if err := put(i, field, v); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func decodeBandStats(b []byte, n int) (samples, days [][3]int, err error) {
	if len(b) != n*6*4 {
		return nil, nil, errors.New("forecast archive band statistic length mismatch")
	}
	samples = make([][3]int, n)
	days = make([][3]int, n)
	for i := 0; i < n; i++ {
		for field := 0; field < 3; field++ {
			samples[i][field] = int(binary.LittleEndian.Uint32(b[(i*6+field*2)*4:]))
			days[i][field] = int(binary.LittleEndian.Uint32(b[(i*6+field*2+1)*4:]))
		}
	}
	return samples, days, nil
}

// encodeForecastStarts packs the weather slot starts one run used from one
// fetch as uvarints: the first start, then each gap to the next. An hourly
// day of 48 slots takes about 200 bytes.
func encodeForecastStarts(starts []int64) ([]byte, error) {
	if len(starts) == 0 || len(starts) > forecastLongMaxVector {
		return nil, errors.New("forecast archive weather slot list length is invalid")
	}
	sorted := append([]int64(nil), starts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if sorted[0] <= 0 {
		return nil, errors.New("forecast archive weather slot start is invalid")
	}
	out := binary.AppendUvarint(nil, uint64(sorted[0]))
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return nil, errors.New("forecast archive weather slot repeats")
		}
		out = binary.AppendUvarint(out, uint64(sorted[i]-sorted[i-1]))
	}
	return out, nil
}

func decodeForecastStarts(b []byte) ([]int64, error) {
	var out []int64
	var last uint64
	for len(b) > 0 {
		v, n := binary.Uvarint(b)
		if n <= 0 || v == 0 || v > math.MaxInt64-last || len(out) >= forecastLongMaxVector {
			return nil, errors.New("forecast archive weather slot list is invalid")
		}
		last += v
		out = append(out, int64(last))
		b = b[n:]
	}
	if len(out) == 0 {
		return nil, errors.New("forecast archive weather slot list is empty")
	}
	return out, nil
}

// encodeOccupancy stores a bit per quarter when the rows are contiguous, and
// the available-at time of each quarter so a restored issue still validates.
// Gaps use an explicit record. An empty slice returns a nil blob.
func encodeOccupancy(rows []forecasting.Occupancy) (blob []byte, startMS int64, n int, err error) {
	if len(rows) == 0 {
		return nil, 0, 0, nil
	}
	if len(rows) > 512 {
		return nil, 0, 0, errors.New("forecast archive occupancy is too long")
	}
	n = len(rows)
	startMS = rows[0].StartMS
	contiguous := true
	for i, row := range rows {
		if row.EndMS-row.StartMS != forecastLongQuarterMS {
			contiguous = false
			break
		}
		if i > 0 && row.StartMS != rows[i-1].EndMS {
			contiguous = false
			break
		}
	}
	if contiguous {
		bits := (n + 7) / 8
		blob = make([]byte, 1+bits+n*8)
		blob[0] = 0
		for i, row := range rows {
			if row.Home {
				blob[1+i/8] |= 1 << (uint(i) % 8)
			}
			binary.LittleEndian.PutUint64(blob[1+bits+i*8:], uint64(row.AvailableAtMS))
		}
		return blob, startMS, n, nil
	}
	blob = make([]byte, 1+n*25)
	blob[0] = 1
	off := 1
	for _, row := range rows {
		binary.LittleEndian.PutUint64(blob[off:], uint64(row.StartMS))
		binary.LittleEndian.PutUint64(blob[off+8:], uint64(row.EndMS))
		binary.LittleEndian.PutUint64(blob[off+16:], uint64(row.AvailableAtMS))
		if row.Home {
			blob[off+24] = 1
		}
		off += 25
	}
	return blob, startMS, n, nil
}

func decodeOccupancy(blob []byte, startMS int64, n int) ([]forecasting.Occupancy, error) {
	if n == 0 {
		return nil, nil
	}
	if n < 0 || n > 512 || len(blob) == 0 {
		return nil, errors.New("forecast archive occupancy is invalid")
	}
	switch blob[0] {
	case 0:
		bits := (n + 7) / 8
		if len(blob) != 1+bits+n*8 || startMS <= 0 {
			return nil, errors.New("forecast archive occupancy bitset length mismatch")
		}
		out := make([]forecasting.Occupancy, n)
		for i := 0; i < n; i++ {
			out[i] = forecasting.Occupancy{
				StartMS:       startMS + int64(i)*forecastLongQuarterMS,
				EndMS:         startMS + int64(i+1)*forecastLongQuarterMS,
				AvailableAtMS: int64(binary.LittleEndian.Uint64(blob[1+bits+i*8:])),
				Home:          blob[1+i/8]&(1<<(uint(i)%8)) != 0,
			}
		}
		return out, nil
	case 1:
		if len(blob) != 1+n*25 {
			return nil, errors.New("forecast archive occupancy record length mismatch")
		}
		out := make([]forecasting.Occupancy, n)
		off := 1
		for i := 0; i < n; i++ {
			out[i] = forecasting.Occupancy{
				StartMS:       int64(binary.LittleEndian.Uint64(blob[off:])),
				EndMS:         int64(binary.LittleEndian.Uint64(blob[off+8:])),
				AvailableAtMS: int64(binary.LittleEndian.Uint64(blob[off+16:])),
				Home:          blob[off+24] != 0,
			}
			off += 25
		}
		return out, nil
	default:
		return nil, errors.New("forecast archive occupancy version is unknown")
	}
}

// forecastLongInputHash is the SHA-256 of the frozen site, occupancy, weather
// and model references. Series watts are stored beside it and compared on
// their own, so a changed forecast with the same inputs is still rejected.
func forecastLongInputHash(issue forecasting.Issue) (string, error) {
	models := append([]forecasting.ModelState(nil), issue.Models...)
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	for i := range models {
		models[i].State = nil
	}
	weather := append([]forecasting.Weather(nil), issue.Weather...)
	sort.Slice(weather, func(i, j int) bool {
		if weather[i].AvailableAtMS != weather[j].AvailableAtMS {
			return weather[i].AvailableAtMS < weather[j].AvailableAtMS
		}
		if weather[i].Source != weather[j].Source {
			return weather[i].Source < weather[j].Source
		}
		return weather[i].StartMS < weather[j].StartMS
	})
	occupancy := append([]forecasting.Occupancy(nil), issue.Occupancy...)
	sort.Slice(occupancy, func(i, j int) bool { return occupancy[i].StartMS < occupancy[j].StartMS })
	payload, err := json.Marshal(struct {
		Site      *forecasting.SiteContext `json:"site,omitempty"`
		Occupancy []forecasting.Occupancy  `json:"occupancy,omitempty"`
		Weather   []forecasting.Weather    `json:"weather,omitempty"`
		Models    []forecasting.ModelState `json:"models,omitempty"`
	}{issue.Site, occupancy, weather, models})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
