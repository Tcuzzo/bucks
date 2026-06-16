// Package strategy holds BUCKS's baseline trading brains and the indicator math
// that feeds them. It is deliberately split MONEY-vs-SIGNAL:
//
//   - INDICATORS in this file (SMA, EMA, RSI, ATR, Bollinger, z-score, Donchian)
//     are SIGNAL math. They use float64 ON PURPOSE: they are not money, they never
//     touch the cash/position/PnL ledger, and float64 is bit-for-bit deterministic
//     run-to-run on the same binary (no map order, no wall clock, no RNG), which is
//     all the backtest determinism gate requires of them. Calling these "money"
//     would be wrong; float64 here is the right tool.
//   - MONEY lives in the backtest package's ledger and in Signal.Stop, which is an
//     orders.Decimal price. Entry/exit prices, cash, position qty, P&L, fees, and
//     slippage are ALL orders.Decimal — never float64. The only float64→Decimal
//     crossing is the protective-stop price, computed from an ATR (a float64
//     indicator) and then carried as an exact Decimal on the Signal.
//
// All indicators are INCREMENTAL where natural: each Update(value) pushes one new
// observation and returns the current reading plus a readiness flag (false until
// the lookback window has filled), so a strategy processes a bar in O(1)/O(window)
// without rescanning history. They are pure: identical inputs in identical order
// yield identical outputs, with no global state.
package strategy

import "math"

// SMA is an incremental simple moving average over a fixed window. Update pushes a
// value and returns (average, ready). ready is false until `period` values have
// been seen; the average is over the most recent `period` values thereafter.
type SMA struct {
	period int
	buf    []float64
	idx    int
	count  int
	sum    float64
}

// NewSMA builds an SMA over the given period (period must be >= 1).
func NewSMA(period int) *SMA {
	if period < 1 {
		period = 1
	}
	return &SMA{period: period, buf: make([]float64, period)}
}

// Update pushes v and returns the current average and whether the window is full.
func (s *SMA) Update(v float64) (float64, bool) {
	// Maintain a running sum over a ring buffer: subtract the value we overwrite,
	// add the new one. Deterministic and O(1).
	if s.count == s.period {
		s.sum -= s.buf[s.idx]
	}
	s.buf[s.idx] = v
	s.sum += v
	s.idx = (s.idx + 1) % s.period
	if s.count < s.period {
		s.count++
	}
	avg := s.sum / float64(s.count)
	return avg, s.count >= s.period
}

// Value returns the latest average and readiness without pushing a new value.
func (s *SMA) Value() (float64, bool) {
	if s.count == 0 {
		return 0, false
	}
	return s.sum / float64(s.count), s.count >= s.period
}

// EMA is an incremental exponential moving average. It seeds from the first
// `period` values' SMA, then applies the standard alpha = 2/(period+1) recurrence.
type EMA struct {
	period int
	alpha  float64
	seed   *SMA
	val    float64
	ready  bool
}

// NewEMA builds an EMA over the given period (period must be >= 1).
func NewEMA(period int) *EMA {
	if period < 1 {
		period = 1
	}
	return &EMA{
		period: period,
		alpha:  2.0 / (float64(period) + 1.0),
		seed:   NewSMA(period),
	}
}

// Update pushes v and returns the current EMA and whether it is ready.
func (e *EMA) Update(v float64) (float64, bool) {
	if !e.ready {
		avg, ok := e.seed.Update(v)
		if ok {
			e.val = avg
			e.ready = true
		}
		return e.val, e.ready
	}
	e.val = e.alpha*v + (1.0-e.alpha)*e.val
	return e.val, true
}

// RSI is the Wilder Relative Strength Index over a fixed period. It tracks Wilder-
// smoothed average gains/losses and reports a 0..100 oscillator.
type RSI struct {
	period   int
	prev     float64
	havePrev bool
	avgGain  float64
	avgLoss  float64
	seenDiff int
	ready    bool
}

