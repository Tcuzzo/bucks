// Package backtest is BUCKS's deterministic backtester: it runs a Strategy over a
// slice of historical Bars, modeling fees and slippage, and produces honest
// performance metrics. It is the §4.5 honesty layer in code — it never invents an
// edge, it surfaces overfitting red flags, and it is marketed on risk controls, not
// on a claimed return.
//
// MONEY DISCIPLINE (the central rule): EVERY money/position value in this engine —
// cash, position quantity, entry/exit price, P&L, fees, slippage — is
// orders.Decimal. There is NO float64 anywhere in the cash/PnL/fee path. (Indicator
// SIGNAL math lives in the strategy package and uses float64 by design; it never
// reaches this ledger.) The win-rate field is the single ratio rendered as a
// float64 for human display, computed from integer trade counts — it is not money
// and never feeds a money calculation.
//
// DETERMINISM: the engine reads no wall clock, uses no RNG, and iterates no Go
// maps in the simulation path. Same bars + same Config → byte-identical Result and
// trade log → identical Fingerprint(). The Fingerprint test proves run1 == run2.
package backtest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"bucks/internal/data"
	"bucks/internal/orders"
	"bucks/internal/strategy"
)

// Config parameterizes one backtest run. All money knobs are Decimal.
//
//   - StartingCash is the initial account cash.
//   - Qty is the fixed position size (shares/units) per entry. (Fixed-size keeps
//     this slice's gate about the engine + cost model, not about sizing; sizing is
//     a later slice.)
//   - CommissionPerTrade is a flat per-FILL commission (charged on entry AND on
//     exit — i.e. twice per round trip), in account currency.
//   - SlippageBps is slippage in basis points applied ADVERSELY to every fill: a
//     buy fills SlippageBps higher than the bar price, a sell fills SlippageBps
//     lower. 1 bp = 0.0001. Modeled in exact Decimal.
//   - EnterAtNextOpen, when true, fills an entry signaled on bar i at bar i+1's
//     OPEN (the realistic "decide on close, act next bar" model the breakout
//     strategy documents). When false, entries fill at the signal bar's close.
//     Exits follow the same convention. This is a determinism-preserving modeling
//     choice, not a money one.
type Config struct {
	StartingCash       orders.Decimal
	Qty                orders.Decimal
	CommissionPerTrade orders.Decimal
	SlippageBps        orders.Decimal
	EnterAtNextOpen    bool
}

// Trade is one completed round trip (entry → exit) in the trade log. All money is
// Decimal. Side is the position direction (long/short). The trade log is part of
// the fingerprint, so its fields are rendered deterministically.
type Trade struct {
	Symbol    string
	Side      orders.Side
	Qty       orders.Decimal
	EntryPx   orders.Decimal // fill price INCLUDING slippage
	ExitPx    orders.Decimal // fill price INCLUDING slippage
	GrossPnL  orders.Decimal // (exit-entry)*qty for long; (entry-exit)*qty for short
	Fees      orders.Decimal // total commission for the round trip (entry + exit)
	NetPnL    orders.Decimal // GrossPnL - Fees
	Reason    string         // exit reason
	EntryTS   data.Bar       // entry bar (for provenance/debug; not fingerprinted beyond TS)
	openIndex int
}

// Result is the honest performance report of a backtest. Money fields are Decimal;
// the only float64s are WinRate and ProfitFactor (display ratios).
//
// GROSS-vs-NET, on purpose:
//   - NetPnL / WinRate / Expectancy reflect AFTER-COST reality — what the trader
//     actually keeps once commission + slippage are paid. NumWins/NumLosses/WinRate
//     count a trade by its NET outcome (a gross winner that fees turn net-negative
//     is a LOSS).
//   - ProfitFactor measures the RAW SIGNAL EDGE before costs (GROSS per-trade P&L).
//     It is the overfit/quality detector: keeping it gross stops fees from dragging
//     a genuinely overfit ratio under the 4.0 threshold and masking it.
type Result struct {
	NetPnL       orders.Decimal // sum of trade NetPnL (after-cost reality)
	GrossPnL     orders.Decimal // sum of trade GrossPnL (pre-cost)
	FeesPaid     orders.Decimal // sum of trade Fees
	EndingCash   orders.Decimal // StartingCash + NetPnL (flat at end; any open pos is closed at last bar)
	NumTrades    int
	NumWins      int            // trades with NET P&L > 0 (after fees)
	NumLosses    int            // trades with NET P&L < 0 (after fees)
	WinRate      float64        // NumWins / NumTrades — NET (after-cost) display ratio; not money
	ProfitFactor float64        // sum gross-winning P&L / sum |gross-losing P&L| — GROSS (raw edge / overfit signal); +Inf when no gross losers
	Expectancy   orders.Decimal // NetPnL / NumTrades (avg net P&L per trade) — exact Decimal
	MaxDrawdown  orders.Decimal // largest peak-to-trough drop in the equity curve
	Trades       []Trade
	Warnings     []string // honesty red flags (overfit signals)
}

