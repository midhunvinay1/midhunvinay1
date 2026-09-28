package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/hook"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
)

// RobinhoodNative talks to Robinhood's Agentic Trading MCP server directly
// with the official Go MCP SDK: no LLM in the execution path, and sub-second
// order round trips instead of a Claude Code session per order.
//
// Authentication is MCP OAuth (discovery, dynamic client registration, PKCE).
// `tradebot rh-login` runs the browser flow once; the refresh token is stored
// (0600) in the broker state directory and refreshed automatically.
//
// Every order is still checked in-process by the same guard the Claude Code
// executor uses (internal/hook), and the only orders this broker will cancel
// are ones it placed itself in this process.
type RobinhoodNative struct {
	Cfg         config.RobinhoodConfig
	StateDir    string
	Interactive bool          // allow the browser OAuth flow (rh-login only)
	Transport   mcp.Transport // tests: bypasses OAuth and the network
	Out         io.Writer     // where the login URL is printed

	mu      sync.Mutex
	session *mcp.ClientSession
	placed  map[string]bool
}

func (r *RobinhoodNative) Name() string { return "robinhood-native" }

func (r *RobinhoodNative) tokenPath() string {
	p := r.Cfg.Native.TokenFile
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.StateDir, p)
	}
	return p
}

type storedToken struct {
	ClientID     string        `json:"client_id"`
	ClientSecret string        `json:"client_secret,omitempty"`
	AuthURL      string        `json:"auth_url"`
	TokenURL     string        `json:"token_url"`
	AuthStyle    int           `json:"auth_style"`
	Scopes       []string      `json:"scopes,omitempty"`
	Token        *oauth2.Token `json:"token"`
}

func (r *RobinhoodNative) saveToken(oc *oauth2.Config, t *oauth2.Token) error {
	st := storedToken{ClientID: oc.ClientID, ClientSecret: oc.ClientSecret, AuthURL: oc.Endpoint.AuthURL,
		TokenURL: oc.Endpoint.TokenURL, AuthStyle: int(oc.Endpoint.AuthStyle), Scopes: oc.Scopes, Token: t}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.tokenPath()), 0o700); err != nil {
		return err
	}
	tmp := r.tokenPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.tokenPath())
}

// HasToken reports whether a stored login exists.
func (r *RobinhoodNative) HasToken() bool {
	_, err := os.Stat(r.tokenPath())
	return err == nil
}

// persistingTokenSource saves every refreshed token so restarts keep working.
type persistingTokenSource struct {
	mu   sync.Mutex
	base oauth2.TokenSource
	last string
	save func(*oauth2.Token)
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	t, err := p.base.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.AccessToken != p.last {
		p.last = t.AccessToken
		p.save(t)
	}
	return t, nil
}

func (r *RobinhoodNative) redirectURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/callback", r.Cfg.Native.CallbackPort)
}

func (r *RobinhoodNative) oauthHandler() (*auth.AuthorizationCodeHandler, error) {
	n := r.Cfg.Native
	persist := func(oc *oauth2.Config, base oauth2.TokenSource, t *oauth2.Token) oauth2.TokenSource {
		return &persistingTokenSource{base: base, last: t.AccessToken, save: func(nt *oauth2.Token) { _ = r.saveToken(oc, nt) }}
	}
	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL: r.redirectURL(),
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: n.ClientName, RedirectURIs: []string{r.redirectURL()},
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none",
		}},
		AuthorizationCodeFetcher: r.fetchCode,
		RequestRefreshToken:      true,
		NewTokenSource: func(ctx context.Context, oc *oauth2.Config, t *oauth2.Token) (oauth2.TokenSource, error) {
			if err := r.saveToken(oc, t); err != nil {
				return nil, err
			}
			return persist(oc, oc.TokenSource(ctx, t), t), nil
		},
	}
	if b, err := os.ReadFile(r.tokenPath()); err == nil {
		var st storedToken
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, fmt.Errorf("corrupt token file %s: %w", r.tokenPath(), err)
		}
		oc := &oauth2.Config{ClientID: st.ClientID, ClientSecret: st.ClientSecret, Scopes: st.Scopes,
			Endpoint: oauth2.Endpoint{AuthURL: st.AuthURL, TokenURL: st.TokenURL, AuthStyle: oauth2.AuthStyle(st.AuthStyle)}}
		cfg.InitialTokenSource = persist(oc, oc.TokenSource(context.Background(), st.Token), st.Token)
	}
	return auth.NewAuthorizationCodeHandler(cfg)
}