// NewRSI builds an RSI over the given period (period must be >= 1).
func NewRSI(period int) *RSI {
	if period < 1 {
		period = 1
	}
	return &RSI{period: period}
}

// Update pushes a close price and returns (rsi, ready). ready is false until
// `period` price-to-price differences have been seen.
func (r *RSI) Update(close float64) (float64, bool) {
	if !r.havePrev {
		r.prev = close
		r.havePrev = true
		return 0, false
	}
	change := close - r.prev
	r.prev = close
	gain, loss := 0.0, 0.0
	if change > 0 {
		gain = change
	} else {
		loss = -change
	}

	if !r.ready {
		// Accumulate the first `period` diffs into a simple average seed.
		r.avgGain += gain
		r.avgLoss += loss
		r.seenDiff++
		if r.seenDiff < r.period {
			return 0, false
		}
		r.avgGain /= float64(r.period)
		r.avgLoss /= float64(r.period)
		r.ready = true
	} else {
		// Wilder smoothing.
		p := float64(r.period)
		r.avgGain = (r.avgGain*(p-1) + gain) / p
		r.avgLoss = (r.avgLoss*(p-1) + loss) / p
	}

	if r.avgLoss == 0 {
		return 100, true
	}
	rs := r.avgGain / r.avgLoss
	return 100 - (100 / (1 + rs)), true
}

// ATR is the Wilder Average True Range over a fixed period, fed full OHLC bars. It
// is the volatility input for protective stops (2*ATR). True range uses the prior
// close, so the first bar only establishes the close reference.
type ATR struct {
	period    int
	prevClose float64
	havePrev  bool
	atr       float64
	seenTR    int
	ready     bool
}

// NewATR builds an ATR over the given period (period must be >= 1).
func NewATR(period int) *ATR {
	if period < 1 {
		period = 1
	}
	return &ATR{period: period}
}

// Update pushes a bar's high/low/close and returns (atr, ready). ready is false
// until `period` true ranges have been accumulated.
func (a *ATR) Update(high, low, close float64) (float64, bool) {
	tr := high - low
	if a.havePrev {
		// True range = max(H-L, |H-prevClose|, |L-prevClose|).
		tr = math.Max(tr, math.Abs(high-a.prevClose))
		tr = math.Max(tr, math.Abs(low-a.prevClose))
	}
	a.prevClose = close
	a.havePrev = true

	if !a.ready {
		a.atr += tr
		a.seenTR++
		if a.seenTR < a.period {
			return 0, false
		}
		a.atr /= float64(a.period)
		a.ready = true
		return a.atr, true
	}
	p := float64(a.period)
	a.atr = (a.atr*(p-1) + tr) / p
	return a.atr, true
}

// Value returns the current ATR and readiness without pushing a bar.
func (a *ATR) Value() (float64, bool) { return a.atr, a.ready }

// rollingStats keeps a ring buffer of the last `period` values and reports their
// mean and population standard deviation. It is the shared engine behind Bollinger
// bands and the z-score (both are mean ± k*stddev / (x-mean)/stddev).
type rollingStats struct {
	period int
	buf    []float64
	idx    int
	count  int
}

func newRollingStats(period int) *rollingStats {
	if period < 1 {
		period = 1
	}
	return &rollingStats{period: period, buf: make([]float64, period)}
}

// push records v; ready reports whether the window is full.
func (r *rollingStats) push(v float64) (ready bool) {
	r.buf[r.idx] = v
	r.idx = (r.idx + 1) % r.period
	if r.count < r.period {
		r.count++
	}
	return r.count >= r.period
}

