package strategy

import (
	"bucks/internal/data"
	"bucks/internal/orders"
)

// This file implements the three v1 baseline strategies (build spec §4.7):
// Momentum (MA-crossover + Donchian breakout confirm), MeanReversion (Bollinger +
// RSI + z-score, range-gated), and Breakout (Donchian channel breakout). Each is
// structured indicators → entry → exit (the Freqtrade discipline — pattern, not
// code) and each entry carries a 2*ATR protective stop on the correct side. They
// are stateful: indicator windows persist across OnBar calls. They make NO money
// decisions — sizing/cash/PnL is the backtest engine's job; a strategy only emits
// an Action plus the protective Stop price.

// atrStopMult is the baseline protective-stop multiple: 2*ATR (build spec §4.7).
const atrStopMult = 2.0

// Momentum is a trend strategy: a fast/slow SMA crossover, CONFIRMED by a Donchian
// breakout in the same direction, with a 2*ATR stop. It enters long when the fast
// SMA crosses ABOVE the slow SMA and the close breaks the prior Donchian upper
// band; it exits the long when the fast SMA crosses back below the slow SMA. It
// holds at most one position at a time (the engine enforces single-position; the
// strategy simply tracks whether it is currently long to gate exits).
type Momentum struct {
	fast      *SMA
	slow      *SMA
	donchian  *Donchian
	atr       *ATR
	prevFast  float64
	prevSlow  float64
	havePrev  bool
	long      bool // currently in a long position (per this strategy's view)
	donPeriod int
}

// NewMomentum builds a Momentum strategy with the given fast/slow SMA periods, a
// Donchian breakout-confirm period, and an ATR period for the stop.
func NewMomentum(fast, slow, donchian, atr int) *Momentum {
	return &Momentum{
		fast:      NewSMA(fast),
		slow:      NewSMA(slow),
		donchian:  NewDonchian(donchian),
		atr:       NewATR(atr),
		donPeriod: donchian,
	}
}

// Name is the stable strategy identifier (used in the trade log + fingerprint).
func (m *Momentum) Name() string { return "momentum" }

// OnBar processes one bar and returns the signal.
func (m *Momentum) OnBar(bar data.Bar) Signal {
	_, high, low, close := barFloats(bar)
	fast, fastReady := m.fast.Update(close)
	slow, slowReady := m.slow.Update(close)
	upper, _, donReady := m.donchian.Update(high, low)
	atr, atrReady := m.atr.Update(high, low, close)

	defer func() {
		m.prevFast, m.prevSlow, m.havePrev = fast, slow, true
	}()

	if !fastReady || !slowReady {
		return hold()
	}

	// Exit first: if long and the fast SMA crosses back below the slow SMA, exit.
	if m.long {
		if m.havePrev && m.prevFast >= m.prevSlow && fast < slow {
			m.long = false
			return Signal{Action: Exit, Reason: "momentum: fast SMA crossed below slow SMA"}
		}
		return hold()
	}

	// Entry: a fresh bullish crossover (prev fast <= slow, now fast > slow)
	// CONFIRMED by a Donchian upper-band breakout, with a valid 2*ATR stop.
	if m.havePrev && m.prevFast <= m.prevSlow && fast > slow && donReady && atrReady && upper > 0 {
		if close > upper {
			stop, ok := stopFromATR(bar.Close, atr, atrStopMult, true)
			if ok {
				m.long = true
				return Signal{
					Action: EnterLong,
					Stop:   stop,
					Reason: "momentum: fast>slow crossover confirmed by Donchian breakout",
				}
			}
		}
	}
	return hold()
}

// MeanReversion fades extremes IN A RANGE: it enters when the z-score of the close
// is beyond ±2 AND price is outside the Bollinger(20,2) band AND RSI confirms the
// extreme, but ONLY in a range regime (low ATR relative to price — not a strong
// trend). Long when oversold (z <= -2), short when overbought (z >= +2). It exits
// on mean reversion (z crosses back through 0). 2*ATR stop on the correct side.
type MeanReversion struct {
	boll     *Bollinger
	rsi      *RSI
	zscore   *ZScore
	atr      *ATR
	zEnter   float64 // z magnitude to enter (2.0)
	rsiLow   float64 // RSI oversold threshold (e.g. 30)
	rsiHigh  float64 // RSI overbought threshold (e.g. 70)
	rangeMax float64 // max ATR/price ratio to count as a range regime
	pos      int     // +1 long, -1 short, 0 flat (this strategy's view)
}

// NewMeanReversion builds the mean-reversion strategy with a Bollinger/z period
// and an ATR period. Thresholds are the documented baseline (z=±2, RSI 30/70,
// range regime = ATR/price <= 5%).
func NewMeanReversion(period, atr int) *MeanReversion {
	return &MeanReversion{
		boll:     NewBollinger(period, 2.0),
		rsi:      NewRSI(period),
		zscore:   NewZScore(period),
		atr:      NewATR(atr),
		zEnter:   2.0,
		rsiLow:   30,
		rsiHigh:  70,
		rangeMax: 0.05,
	}
}