// Overfitting red-flag thresholds (build spec §4.5): a profit factor above 4 or a
// win rate above 80% are treated as LIKELY OVERFIT and surfaced as warnings, never
// celebrated. These are advisory honesty flags, not gates — the engine reports
// them so a human (and the Critic) sees them.
const (
	overfitProfitFactor = 4.0
	overfitWinRate      = 0.8
)

// Engine runs a strategy over bars under a config. It is constructed once and Run
// is pure with respect to (strategy state, bars, config): no shared mutable global
// state, no clock, no RNG.
type Engine struct {
	cfg Config
}

// NewEngine builds an Engine with the given config.
func NewEngine(cfg Config) *Engine { return &Engine{cfg: cfg} }

// position is the engine's open-position ledger entry — all Decimal money.
type position struct {
	open     bool
	side     orders.Side
	qty      orders.Decimal
	entryPx  orders.Decimal // includes entry slippage
	entryFee orders.Decimal
	openBar  int
	openTS   data.Bar
}

// Run executes strat over bars and returns the honest Result. The simulation is a
// single forward pass: for each bar it asks the strategy for a Signal, then applies
// fills against the engine's Decimal ledger under the fee + slippage model. Any
// position still open at the last bar is closed at that bar's close (no dangling
// position leaks into the metrics). Determinism: the loop reads only bars (in
// order) and config; it never iterates a map, reads a clock, or draws a random.
func (e *Engine) Run(strat strategy.Strategy, bars []data.Bar) (Result, error) {
	var (
		pos      position
		trades   []Trade
		grossSum = orders.ZeroDecimal
		feeSum   = orders.ZeroDecimal
		netSum   = orders.ZeroDecimal
		// Profit-factor accumulators are GROSS (pre-fee) per-trade P&L: this metric is
		// the RAW signal-edge / overfit detector, so fees must NOT be allowed to drag a
		// genuinely overfit ratio under the 4.0 threshold and mask it. A trade with
		// GROSS P&L > 0 adds its gross to grossProfitSum; gross < 0 adds |gross| to
		// grossLossSum. ProfitFactor = grossProfitSum / grossLossSum.
		grossProfitSum = orders.ZeroDecimal // sum of POSITIVE gross P&L (overfit signal)
		grossLossSum   = orders.ZeroDecimal // sum of |NEGATIVE gross P&L| (overfit signal)
		// wins/losses/WinRate stay NET (after fees) — what the trader actually keeps.
		wins   int
		losses int
		// Equity curve (running cash) for max-drawdown; starts at StartingCash.
		equity = e.cfg.StartingCash
		peak   = e.cfg.StartingCash
		maxDD  = orders.ZeroDecimal
	)

	// pendingEntry holds a signal awaiting a next-bar-open fill (EnterAtNextOpen).
	type pending struct {
		active bool
		sig    strategy.Signal
	}
	var pendEntry pending
	var pendExit pending

	closeTrade := func(barIdx int, fillBar data.Bar, exitRaw orders.Decimal, reason string) error {
		exitPx, err := applySlippage(exitRaw, exitSide(pos.side), e.cfg.SlippageBps)
		if err != nil {
			return err
		}
		exitFee := e.cfg.CommissionPerTrade
		gross, err := grossPnL(pos.side, pos.entryPx, exitPx, pos.qty)
		if err != nil {
			return err
		}
		fees, err := pos.entryFee.Add(exitFee)
		if err != nil {
			return err
		}
		net, err := gross.Sub(fees)
		if err != nil {
			return err
		}

		grossSum, err = grossSum.Add(gross)
		if err != nil {
			return err
		}
		feeSum, err = feeSum.Add(fees)
		if err != nil {
			return err
		}
		netSum, err = netSum.Add(net)
		if err != nil {
			return err
		}
		// Win/loss counts (and thus WinRate) are on NET P&L — the real after-fee
		// outcome the trader keeps. A gross winner that fees turn into a net loser
		// counts as a LOSS here, correctly.
		if net.Sign() > 0 {
			wins++
		} else if net.Sign() < 0 {
			losses++
		}
		// Profit-factor accumulators are on GROSS P&L — the raw signal edge before
		// costs. This is what the overfit detector must see so fees can't mask it.
		if gross.Sign() > 0 {
			grossProfitSum, err = grossProfitSum.Add(gross)
			if err != nil {
				return err
			}
		} else if gross.Sign() < 0 {
			grossLossSum, err = grossLossSum.Add(gross.Abs())
			if err != nil {
				return err
			}
		}

		// Update the equity curve + max drawdown in exact Decimal.
		equity, err = equity.Add(net)
		if err != nil {
			return err
		}
		if equity.Cmp(peak) > 0 {
			peak = equity
		} else {
			dd, err := peak.Sub(equity)
			if err != nil {
				return err
			}
			if dd.Cmp(maxDD) > 0 {
				maxDD = dd
			}
		}

		trades = append(trades, Trade{
			Symbol:    fillBar.Symbol,
			Side:      pos.side,
			Qty:       pos.qty,
			EntryPx:   pos.entryPx,
			ExitPx:    exitPx,
			GrossPnL:  gross,
			Fees:      fees,
			NetPnL:    net,
			Reason:    reason,
			EntryTS:   pos.openTS,
			openIndex: pos.openBar,
		})
		pos = position{}
		return nil
	}

	openPosition := func(barIdx int, fillBar data.Bar, side orders.Side, entryRaw orders.Decimal) error {
		entryPx, err := applySlippage(entryRaw, side, e.cfg.SlippageBps)
		if err != nil {
			return err
		}
		pos = position{
			open:     true,
			side:     side,
			qty:      e.cfg.Qty,
			entryPx:  entryPx,
			entryFee: e.cfg.CommissionPerTrade,
			openBar:  barIdx,
			openTS:   fillBar,
		}
		return nil
	}

	for i := range bars {
		bar := bars[i]

		// 1. Apply any pending fill scheduled for THIS bar's open (next-bar model).
		if e.cfg.EnterAtNextOpen {
			if pendExit.active && pos.open {
				if err := closeTrade(i, bar, bar.Open, pendExit.sig.Reason); err != nil {
					return Result{}, err
				}
				pendExit = pending{}
			}
			if pendEntry.active && !pos.open {
				side := entrySide(pendEntry.sig.Action)
				if err := openPosition(i, bar, side, bar.Open); err != nil {
					return Result{}, err
				}
				pendEntry = pending{}
			}
		}

		// 2. Ask the strategy for this bar's signal.
		sig := strat.OnBar(bar)

		// 3. Route the signal to a fill (now, at close) or schedule it (next open).
		switch sig.Action {
		case strategy.EnterLong, strategy.EnterShort:
			if !pos.open && !pendEntry.active {
				if e.cfg.EnterAtNextOpen {
					pendEntry = pending{active: true, sig: sig}
				} else {
					side := entrySide(sig.Action)
					if err := openPosition(i, bar, side, bar.Close); err != nil {
						return Result{}, err
					}
				}
			}
		case strategy.Exit:
			if pos.open && !pendExit.active {
				if e.cfg.EnterAtNextOpen {
					pendExit = pending{active: true, sig: sig}
				} else {
					if err := closeTrade(i, bar, bar.Close, sig.Reason); err != nil {
						return Result{}, err
					}
				}
			}
		case strategy.Hold:
			// nothing
		}
	}

	// 4. Close any still-open position at the last bar's close (no dangling P&L).
	if pos.open && len(bars) > 0 {
		last := bars[len(bars)-1]
		if err := closeTrade(len(bars)-1, last, last.Close, "backtest: end-of-data close"); err != nil {
			return Result{}, err
		}
	}

	res := Result{
		NetPnL:      netSum,
		GrossPnL:    grossSum,
		FeesPaid:    feeSum,
		NumTrades:   len(trades),
		NumWins:     wins,
		NumLosses:   losses,
		MaxDrawdown: maxDD,
		Trades:      trades,
	}

	endCash, err := e.cfg.StartingCash.Add(netSum)
	if err != nil {
		return Result{}, err
	}
	res.EndingCash = endCash

	// Win rate: integer counts → display ratio (not money).
	if res.NumTrades > 0 {
		res.WinRate = float64(wins) / float64(res.NumTrades)
		exp, err := netSum.Quo(intDecimal(res.NumTrades))
		if err != nil {
			return Result{}, err
		}
		res.Expectancy = exp
	}

	// Profit factor: sum of GROSS winning P&L / sum of |GROSS losing P&L| (both
	// Decimal); rendered as a display float. This measures the RAW signal edge
	// BEFORE costs — it is the overfit/quality signal, so fees never soften it. No
	// gross losers → +Inf (flagged as overfit below).
	res.ProfitFactor = profitFactor(grossProfitSum, grossLossSum)

	res.Warnings = honestyWarnings(res)
	return res, nil
}