// fetchCode runs the loopback redirect listener for the browser login.
func (r *RobinhoodNative) fetchCode(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	if !r.Interactive {
		return nil, errors.New("Robinhood authorization required: run `tradebot rh-login` on this machine")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", r.Cfg.Native.CallbackPort))
	if err != nil {
		return nil, err
	}
	ch := make(chan *auth.AuthorizationResult, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/callback" {
			http.NotFound(w, req)
			return
		}
		q := req.URL.Query()
		if e := q.Get("error"); e != "" {
			fmt.Fprintf(w, "Authorization failed: %s", e)
			return
		}
		select {
		case ch <- &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}:
		default:
		}
		fmt.Fprint(w, "tradebot is authorized with Robinhood. You can close this window.")
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	out := r.Out
	if out == nil {
		out = os.Stdout
	}
	fmt.Fprintf(out, "\nOpen this URL in your browser and approve tradebot in Robinhood:\n\n  %s\n\nWaiting for the redirect to %s ...\n", args.URL, r.redirectURL())
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Minute):
		return nil, errors.New("timed out waiting for the Robinhood authorization redirect")
	}
}

// Connect opens (or reuses) the MCP session.
func (r *RobinhoodNative) Connect(ctx context.Context) (*mcp.ClientSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session != nil {
		return r.session, nil
	}
	t := r.Transport
	if t == nil {
		h, err := r.oauthHandler()
		if err != nil {
			return nil, err
		}
		t = &mcp.StreamableClientTransport{
			Endpoint: r.Cfg.Native.ServerURL, OAuthHandler: h, DisableStandaloneSSE: true,
			HTTPClient: &http.Client{Timeout: 60 * time.Second},
		}
	}
	c := mcp.NewClient(&mcp.Implementation{Name: r.Cfg.Native.ClientName, Version: "1.0.0"}, nil)
	s, err := c.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("robinhood MCP connect: %w", err)
	}
	r.session = s
	return s, nil
}

func (r *RobinhoodNative) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session == nil {
		return nil
	}
	err := r.session.Close()
	r.session = nil
	return err
}

// ListTools returns every tool the server exposes (for `tradebot rh-tools`).
func (r *RobinhoodNative) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	s, err := r.Connect(ctx)
	if err != nil {
		return nil, err
	}
	var tools []*mcp.Tool
	cursor := ""
	for {
		res, err := s.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

// call invokes one tool and decodes its result into generic JSON values.
func (r *RobinhoodNative) call(ctx context.Context, tool string, args map[string]any) (any, error) {
	if tool == "" {
		return nil, errors.New("robinhood native: tool name not configured (see `tradebot rh-tools`)")
	}
	s, err := r.Connect(ctx)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(max(r.Cfg.Native.CallTimeoutSec, 1)) * time.Second
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := s.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("robinhood %s: %w", tool, err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		return nil, fmt.Errorf("robinhood %s returned an error: %s", tool, strings.TrimSpace(text))
	}
	if res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			var v any
			if json.Unmarshal(b, &v) == nil {
				return v, nil
			}
		}
	}
	var v any
	if err := json.Unmarshal([]byte(text), &v); err == nil {
		return v, nil
	}
	return text, nil
}

func (r *RobinhoodNative) keys(field string) []string { return r.Cfg.Native.ResultKeys[field] }

func (r *RobinhoodNative) Account(ctx context.Context) (Account, error) {
	n := r.Cfg.Native
	v, err := r.call(ctx, n.AccountTool, map[string]any{})
	if err != nil {
		return Account{}, err
	}
	acct := Account{Positions: map[string]Position{}}
	acct.Equity, _ = findNum(v, r.keys("equity"))
	acct.Cash, _ = findNum(v, r.keys("cash"))
	pv := v
	if n.PositionsTool != "" && n.PositionsTool != n.AccountTool {
		if pv, err = r.call(ctx, n.PositionsTool, map[string]any{}); err != nil {
			return Account{}, err
		}
	}
	list, _ := pv.([]any)
	if list == nil {
		if x, ok := findVal(pv, r.keys("positions")); ok {
			list, _ = x.([]any)
		}
	}
	for _, item := range list {
		sym, _ := findStr(item, r.keys("symbol"))
		qty, okQ := findNum(item, r.keys("quantity"))
		avg, _ := findNum(item, r.keys("avg_price"))
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" || !okQ || qty < 0 {
			return Account{}, fmt.Errorf("robinhood native: cannot parse position %v", item)
		}
		if qty > 0 {
			acct.Positions[sym] = Position{Symbol: sym, Qty: qty, AvgPrice: avg}
		}
	}
	if !(acct.Equity > 0) || acct.Cash < 0 || acct.Cash > acct.Equity*1.01 {
		return Account{}, fmt.Errorf("robinhood native: implausible equity %.2f / cash %.2f (check result_keys)", acct.Equity, acct.Cash)
	}
	return acct, nil
}

