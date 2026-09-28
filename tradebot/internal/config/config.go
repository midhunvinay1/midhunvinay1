// Package config loads and validates the bot configuration (YAML).
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Universe  []string        `yaml:"universe"`
	Benchmark string          `yaml:"benchmark"`
	Defensive string          `yaml:"defensive"` // held by the momentum sleeve in risk-off regimes; "" = cash
	Strategy  StrategyConfig  `yaml:"strategy"`
	Execution ExecutionConfig `yaml:"execution"`
	Risk      RiskConfig      `yaml:"risk"`
	Backtest  BacktestConfig  `yaml:"backtest"`
	LLM       LLMConfig       `yaml:"llm"`
	Alpaca    AlpacaConfig    `yaml:"alpaca"`
	Robinhood RobinhoodConfig `yaml:"robinhood"`
	Notify    NotifyConfig    `yaml:"notify"`
	Intraday  IntradayConfig  `yaml:"intraday"`
	StateDir  string          `yaml:"state_dir"`
}

type StrategyConfig struct {
	RegimeSMA       int                 `yaml:"regime_sma"`
	RegimeBand      float64             `yaml:"regime_band"`
	Momentum        MomentumConfig      `yaml:"momentum"`
	MeanReversion   MeanReversionConfig `yaml:"mean_reversion"`
	TargetVol       float64             `yaml:"target_vol"`
	VolWindow       int                 `yaml:"vol_window"`
	MaxSymbolWeight float64             `yaml:"max_symbol_weight"`
	MinPrice        float64             `yaml:"min_price"`
	MinDollarVolume float64             `yaml:"min_dollar_volume"`
	LiquidityWindow int                 `yaml:"liquidity_window"`
}

type MomentumConfig struct {
	Weight        float64 `yaml:"weight"`
	TopN          int     `yaml:"top_n"`
	Lookbacks     []int   `yaml:"lookbacks"`
	Skip          int     `yaml:"skip"`
	VolWindow     int     `yaml:"vol_window"`
	RebalanceDays int     `yaml:"rebalance_days"`
	TrendSMA      int     `yaml:"trend_sma"`
	TrailATRMult  float64 `yaml:"trail_atr_mult"`
	ATRWindow     int     `yaml:"atr_window"`
}

type MeanReversionConfig struct {
	Weight        float64 `yaml:"weight"`
	MaxPositions  int     `yaml:"max_positions"`
	RSIPeriod     int     `yaml:"rsi_period"`
	EntryRSI      float64 `yaml:"entry_rsi"`
	TrendSMA      int     `yaml:"trend_sma"`
	ExitSMA       int     `yaml:"exit_sma"`
	MaxHoldDays   int     `yaml:"max_hold_days"`
	StopATRMult   float64 `yaml:"stop_atr_mult"`
	ATRWindow     int     `yaml:"atr_window"`
	RequireRiskOn bool    `yaml:"require_risk_on"`
}

type ExecutionConfig struct {
	EntryLimitBps    float64 `yaml:"entry_limit_bps"`
	ExitLimitBps     float64 `yaml:"exit_limit_bps"`
	RebalanceBand    float64 `yaml:"rebalance_band"`
	AllowFractional  bool    `yaml:"allow_fractional"`
	MinOrderNotional float64 `yaml:"min_order_notional"`
}

type RiskConfig struct {
	MaxPositionWeight    float64  `yaml:"max_position_weight"`
	MaxGrossExposure     float64  `yaml:"max_gross_exposure"`
	MaxOrdersPerRun      int      `yaml:"max_orders_per_run"`
	MaxTurnoverPerRun    float64  `yaml:"max_turnover_per_run"`
	DailyLossLimit       float64  `yaml:"daily_loss_limit"`
	SoftDrawdown         float64  `yaml:"soft_drawdown"`
	SoftDrawdownScale    float64  `yaml:"soft_drawdown_scale"`
	HardDrawdown         float64  `yaml:"hard_drawdown"`
	MaxPriceDeviationBps float64  `yaml:"max_price_deviation_bps"`
	MaxDataAgeDays       int      `yaml:"max_data_age_days"`
	DenySymbols          []string `yaml:"deny_symbols"`
	HaltAllowsExits      bool     `yaml:"halt_allows_exits"`
}