// Name is the stable strategy identifier.
func (mr *MeanReversion) Name() string { return "mean_reversion" }

// OnBar processes one bar and returns the signal.
func (mr *MeanReversion) OnBar(bar data.Bar) Signal {
	_, high, low, close := barFloats(bar)
	lower, _, upper, bollReady := mr.boll.Update(close)
	rsi, rsiReady := mr.rsi.Update(close)
	z, zReady := mr.zscore.Update(close)
	atr, atrReady := mr.atr.Update(high, low, close)

	if !bollReady || !rsiReady || !zReady || !atrReady {
		return hold()
	}

	// Exit: mean reversion back through 0 closes the position.
	if mr.pos > 0 && z >= 0 {
		mr.pos = 0
		return Signal{Action: Exit, Reason: "mean_reversion: z reverted to mean (long)"}
	}
	if mr.pos < 0 && z <= 0 {
		mr.pos = 0
		return Signal{Action: Exit, Reason: "mean_reversion: z reverted to mean (short)"}
	}
	if mr.pos != 0 {
		return hold()
	}

	// Range-regime gate: only fade extremes when volatility is contained (not a
	// runaway trend). ATR/price above rangeMax means "trending" — stand down.
	if close <= 0 {
		return hold()
	}
	if atr/close > mr.rangeMax {
		return hold()
	}

	// Long the oversold extreme: z beyond -2, price below the lower band, RSI low.
	if z <= -mr.zEnter && close < lower && rsi <= mr.rsiLow {
		stop, ok := stopFromATR(bar.Close, atr, atrStopMult, true)
		if ok {
			mr.pos = 1
			return Signal{
				Action: EnterLong,
				Stop:   stop,
				Reason: "mean_reversion: oversold z<=-2 below lower band, RSI confirms",
			}
		}
	}
	// Short the overbought extreme: z beyond +2, price above the upper band, RSI high.
	if z >= mr.zEnter && close > upper && rsi >= mr.rsiHigh {
		stop, ok := stopFromATR(bar.Close, atr, atrStopMult, false)
		if ok {
			mr.pos = -1
			return Signal{
				Action: EnterShort,
				Stop:   stop,
				Reason: "mean_reversion: overbought z>=2 above upper band, RSI confirms",
			}
		}
	}
	return hold()
}

// Breakout is a pure Donchian channel breakout: it enters long when the close
// breaks above the prior N-period upper band (the breakout) and exits when the
// close falls back below the prior N-period lower band. 2*ATR stop. The spec says
// "enter next bar" — the engine fills entries at the NEXT bar's open (see backtest
// engine), so the strategy emits EnterLong on the breakout bar and the engine
// applies the next-bar fill; the strategy itself stays causal (it only ever reads
// the PRIOR window via the Donchian exclusion).
type Breakout struct {
	donchian *Donchian
	atr      *ATR
	long     bool
}

// NewBreakout builds a Donchian breakout strategy with a channel period and an
// ATR period for the stop.
func NewBreakout(period, atr int) *Breakout {
	return &Breakout{
		donchian: NewDonchian(period),
		atr:      NewATR(atr),
	}
}

// Name is the stable strategy identifier.
func (b *Breakout) Name() string { return "breakout" }

// OnBar processes one bar and returns the signal.
func (b *Breakout) OnBar(bar data.Bar) Signal {
	_, high, low, close := barFloats(bar)
	upper, lower, donReady := b.donchian.Update(high, low)
	atr, atrReady := b.atr.Update(high, low, close)

	if !donReady {
		return hold()
	}

	if b.long {
		// Exit when price breaks back below the prior lower channel.
		if close < lower {
			b.long = false
			return Signal{Action: Exit, Reason: "breakout: close fell below Donchian lower band"}
		}
		return hold()
	}

	// Entry: close breaks ABOVE the prior upper band, with a valid 2*ATR stop.
	if atrReady && upper > 0 && close > upper {
		stop, ok := stopFromATR(bar.Close, atr, atrStopMult, true)
		if ok {
			b.long = true
			return Signal{
				Action: EnterLong,
				Stop:   stop,
				Reason: "breakout: close broke above Donchian upper band",
			}
		}
	}
	return hold()
}

// Compile-time assertions that all three implement Strategy.
var (
	_ Strategy = (*Momentum)(nil)
	_ Strategy = (*MeanReversion)(nil)
	_ Strategy = (*Breakout)(nil)
)

// ensure the orders import is used even if a build trims a path (Stop is Decimal).
var _ = orders.ZeroDecimal