// OrderArgs builds the place-order arguments for o from the configured mapping.
func (r *RobinhoodNative) OrderArgs(o portfolio.Order) map[string]any {
	n := r.Cfg.Native
	args := map[string]any{}
	for k, v := range n.FixedArgs {
		args[k] = v
	}
	num := func(x float64) any {
		if n.NumbersAsStrings {
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return x
	}
	set := func(field string, v any) {
		if k := n.OrderArgs[field]; k != "" {
			args[k] = v
		}
	}
	set("symbol", o.Symbol)
	set("side", o.Side)
	set("quantity", num(o.Qty))
	set("limit_price", num(market.RoundTick(o.LimitPrice)))
	if o.ID != "" {
		set("client_order_id", o.ID)
	}
	return args
}

func (r *RobinhoodNative) Submit(ctx context.Context, orders []portfolio.Order) ([]Result, error) {
	n := r.Cfg.Native
	now := time.Now()
	intents := hook.IntentFile{RunID: "native", CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	for _, o := range orders {
		intents.Intents = append(intents.Intents, hook.Intent{ID: o.ID, Symbol: o.Symbol, Side: o.Side, Qty: o.Qty, LimitPrice: market.RoundTick(o.LimitPrice)})
	}
	claimed := map[string]bool{}
	var out []Result
	for _, o := range orders {
		res := Result{OrderID: o.ID, Symbol: o.Symbol, Side: o.Side, Status: "failed"}
		args := r.OrderArgs(o)
		// Same guard as the Claude Code executor: catches config mistakes such
		// as market orders, option fields or a mis-mapped quantity.
		d := hook.Evaluate(hook.Call{ToolName: "mcp__" + r.Cfg.MCPServer + "__" + n.PlaceOrderTool, ToolInput: args},
			r.Cfg, intents, func(id string) bool { return claimed[id] }, now)
		if !d.Allow {
			res.Message = "guard: " + d.Reason
			out = append(out, res)
			continue
		}
		claimed[d.IntentID] = true
		v, err := r.call(ctx, n.PlaceOrderTool, args)
		if err != nil {
			res.Message = err.Error()
			out = append(out, res)
			continue
		}
		id, _ := findStr(v, r.keys("order_id"))
		if id == "" {
			res.Status, res.Message = "unknown", "order sent but no order id in the response; verify in the Robinhood app"
			out = append(out, res)
			continue
		}
		r.mu.Lock()
		if r.placed == nil {
			r.placed = map[string]bool{}
		}
		r.placed[id] = true
		r.mu.Unlock()
		res.Status, res.BrokerOrderID = "submitted", id
		out = append(out, res)
	}
	return out, nil
}

func (r *RobinhoodNative) idArgs(id string) map[string]any {
	k := r.Cfg.Native.OrderArgs["order_id"]
	if k == "" {
		k = "order_id"
	}
	return map[string]any{k: id}
}

func (r *RobinhoodNative) OrderStatus(ctx context.Context, id string) (OrderStatus, error) {
	v, err := r.call(ctx, r.Cfg.Native.OrderStatusTool, r.idArgs(id))
	if err != nil {
		return OrderStatus{}, err
	}
	st, _ := findStr(v, r.keys("status"))
	qty, _ := findNum(v, r.keys("filled"))
	px, _ := findNum(v, r.keys("fill_price"))
	return OrderStatus{Status: NormalizeStatus(st), FilledQty: qty, FilledPrice: px}, nil
}

// Cancel cancels an order this process placed; anything else is refused.
func (r *RobinhoodNative) Cancel(ctx context.Context, id string) error {
	r.mu.Lock()
	ok := r.placed[id]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("refusing to cancel order %s: not placed by this process", id)
	}
	_, err := r.call(ctx, r.Cfg.Native.CancelOrderTool, r.idArgs(id))
	return err
}

// ---- generic JSON extraction ----

// findVal does a breadth-first search for the first key (case-insensitive)
// in keys, through nested objects and arrays.
func findVal(v any, keys []string) (any, bool) {
	queue := []any{v}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		switch x := cur.(type) {
		case map[string]any:
			for _, k := range keys {
				for ik, iv := range x {
					if strings.EqualFold(ik, k) {
						return iv, true
					}
				}
			}
			for _, k := range market.SortedKeys(x) {
				queue = append(queue, x[k])
			}
		case []any:
			queue = append(queue, x...)
		}
	}
	return nil, false
}

func findStr(v any, keys []string) (string, bool) {
	x, ok := findVal(v, keys)
	if !ok {
		return "", false
	}
	switch s := x.(type) {
	case string:
		return s, true
	case float64:
		return strconv.FormatFloat(s, 'f', -1, 64), true
	}
	return "", false
}

func findNum(v any, keys []string) (float64, bool) {
	x, ok := findVal(v, keys)
	if !ok {
		return 0, false
	}
	switch n := x.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case string:
		f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(n), "$"), ",", ""), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}