type BacktestConfig struct {
	InitialEquity float64 `yaml:"initial_equity"`
	SlippageBps   float64 `yaml:"slippage_bps"`
	RiskFreeRate  float64 `yaml:"risk_free_rate"`
	Start         string  `yaml:"start"`
	End           string  `yaml:"end"`
}

type LLMConfig struct {
	Enabled          bool   `yaml:"enabled"`
	Model            string `yaml:"model"`
	Effort           string `yaml:"effort"`
	ReviewNewEntries bool   `yaml:"review_new_entries"`
	OnError          string `yaml:"on_error"` // "skip" (fail closed) or "allow"
	MaxCallsPerRun   int    `yaml:"max_calls_per_run"`
	NewsLookbackDays int    `yaml:"news_lookback_days"`
	MaxHeadlines     int    `yaml:"max_headlines"`
	APIKeyEnv        string `yaml:"api_key_env"`
}

type AlpacaConfig struct {
	KeyEnv         string `yaml:"key_env"`
	SecretEnv      string `yaml:"secret_env"`
	DataFeed       string `yaml:"data_feed"`  // feed for historical daily bars (sip works on the free plan for data older than 15 min)
	PriceFeed      string `yaml:"price_feed"` // feed for latest prices (iex on the free plan)
	DataBaseURL    string `yaml:"data_base_url"`
	TradingBaseURL string `yaml:"trading_base_url"`
}

type RobinhoodConfig struct {
	ClaudeBin          string              `yaml:"claude_bin"`
	ExecutorDir        string              `yaml:"executor_dir"`
	MCPServer          string              `yaml:"mcp_server"`
	TimeoutMinutes     int                 `yaml:"timeout_minutes"`
	OrderToolKeywords  []string            `yaml:"order_tool_keywords"`
	ReadToolKeywords   []string            `yaml:"read_tool_keywords"`
	DenyKeywords       []string            `yaml:"deny_keywords"`
	Fields             map[string][]string `yaml:"fields"`
	ForbiddenInputKeys []string            `yaml:"forbidden_input_keys"`
	PriceToleranceBps  float64             `yaml:"price_tolerance_bps"`
	IntentTTLMinutes   int                 `yaml:"intent_ttl_minutes"`
	Native             NativeMCPConfig     `yaml:"native"`
}

// NativeMCPConfig configures the direct (no-LLM) MCP client for Robinhood.
// Tool names and argument keys must be filled in from `tradebot rh-tools`.
type NativeMCPConfig struct {
	Enabled         bool                `yaml:"enabled"`
	ServerURL       string              `yaml:"server_url"`
	TokenFile       string              `yaml:"token_file"` // relative paths are inside the broker state dir
	CallbackPort    int                 `yaml:"callback_port"`
	ClientName      string              `yaml:"client_name"`
	AccountTool     string              `yaml:"account_tool"`
	PositionsTool   string              `yaml:"positions_tool"`
	PlaceOrderTool  string              `yaml:"place_order_tool"`
	OrderStatusTool string              `yaml:"order_status_tool"`
	CancelOrderTool string              `yaml:"cancel_order_tool"`
	OrderArgs       map[string]string   `yaml:"order_args"`  // logical field -> tool argument key
	FixedArgs       map[string]any      `yaml:"fixed_args"`  // extra constant arguments, e.g. type: limit
	ResultKeys      map[string][]string `yaml:"result_keys"` // logical field -> candidate keys in tool results
	CallTimeoutSec  int                 `yaml:"call_timeout_sec"`
	// NumbersAsStrings sends quantity and price as strings ("10", "101.25").
	NumbersAsStrings bool `yaml:"numbers_as_strings"`
}

