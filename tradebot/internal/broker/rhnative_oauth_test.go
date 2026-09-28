package broker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/midhunvinay1/tradebot/internal/config"
)

// fakeAuthServer is a minimal OAuth 2.1 authorization server plus an
// MCP resource server protected by bearer tokens.
type fakeAuthServer struct {
	srv        *httptest.Server
	authorizes atomic.Int32
	refreshes  atomic.Int32
}

func newFakeAuthServer(t *testing.T) *fakeAuthServer {
	f := &fakeAuthServer{}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	base := f.srv.URL
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	prm := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}, "scopes_supported": []string{"trading"}})
	}
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", prm)
	mux.HandleFunc("/.well-known/oauth-protected-resource", prm)
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{
			"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
			"registration_endpoint": base + "/register", "response_types_supported": []string{"code"},
			"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{"trading", "offline_access"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var md map[string]any
		_ = json.NewDecoder(r.Body).Decode(&md)
		md["client_id"] = "client-1"
		writeJSON(w, 201, md)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		f.authorizes.Add(1)
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=code-1&state="+q.Get("state"), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			writeJSON(w, 200, map[string]any{"access_token": "access-1", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "refresh-1"})
		case "refresh_token":
			f.refreshes.Add(1)
			writeJSON(w, 200, map[string]any{"access_token": "access-2", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "refresh-2"})
		default:
			writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
		}
	})
	mcpSrv := (&fakeRobinhood{}).server()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)
	verify := func(_ context.Context, tok string, _ *http.Request) (*auth.TokenInfo, error) {
		if tok == "access-1" || tok == "access-2" {
			return &auth.TokenInfo{Scopes: []string{"trading"}, Expiration: time.Now().Add(time.Hour)}, nil
		}
		return nil, auth.ErrInvalidToken
	}
	mux.Handle("/mcp", auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{ResourceMetadataURL: base + "/.well-known/oauth-protected-resource/mcp"})(handler))
	t.Cleanup(f.srv.Close)
	return f
}

// browser "opens" the printed authorization URL by following its redirects.
type browser struct {
	mu  sync.Mutex
	buf string
}

var authURL = regexp.MustCompile(`https?://\S+/authorize\S*`)

func (b *browser) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf += string(p)
	if u := authURL.FindString(b.buf); u != "" {
		b.buf = ""
		go func() {
			if resp, err := http.Get(u); err == nil {
				resp.Body.Close()
			}
		}()
	}
	return len(p), nil
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestNativeOAuthLoginAndReuse(t *testing.T) {
	f := newFakeAuthServer(t)
	cfg := config.Defaults().Robinhood
	cfg.Native.ServerURL = f.srv.URL + "/mcp"
	cfg.Native.CallbackPort = freePort(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Non-interactive without a token must fail with a clear instruction.
	headless := &RobinhoodNative{Cfg: cfg, StateDir: dir}
	if _, err := headless.ListTools(ctx); err == nil {
		t.Fatal("expected an authorization error without a stored token")
	}
	headless.Close()

	login := &RobinhoodNative{Cfg: cfg, StateDir: dir, Interactive: true, Out: &browser{}}
	tools, err := login.ListTools(ctx)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	login.Close()
	if len(tools) == 0 || !login.HasToken() {
		t.Fatalf("login should list tools and store a token (tools %d)", len(tools))
	}

	// Expire the stored access token: the next headless run must refresh it
	// (no browser) and persist the new token.
	b, err := os.ReadFile(login.tokenPath())
	if err != nil {
		t.Fatal(err)
	}
	var st storedToken
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	st.Token.Expiry = time.Now().Add(-time.Hour)
	b, _ = json.Marshal(st)
	if err := os.WriteFile(login.tokenPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}

	again := &RobinhoodNative{Cfg: cfg, StateDir: dir}
	defer again.Close()
	if _, err := again.ListTools(ctx); err != nil {
		t.Fatalf("reuse stored token: %v", err)
	}
	if n := f.authorizes.Load(); n != 1 {
		t.Fatalf("browser authorization ran %d times, want 1", n)
	}
	if n := f.refreshes.Load(); n != 1 {
		t.Fatalf("expected one refresh, got %d", n)
	}
	b, _ = os.ReadFile(login.tokenPath())
	if !strings.Contains(string(b), "access-2") {
		t.Fatalf("refreshed token was not persisted: %s", b)
	}
	if fi, _ := os.Stat(login.tokenPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file permissions = %v, want 0600", fi.Mode().Perm())
	}
}