// honestyWarnings surfaces the §4.5 overfitting red flags. Advisory, not a gate.
func honestyWarnings(res Result) []string {
	var w []string
	if res.NumTrades > 0 {
		if res.ProfitFactor > overfitProfitFactor {
			w = append(w, fmt.Sprintf(
				"profit factor %.4g exceeds %.1f — LIKELY OVERFIT (BUCKS is marketed on risk controls, not a claimed edge)",
				res.ProfitFactor, overfitProfitFactor))
		}
		if res.WinRate > overfitWinRate {
			w = append(w, fmt.Sprintf(
				"win rate %.2f%% exceeds %.0f%% — LIKELY OVERFIT (a real edge does not win this often; suspect look-ahead/curve-fit)",
				res.WinRate*100, overfitWinRate*100))
		}
	}
	return w
}

// PerTradeCost returns the modeled round-trip cost (entry + exit commission +
// entry + exit slippage) for a fill of qty at refPx, in exact Decimal. The
// expectancy-clears-cost gate compares Result.Expectancy against this. Slippage is
// charged on BOTH legs (buy up, sell down) so this is the full adverse cost a
// real round trip pays.
func (e *Engine) PerTradeCost(refPx, qty orders.Decimal) (orders.Decimal, error) {
	// Two commissions (entry + exit).
	twoComm, err := e.cfg.CommissionPerTrade.Add(e.cfg.CommissionPerTrade)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	// Slippage per leg = refPx * bps/10000 * qty; charged twice (entry + exit).
	slipPerLeg, err := slippageAmount(refPx, qty, e.cfg.SlippageBps)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	twoSlip, err := slipPerLeg.Add(slipPerLeg)
	if err != nil {
		return orders.ZeroDecimal, err
	}
	return twoComm.Add(twoSlip)
}