// IntradayConfig configures the opening-range-breakout day-trading mode.
type IntradayConfig struct {
	Universe            []string `yaml:"universe"` // empty = top-level universe
	OpeningRangeMinutes int      `yaml:"opening_range_minutes"`
	RVOLLookbackDays    int      `yaml:"rvol_lookback_days"`
	MinRVOL             float64  `yaml:"min_rvol"`
	TopNInPlay          int      `yaml:"top_n_in_play"`
	MinATRDollars       float64  `yaml:"min_atr_dollars"`
	ATRDays             int      `yaml:"atr_days"`
	MinPrice            float64  `yaml:"min_price"`
	StopATRFraction     float64  `yaml:"stop_atr_fraction"`
	BreakevenAtR        float64  `yaml:"breakeven_at_r"` // 0 = off
	TakeProfitR         float64  `yaml:"take_profit_r"`  // 0 = hold to the close
	RiskPerTrade        float64  `yaml:"risk_per_trade"` // fraction of equity lost if the stop is hit
	MaxPositions        int      `yaml:"max_positions"`
	MaxPositionWeight   float64  `yaml:"max_position_weight"`
	MaxTradesPerDay     int      `yaml:"max_trades_per_day"`
	NoEntriesAfter      string   `yaml:"no_entries_after"` // HH:MM New York
	FlattenAt           string   `yaml:"flatten_at"`       // HH:MM New York
	DailyLossLimit      float64  `yaml:"daily_loss_limit"`
	DayTradeLimit5D     int      `yaml:"day_trade_limit_5d"` // 0 = unlimited (PDT rule removed; set 3 if your broker still enforces it)
	AccountType         string   `yaml:"account_type"`       // cash (buys limited to settled cash) | margin (still no leverage)
	EntryLimitBps       float64  `yaml:"entry_limit_bps"`
	ExitLimitBps        float64  `yaml:"exit_limit_bps"`
	SlippageBps         float64  `yaml:"slippage_bps"`
	PollSeconds         float64  `yaml:"poll_seconds"`
	MaxQuoteAgeSeconds  float64  `yaml:"max_quote_age_seconds"`
	OrderTimeoutSeconds float64  `yaml:"order_timeout_seconds"`
	PremarketReview     bool     `yaml:"premarket_review"`
	NewsLookbackHours   int      `yaml:"news_lookback_hours"`
	AllowSlowExecutor   bool     `yaml:"allow_slow_executor"` // allow the Claude Code executor intraday (10-30 s per order)
}

// IntradaySymbols returns the intraday universe (defaults to the main universe).
func (c *Config) IntradaySymbols() []string {
	if len(c.Intraday.Universe) > 0 {
		return c.Intraday.Universe
	}
	return c.Universe
}

type NotifyConfig struct {
	WebhookURLEnv string `yaml:"webhook_url_env"`
}

// Load reads a YAML file on top of Defaults() and validates the result.
func Load(path string) (*Config, error) {
	c := Defaults()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Symbols returns the universe plus benchmark and defensive symbols, deduplicated.
func (c *Config) Symbols() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range c.Universe {
		add(s)
	}
	add(c.Benchmark)
	add(c.Defensive)
	return out
}