// meanStd returns the mean and population stddev over the current window. It sums
// the live window each call (period is small and fixed) so the result depends only
// on the window contents, not on accumulation order across the whole series — this
// keeps it numerically stable and order-deterministic.
//
// NOTE: the ring buffer is iterated by SLOT INDEX (buf[0..count-1]), NOT in
// chronological order — once the window has wrapped, buf[0] is whatever slot the
// ring last reused, not the oldest value. That is SAFE here ONLY because mean and
// population stddev are order-independent (sum and sum-of-squared-deviations don't
// care about element order). Any FUTURE consumer that needs time-ordering (e.g. a
// most-recent value, a weighted/EWMA stat, or a slope) must NOT assume
// buf[0..count-1] is chronological — reconstruct order via idx.
func (r *rollingStats) meanStd() (mean, std float64) {
	n := r.count
	if n == 0 {
		return 0, 0
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += r.buf[i]
	}
	mean = sum / float64(n)
	varSum := 0.0
	for i := 0; i < n; i++ {
		dv := r.buf[i] - mean
		varSum += dv * dv
	}
	std = math.Sqrt(varSum / float64(n))
	return mean, std
}

// Bollinger reports the (lower, mid, upper) bands: mid = SMA(period),
// upper/lower = mid ± k*stddev. k is the standard-deviation multiplier (2 for the
// classic Bollinger(20,2)).
type Bollinger struct {
	stats *rollingStats
	k     float64
}

// NewBollinger builds Bollinger bands with the given period and stddev multiplier.
func NewBollinger(period int, k float64) *Bollinger {
	return &Bollinger{stats: newRollingStats(period), k: k}
}

// Update pushes a value and returns (lower, mid, upper, ready).
func (b *Bollinger) Update(v float64) (lower, mid, upper float64, ready bool) {
	ready = b.stats.push(v)
	mean, std := b.stats.meanStd()
	off := b.k * std
	return mean - off, mean, mean + off, ready
}

// ZScore reports (value - mean) / stddev over a rolling window — how many standard
// deviations the latest value sits from its recent mean. It shares rollingStats
// with Bollinger (same window math). A zero stddev (flat window) yields z = 0.
type ZScore struct {
	stats *rollingStats
}

// NewZScore builds a z-score over the given window period.
func NewZScore(period int) *ZScore {
	return &ZScore{stats: newRollingStats(period)}
}

// Update pushes v and returns (z, ready).
func (z *ZScore) Update(v float64) (float64, bool) {
	ready := z.stats.push(v)
	mean, std := z.stats.meanStd()
	if std == 0 {
		return 0, ready
	}
	return (v - mean) / std, ready
}

// Donchian reports the highest high and lowest low over the last `period` bars,
// EXCLUDING the current bar — so "close > upper" is a genuine breakout above the
// PRIOR range, not a tautology against the bar's own high. Update returns the prior
// window's (upper, lower, ready) and THEN folds the current bar in for next time.
type Donchian struct {
	period int
	highs  []float64
	lows   []float64
	idx    int
	count  int
}

// NewDonchian builds a Donchian channel over the given period (period >= 1).
func NewDonchian(period int) *Donchian {
	if period < 1 {
		period = 1
	}
	return &Donchian{
		period: period,
		highs:  make([]float64, period),
		lows:   make([]float64, period),
	}
}

// Update returns the channel (upper=max high, lower=min low) over the PRIOR
// `period` bars (excluding this one) and whether that prior window was full, then
// records this bar's high/low for future windows. The exclusion is what makes a
// breakout test honest: the returned bounds never include the bar being evaluated.
func (dc *Donchian) Update(high, low float64) (upper, lower float64, ready bool) {
	ready = dc.count >= dc.period
	if ready {
		upper = dc.highs[0]
		lower = dc.lows[0]
		for i := 1; i < dc.count; i++ {
			upper = math.Max(upper, dc.highs[i])
			lower = math.Min(lower, dc.lows[i])
		}
	}
	// Record this bar for subsequent windows.
	dc.highs[dc.idx] = high
	dc.lows[dc.idx] = low
	dc.idx = (dc.idx + 1) % dc.period
	if dc.count < dc.period {
		dc.count++
	}
	return upper, lower, ready
}