// Fingerprint returns a deterministic SHA-256 over the Result's money metrics AND
// the full trade log. It is the determinism proof: same bars + same config → same
// fingerprint. Everything hashed is rendered through Decimal.String() (canonical,
// scale-stable) and integer formatting — no float64 money, no map iteration. The
// only float64s included (WinRate, ProfitFactor) are formatted with a fixed
// precision so their text is byte-stable run-to-run.
func (r Result) Fingerprint() string {
	var b strings.Builder
	b.WriteString("net=")
	b.WriteString(r.NetPnL.String())
	b.WriteString("|gross=")
	b.WriteString(r.GrossPnL.String())
	b.WriteString("|fees=")
	b.WriteString(r.FeesPaid.String())
	b.WriteString("|end=")
	b.WriteString(r.EndingCash.String())
	b.WriteString("|n=")
	b.WriteString(strconv.Itoa(r.NumTrades))
	b.WriteString("|w=")
	b.WriteString(strconv.Itoa(r.NumWins))
	b.WriteString("|l=")
	b.WriteString(strconv.Itoa(r.NumLosses))
	b.WriteString("|wr=")
	b.WriteString(strconv.FormatFloat(r.WinRate, 'f', 6, 64))
	b.WriteString("|pf=")
	b.WriteString(strconv.FormatFloat(r.ProfitFactor, 'f', 6, 64))
	b.WriteString("|exp=")
	b.WriteString(r.Expectancy.String())
	b.WriteString("|dd=")
	b.WriteString(r.MaxDrawdown.String())
	for _, t := range r.Trades {
		b.WriteString("||trade:")
		b.WriteString(t.Symbol)
		b.WriteString(",side=")
		b.WriteString(t.Side.String())
		b.WriteString(",qty=")
		b.WriteString(t.Qty.String())
		b.WriteString(",entry=")
		b.WriteString(t.EntryPx.String())
		b.WriteString(",exit=")
		b.WriteString(t.ExitPx.String())
		b.WriteString(",gross=")
		b.WriteString(t.GrossPnL.String())
		b.WriteString(",fees=")
		b.WriteString(t.Fees.String())
		b.WriteString(",net=")
		b.WriteString(t.NetPnL.String())
		b.WriteString(",reason=")
		b.WriteString(t.Reason)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