func (c *Config) Validate() error {
	s := c.Strategy
	switch {
	case len(c.Universe) == 0:
		return fmt.Errorf("universe is empty")
	case c.Benchmark == "":
		return fmt.Errorf("benchmark is required")
	case s.Momentum.Weight < 0 || s.MeanReversion.Weight < 0:
		return fmt.Errorf("sleeve weights must be >= 0")
	case s.Momentum.Weight+s.MeanReversion.Weight > 1.0001:
		return fmt.Errorf("sleeve weights sum to %.2f (> 1.0, no leverage allowed)", s.Momentum.Weight+s.MeanReversion.Weight)
	case len(s.Momentum.Lookbacks) == 0:
		return fmt.Errorf("momentum.lookbacks is empty")
	case s.Momentum.TopN <= 0 || s.MeanReversion.MaxPositions < 0:
		return fmt.Errorf("momentum.top_n must be > 0 and mean_reversion.max_positions >= 0")
	case c.Risk.MaxGrossExposure > 1.0:
		return fmt.Errorf("risk.max_gross_exposure > 1.0 would use margin; not allowed")
	case c.Risk.HardDrawdown <= 0 || c.Risk.HardDrawdown >= 1:
		return fmt.Errorf("risk.hard_drawdown must be in (0,1)")
	case c.LLM.OnError != "skip" && c.LLM.OnError != "allow":
		return fmt.Errorf("llm.on_error must be skip or allow")
	}
	in := c.Intraday
	switch {
	case in.AccountType != "cash" && in.AccountType != "margin":
		return fmt.Errorf("intraday.account_type must be cash or margin")
	case in.RiskPerTrade <= 0 || in.RiskPerTrade > 0.03:
		return fmt.Errorf("intraday.risk_per_trade must be in (0, 0.03]")
	case in.MaxPositionWeight <= 0 || in.MaxPositionWeight*float64(in.MaxPositions) > 1.0001:
		return fmt.Errorf("intraday.max_positions * max_position_weight must be <= 1 (no leverage)")
	case in.PollSeconds < 1:
		return fmt.Errorf("intraday.poll_seconds must be >= 1 (data API rate limits)")
	case in.OpeningRangeMinutes < 1 || in.StopATRFraction <= 0:
		return fmt.Errorf("intraday.opening_range_minutes and stop_atr_fraction must be positive")
	}
	for _, hm := range []string{in.NoEntriesAfter, in.FlattenAt} {
		if _, err := ParseClock(hm); err != nil {
			return err
		}
	}
	for _, lb := range s.Momentum.Lookbacks {
		if lb <= s.Momentum.Skip {
			return fmt.Errorf("momentum lookback %d must exceed skip %d", lb, s.Momentum.Skip)
		}
	}
	return nil
}

