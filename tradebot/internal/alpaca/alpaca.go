// Package alpaca is a minimal REST client for Alpaca market data (bars, prices,
// news) and paper trading. Only the endpoints the bot needs are implemented.
package alpaca

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/market"
)

type Client struct {
	key, secret string
	dataURL     string
	tradeURL    string
	feed        string
	priceFeed   string
	http        *http.Client
}

// New builds a client from config; keys come from the environment.
func New(c config.AlpacaConfig) (*Client, error) {
	key, secret := os.Getenv(c.KeyEnv), os.Getenv(c.SecretEnv)
	if key == "" || secret == "" {
		return nil, fmt.Errorf("alpaca: set %s and %s", c.KeyEnv, c.SecretEnv)
	}
	if !strings.Contains(c.TradingBaseURL, "paper-api") {
		return nil, fmt.Errorf("alpaca: trading_base_url must be the paper endpoint; live trading goes through Robinhood")
	}
	priceFeed := c.PriceFeed
	if priceFeed == "" {
		priceFeed = "iex"
	}
	return &Client{
		key: key, secret: secret,
		dataURL: strings.TrimRight(c.DataBaseURL, "/"), tradeURL: strings.TrimRight(c.TradingBaseURL, "/"),
		feed: c.DataFeed, priceFeed: priceFeed,
		http: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (c *Client) do(ctx context.Context, method, rawURL string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("APCA-API-KEY-ID", c.key)
	req.Header.Set("APCA-API-SECRET-KEY", c.secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Retry idempotent reads on rate limits and server errors; never retry
	// order submissions (a retried POST could double an order).
	var resp *http.Response
	var b []byte
	for attempt := 0; ; attempt++ {
		resp, err = c.http.Do(req)
		if err == nil {
			b, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		retryable := method == http.MethodGet && (err != nil || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500)
		if !retryable || attempt >= 4 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 500 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("alpaca %s %s: %d %s", method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// ---- Market data ----

type rawBar struct {
	T time.Time `json:"t"`
	O float64   `json:"o"`
	H float64   `json:"h"`
	L float64   `json:"l"`
	C float64   `json:"c"`
	V float64   `json:"v"`
}

// DailyBars fetches split/dividend-adjusted daily bars for [start, end).
func (c *Client) DailyBars(ctx context.Context, symbols []string, start, end time.Time) (market.Dataset, error) {
	return c.bars(ctx, symbols, "1Day", start, end, true, c.feed)
}

// IntradayBars fetches raw-timestamp bars (e.g. timeframe "1Min", "5Min"),
// regular trading hours only, split-adjusted.
func (c *Client) IntradayBars(ctx context.Context, symbols []string, timeframe string, start, end time.Time) (market.Dataset, error) {
	return c.IntradayBarsFeed(ctx, symbols, timeframe, start, end, c.feed)
}

// IntradayBarsFeed is IntradayBars from an explicit feed ("iex" or "sip").
// Live intraday code uses the real-time price feed so today's bars are
// available (free plans cannot query SIP data from the last 15 minutes) and
// relative volume compares like with like.
func (c *Client) IntradayBarsFeed(ctx context.Context, symbols []string, timeframe string, start, end time.Time, feed string) (market.Dataset, error) {
	ds, err := c.bars(ctx, symbols, timeframe, start, end, false, feed)
	if err != nil {
		return nil, err
	}
	for sym, s := range ds {
		kept := s[:0]
		for _, b := range s {
			ny := b.Date.In(market.NewYork)
			if m := ny.Hour()*60 + ny.Minute(); m >= 9*60+30 && m < 16*60 {
				kept = append(kept, b)
			}
		}
		ds[sym] = kept
	}
	return ds, nil
}

// PriceFeed is the configured real-time feed.
func (c *Client) PriceFeed() string { return c.priceFeed }

func (c *Client) bars(ctx context.Context, symbols []string, timeframe string, start, end time.Time, daily bool, feed string) (market.Dataset, error) {
	ds := market.Dataset{}
	for i := 0; i < len(symbols); i += 50 {
		j := min(i+50, len(symbols))
		pageToken := ""
		for {
			q := url.Values{}
			q.Set("symbols", strings.Join(symbols[i:j], ","))
			q.Set("timeframe", timeframe)
			q.Set("start", start.UTC().Format(time.RFC3339))
			q.Set("end", end.UTC().Format(time.RFC3339))
			q.Set("adjustment", "all")
			q.Set("feed", feed)
			q.Set("limit", "10000")
			if pageToken != "" {
				q.Set("page_token", pageToken)
			}
			var resp struct {
				Bars          map[string][]rawBar `json:"bars"`
				NextPageToken *string             `json:"next_page_token"`
			}
			if err := c.do(ctx, http.MethodGet, c.dataURL+"/v2/stocks/bars?"+q.Encode(), nil, &resp); err != nil {
				return nil, err
			}
			for sym, bars := range resp.Bars {
				for _, b := range bars {
					d := b.T.UTC()
					if daily {
						d = market.TradingDate(b.T)
					}
					ds[sym] = append(ds[sym], market.Bar{Date: d, Open: b.O, High: b.H, Low: b.L, Close: b.C, Volume: b.V})
				}
			}
			if resp.NextPageToken == nil || *resp.NextPageToken == "" {
				break
			}
			pageToken = *resp.NextPageToken
		}
	}
	return ds, nil
}

// Trade is the latest trade for a symbol.
type Trade struct {
	Price float64
	Time  time.Time
}

// LatestTrades returns the latest trade (price and timestamp) per symbol in
// one request; intraday loops use the timestamp to reject stale quotes.
func (c *Client) LatestTrades(ctx context.Context, symbols []string) (map[string]Trade, error) {
	q := url.Values{}
	q.Set("symbols", strings.Join(symbols, ","))
	q.Set("feed", c.priceFeed)
	var resp struct {
		Trades map[string]struct {
			P float64   `json:"p"`
			T time.Time `json:"t"`
		} `json:"trades"`
	}
	if err := c.do(ctx, http.MethodGet, c.dataURL+"/v2/stocks/trades/latest?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	out := make(map[string]Trade, len(resp.Trades))
	for sym, t := range resp.Trades {
		if t.P > 0 {
			out[sym] = Trade{Price: t.P, Time: t.T}
		}
	}
	return out, nil
}

// NewsMulti returns recent headlines for several symbols, grouped by symbol.
func (c *Client) NewsMulti(ctx context.Context, symbols []string, since time.Time, perSymbol int) (map[string][]market.News, error) {
	want := map[string]bool{}
	for _, s := range symbols {
		want[s] = true
	}
	out := map[string][]market.News{}
	pageToken := ""
	for page := 0; page < 10; page++ {
		q := url.Values{}
		q.Set("symbols", strings.Join(symbols, ","))
		q.Set("start", since.UTC().Format(time.RFC3339))
		q.Set("limit", "50")
		q.Set("sort", "desc")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		var resp struct {
			News []struct {
				Headline  string    `json:"headline"`
				Summary   string    `json:"summary"`
				Source    string    `json:"source"`
				CreatedAt time.Time `json:"created_at"`
				Symbols   []string  `json:"symbols"`
			} `json:"news"`
			NextPageToken *string `json:"next_page_token"`
		}
		if err := c.do(ctx, http.MethodGet, c.dataURL+"/v1beta1/news?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		for _, n := range resp.News {
			for _, s := range n.Symbols {
				if want[s] && len(out[s]) < perSymbol {
					out[s] = append(out[s], market.News{Time: n.CreatedAt, Source: n.Source, Headline: n.Headline, Summary: n.Summary})
				}
			}
		}
		if resp.NextPageToken == nil || *resp.NextPageToken == "" {
			break
		}
		pageToken = *resp.NextPageToken
	}
	return out, nil
}

// LatestPrices returns the latest trade price per symbol.
func (c *Client) LatestPrices(ctx context.Context, symbols []string) (map[string]float64, error) {
	q := url.Values{}
	q.Set("symbols", strings.Join(symbols, ","))
	q.Set("feed", c.priceFeed)
	var resp map[string]struct {
		LatestTrade *struct {
			P float64 `json:"p"`
		} `json:"latestTrade"`
	}
	if err := c.do(ctx, http.MethodGet, c.dataURL+"/v2/stocks/snapshots?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for sym, s := range resp {
		if s.LatestTrade != nil && s.LatestTrade.P > 0 {
			out[sym] = s.LatestTrade.P
		}
	}
	return out, nil
}

// News returns recent headlines for a symbol, newest first.
func (c *Client) News(ctx context.Context, symbol string, since time.Time, limit int) ([]market.News, error) {
	q := url.Values{}
	q.Set("symbols", symbol)
	q.Set("start", since.UTC().Format(time.RFC3339))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("sort", "desc")
	var resp struct {
		News []struct {
			Headline  string    `json:"headline"`
			Summary   string    `json:"summary"`
			Source    string    `json:"source"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"news"`
	}
	if err := c.do(ctx, http.MethodGet, c.dataURL+"/v1beta1/news?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]market.News, 0, len(resp.News))
	for _, n := range resp.News {
		out = append(out, market.News{Time: n.CreatedAt, Source: n.Source, Headline: n.Headline, Summary: n.Summary})
	}
	return out, nil
}

// ---- Paper trading ----

type Account struct {
	Equity     float64
	LastEquity float64
	Cash       float64
	Blocked    bool
}

func (c *Client) Account(ctx context.Context) (Account, error) {
	var a struct {
		Equity         string `json:"equity"`
		LastEquity     string `json:"last_equity"`
		Cash           string `json:"cash"`
		TradingBlocked bool   `json:"trading_blocked"`
		AccountBlocked bool   `json:"account_blocked"`
	}
	if err := c.do(ctx, http.MethodGet, c.tradeURL+"/v2/account", nil, &a); err != nil {
		return Account{}, err
	}
	return Account{
		Equity: atof(a.Equity), LastEquity: atof(a.LastEquity), Cash: atof(a.Cash),
		Blocked: a.TradingBlocked || a.AccountBlocked,
	}, nil
}

type Position struct {
	Symbol   string
	Qty      float64
	AvgPrice float64
	Price    float64
}

func (c *Client) Positions(ctx context.Context) ([]Position, error) {
	var ps []struct {
		Symbol        string `json:"symbol"`
		Qty           string `json:"qty"`
		AvgEntryPrice string `json:"avg_entry_price"`
		CurrentPrice  string `json:"current_price"`
	}
	if err := c.do(ctx, http.MethodGet, c.tradeURL+"/v2/positions", nil, &ps); err != nil {
		return nil, err
	}
	out := make([]Position, 0, len(ps))
	for _, p := range ps {
		out = append(out, Position{Symbol: p.Symbol, Qty: atof(p.Qty), AvgPrice: atof(p.AvgEntryPrice), Price: atof(p.CurrentPrice)})
	}
	return out, nil
}

type OrderRequest struct {
	Symbol        string `json:"symbol"`
	Qty           string `json:"qty"`
	Side          string `json:"side"`
	Type          string `json:"type"`
	TimeInForce   string `json:"time_in_force"`
	LimitPrice    string `json:"limit_price"`
	ClientOrderID string `json:"client_order_id"`
}

type OrderResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (c *Client) SubmitOrder(ctx context.Context, o OrderRequest) (OrderResponse, error) {
	var r OrderResponse
	err := c.do(ctx, http.MethodPost, c.tradeURL+"/v2/orders", o, &r)
	return r, err
}

// OrderState is the fill status of one order.
type OrderState struct {
	Status      string
	FilledQty   float64
	FilledPrice float64
}

func (c *Client) GetOrder(ctx context.Context, id string) (OrderState, error) {
	var o struct {
		Status         string `json:"status"`
		FilledQty      string `json:"filled_qty"`
		FilledAvgPrice string `json:"filled_avg_price"`
	}
	if err := c.do(ctx, http.MethodGet, c.tradeURL+"/v2/orders/"+url.PathEscape(id), nil, &o); err != nil {
		return OrderState{}, err
	}
	return OrderState{Status: o.Status, FilledQty: atof(o.FilledQty), FilledPrice: atof(o.FilledAvgPrice)}, nil
}

func (c *Client) CancelOrder(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, c.tradeURL+"/v2/orders/"+url.PathEscape(id), nil, nil)
}

// IsTradingDay reports whether the exchange has a session on the given NY date.
func (c *Client) IsTradingDay(ctx context.Context, day time.Time) (bool, error) {
	d := day.Format("2006-01-02")
	var cal []struct {
		Date string `json:"date"`
	}
	if err := c.do(ctx, http.MethodGet, c.tradeURL+"/v2/calendar?start="+d+"&end="+d, nil, &cal); err != nil {
		return false, err
	}
	return len(cal) > 0 && cal[0].Date == d, nil
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