// Defaults returns a complete, conservative configuration.
func Defaults() *Config {
	return &Config{
		Universe: []string{
			// Broad and sector ETFs
			"SPY", "QQQ", "IWM", "DIA", "XLK", "XLF", "XLE", "XLV", "XLY", "XLP", "XLI", "XLU", "XLB", "SMH", "GLD", "TLT",
			// Liquid large caps
			"AAPL", "MSFT", "NVDA", "AMZN", "GOOGL", "META", "AVGO", "TSLA", "JPM", "V", "MA", "UNH", "LLY", "XOM",
			"COST", "HD", "NFLX", "AMD", "CRM", "ORCL", "ADBE", "PEP", "KO", "WMT", "BAC", "CAT",
		},
		Benchmark: "SPY",
		Defensive: "SGOV",
		Strategy: StrategyConfig{
			RegimeSMA:  200,
			RegimeBand: 0.01,
			Momentum: MomentumConfig{
				Weight: 0.6, TopN: 5, Lookbacks: []int{63, 126, 252}, Skip: 21, VolWindow: 63,
				RebalanceDays: 21, TrendSMA: 100, TrailATRMult: 3.0, ATRWindow: 20,
			},
			MeanReversion: MeanReversionConfig{
				Weight: 0.4, MaxPositions: 4, RSIPeriod: 2, EntryRSI: 10, TrendSMA: 200, ExitSMA: 5,
				MaxHoldDays: 10, StopATRMult: 2.5, ATRWindow: 20, RequireRiskOn: true,
			},
			TargetVol:       0.18,
			VolWindow:       63,
			MaxSymbolWeight: 0.25,
			MinPrice:        5,
			MinDollarVolume: 20e6,
			LiquidityWindow: 20,
		},
		Execution: ExecutionConfig{
			EntryLimitBps: 30, ExitLimitBps: 100, RebalanceBand: 0.02, MinOrderNotional: 25,
		},
		Risk: RiskConfig{
			MaxPositionWeight: 0.30, MaxGrossExposure: 1.0, MaxOrdersPerRun: 20, MaxTurnoverPerRun: 1.2,
			DailyLossLimit: 0.04, SoftDrawdown: 0.15, SoftDrawdownScale: 0.5, HardDrawdown: 0.25,
			MaxPriceDeviationBps: 150, MaxDataAgeDays: 5, HaltAllowsExits: true,
		},
		Backtest: BacktestConfig{InitialEquity: 10000, SlippageBps: 5},
		LLM: LLMConfig{
			Enabled: true, Model: "claude-opus-5", Effort: "medium", ReviewNewEntries: true, OnError: "skip",
			MaxCallsPerRun: 10, NewsLookbackDays: 7, MaxHeadlines: 10, APIKeyEnv: "ANTHROPIC_API_KEY",
		},
		Alpaca: AlpacaConfig{
			KeyEnv: "APCA_API_KEY_ID", SecretEnv: "APCA_API_SECRET_KEY", DataFeed: "sip", PriceFeed: "iex",
			DataBaseURL: "https://data.alpaca.markets", TradingBaseURL: "https://paper-api.alpaca.markets",
		},
		Robinhood: RobinhoodConfig{
			ClaudeBin: "claude", ExecutorDir: "deploy/robinhood-executor", MCPServer: "robinhood-trading",
			TimeoutMinutes:    10,
			OrderToolKeywords: []string{"place", "submit", "create_order", "buy", "sell", "trade"},
			ReadToolKeywords:  []string{"get", "list", "search", "quote", "portfolio", "position", "holding", "balance", "status"},
			DenyKeywords: []string{"transfer", "withdraw", "deposit", "bank", "cancel", "close_all", "liquidate",
				"option", "crypto", "margin", "setting", "update", "delete", "disconnect"},
			Fields: map[string][]string{
				"symbol":      {"symbol", "ticker", "instrument_symbol"},
				"side":        {"side", "action", "direction"},
				"quantity":    {"quantity", "qty", "shares"},
				"limit_price": {"limit_price", "price", "limit"},
				"order_type":  {"order_type", "type"},
			},
			ForbiddenInputKeys: []string{"strike", "strike_price", "expiration", "expiration_date", "option_type", "legs",
				"notional", "dollar_amount", "amount_in_dollars", "stop_price", "trail_amount"},
			PriceToleranceBps: 50,
			IntentTTLMinutes:  30,
			Native: NativeMCPConfig{
				ServerURL: "https://agent.robinhood.com/mcp/trading", TokenFile: "oauth_token.json", CallbackPort: 3142,
				ClientName: "tradebot", CallTimeoutSec: 15,
				OrderArgs: map[string]string{"symbol": "symbol", "side": "side", "quantity": "quantity", "limit_price": "limit_price"},
				FixedArgs: map[string]any{"type": "limit", "time_in_force": "gfd"},
				ResultKeys: map[string][]string{
					"equity":     {"equity", "total_equity", "portfolio_value", "total_value"},
					"cash":       {"cash", "cash_available", "buying_power", "cash_balance"},
					"positions":  {"positions", "holdings"},
					"symbol":     {"symbol", "ticker"},
					"quantity":   {"quantity", "qty", "shares"},
					"avg_price":  {"average_price", "average_buy_price", "avg_price", "cost_basis_per_share"},
					"order_id":   {"order_id", "id"},
					"status":     {"status", "state"},
					"filled":     {"filled_quantity", "cumulative_quantity", "filled_qty"},
					"fill_price": {"average_fill_price", "average_price", "filled_avg_price", "price"},
				},
			},
		},
		Notify: NotifyConfig{WebhookURLEnv: "TRADEBOT_WEBHOOK_URL"},
		Intraday: IntradayConfig{
			OpeningRangeMinutes: 5, RVOLLookbackDays: 14, MinRVOL: 1.0, TopNInPlay: 10, MinATRDollars: 0.5, ATRDays: 14,
			MinPrice: 5, StopATRFraction: 0.10, RiskPerTrade: 0.01, MaxPositions: 4, MaxPositionWeight: 0.25,
			MaxTradesPerDay: 8, NoEntriesAfter: "15:00", FlattenAt: "15:55", DailyLossLimit: 0.02, AccountType: "cash",
			EntryLimitBps: 10, ExitLimitBps: 30, SlippageBps: 3, PollSeconds: 2, MaxQuoteAgeSeconds: 10,
			OrderTimeoutSeconds: 20, PremarketReview: true, NewsLookbackHours: 18,
		},
		StateDir: "state",
	}
}

// ParseClock parses "HH:MM" into minutes after midnight.
func ParseClock(hm string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(hm, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid time %q (want HH:MM)", hm)
	}
	return h*60 + m, nil
}
