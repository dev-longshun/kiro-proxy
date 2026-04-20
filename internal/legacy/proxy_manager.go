package legacy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// AccountLike is an interface for account types used by ProxyManager
type AccountLike interface {
	GetID() string
	GetProxyID() string
}

// ProxyInfo represents a configured proxy with account binding
type ProxyInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`          // http://user:pass@host:port or socks5://...
	Type        string    `json:"type"`         // http, socks5
	Enabled     bool      `json:"enabled"`
	MaxAccounts int       `json:"max_accounts"` // max accounts per proxy, 0 = unlimited
	ExpiresAt   string    `json:"expires_at,omitempty"` // expiry date string, e.g. "2026-4-10"
	CreatedAt   time.Time `json:"created_at"`

	// Bound accounts (account IDs)
	BoundAccounts []string `json:"bound_accounts"`

	// Runtime stats
	SuccessCount int64     `json:"success_count"`
	ErrorCount   int64     `json:"error_count"`
	LastUsedAt   time.Time `json:"last_used_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	LastLatency  int64     `json:"last_latency_ms,omitempty"`
	LastTestIP   string    `json:"last_test_ip,omitempty"`
}

// AvailableSlots returns how many more accounts can bind to this proxy
func (p *ProxyInfo) AvailableSlots() int {
	if p.MaxAccounts <= 0 {
		return 999 // unlimited
	}
	slots := p.MaxAccounts - len(p.BoundAccounts)
	if slots < 0 {
		return 0
	}
	return slots
}

// IsBound checks if an account is bound to this proxy
func (p *ProxyInfo) IsBound(accountID string) bool {
	for _, id := range p.BoundAccounts {
		if id == accountID {
			return true
		}
	}
	return false
}

// ProxyManager manages proxies with account binding
type ProxyManager struct {
	proxies        []*ProxyInfo
	mu             sync.RWMutex
	filePath       string
	db             *Database
	PreProxy       string // pre-proxy URL, e.g. "http://127.0.0.1:7897"
	transportCache map[string]http.RoundTripper // 缓存: proxyID → Transport
	transportMu    sync.RWMutex
}

func NewProxyManager(db *Database) *ProxyManager {
	pm := &ProxyManager{
		filePath:       "proxies.json",
		db:             db,
		transportCache: make(map[string]http.RoundTripper),
	}
	pm.loadFromDB()
	// Load pre_proxy from DB
	if db != nil {
		if val := db.GetSetting("pre_proxy"); val != "" {
			pm.PreProxy = val
			log.Printf("[ProxyManager] Loaded pre_proxy: %s", val)
		}
	}
	return pm
}

func (pm *ProxyManager) loadFromDB() {
	if pm.db == nil {
		pm.loadFromFile()
		return
	}
	proxies, err := pm.db.LoadProxies()
	if err != nil {
		log.Printf("[ProxyManager] DB load error, falling back to file: %v", err)
		pm.loadFromFile()
		return
	}
	if len(proxies) > 0 {
		pm.proxies = proxies
		log.Printf("[ProxyManager] Loaded %d proxies from database", len(proxies))
	} else {
		pm.loadFromFile()
	}
}

// ReconcileBindings cleans up stale bindings by cross-checking with accounts
func (pm *ProxyManager) ReconcileBindings(accounts []AccountLike) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Build map: accountID -> proxyID (from accounts as source of truth)
	accProxy := make(map[string]string)
	for _, acc := range accounts {
		if acc.GetProxyID() != "" {
			accProxy[acc.GetID()] = acc.GetProxyID()
		}
	}

	changed := false
	for _, p := range pm.proxies {
		var clean []string
		for _, accID := range p.BoundAccounts {
			// Keep only if this account's ProxyID points to THIS proxy
			if boundTo, ok := accProxy[accID]; ok && boundTo == p.ID {
				clean = append(clean, accID)
			} else {
				log.Printf("[ProxyManager] 清理残留绑定: proxy=%s account=%s (实际绑定=%s)", p.Name, accID, accProxy[accID])
				changed = true
			}
		}
		if clean == nil {
			clean = []string{}
		}
		p.BoundAccounts = clean
	}

	if changed {
		pm.saveToFile()
	}
}

// ParseProxyLine parses various proxy formats into a standard ProxyInfo.
// Supported formats:
//   - IP|端口|用户名|密码|过期时间    (e.g. "113.108.88.5|5858|2222530|678958|2026-4-10")
//   - IP|端口|用户名|密码             (no expiry)
//   - IP|端口                         (no auth)
//   - http://user:pass@host:port      (standard URL)
//   - socks5://user:pass@host:port    (standard URL)
//
// For pipe format, auto-detects protocol by trying SOCKS5 first then HTTP.
func ParseProxyLine(line string) *ProxyInfo {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil
	}

	// Already a URL?
	if strings.Contains(line, "://") {
		return &ProxyInfo{URL: line}
	}

	// Pipe-separated format: IP|Port|User|Pass|Expiry
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return nil
	}

	host := strings.TrimSpace(parts[0])
	port := strings.TrimSpace(parts[1])

	p := &ProxyInfo{
		Name: host + ":" + port,
		Type: "socks5", // default to socks5, most common for this format
	}

	if len(parts) >= 4 {
		user := strings.TrimSpace(parts[2])
		pass := strings.TrimSpace(parts[3])
		p.URL = fmt.Sprintf("socks5://%s:%s@%s:%s", user, pass, host, port)
	} else {
		p.URL = fmt.Sprintf("socks5://%s:%s", host, port)
	}

	if len(parts) >= 5 {
		p.ExpiresAt = strings.TrimSpace(parts[4])
	}

	return p
}

// ParseProxyBatch parses multiple proxy lines (one per line)
func ParseProxyBatch(text string) []*ProxyInfo {
	var result []*ProxyInfo
	for _, line := range strings.Split(text, "\n") {
		if p := ParseProxyLine(line); p != nil {
			result = append(result, p)
		}
	}
	return result
}

// AddProxy adds a new proxy
func (pm *ProxyManager) AddProxy(p *ProxyInfo) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if p.ID == "" {
		p.ID = fmt.Sprintf("px_%d", time.Now().UnixNano())
	}
	if p.Type == "" {
		if strings.HasPrefix(p.URL, "socks5://") || strings.HasPrefix(p.URL, "socks5h://") {
			p.Type = "socks5"
		} else {
			p.Type = "http"
		}
	}
	if p.BoundAccounts == nil {
		p.BoundAccounts = []string{}
	}
	p.CreatedAt = time.Now()
	p.Enabled = true
	pm.proxies = append(pm.proxies, p)
	pm.saveToFile()
	log.Printf("[ProxyManager] Added proxy: %s (%s) type=%s max=%d", p.Name, MaskProxyURL(p.URL), p.Type, p.MaxAccounts)
}

// DeleteProxy removes a proxy and unbinds all accounts
func (pm *ProxyManager) DeleteProxy(id string) ([]string, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for i, p := range pm.proxies {
		if p.ID == id {
			unboundAccounts := make([]string, len(p.BoundAccounts))
			copy(unboundAccounts, p.BoundAccounts)
			pm.proxies = append(pm.proxies[:i], pm.proxies[i+1:]...)
			pm.saveToFile()
			return unboundAccounts, true
		}
	}
	return nil, false
}

// ToggleProxy enables/disables a proxy
func (pm *ProxyManager) ToggleProxy(id string) (*ProxyInfo, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, p := range pm.proxies {
		if p.ID == id {
			p.Enabled = !p.Enabled
			pm.saveToFile()
			return p, true
		}
	}
	return nil, false
}

// GetAllProxies returns all proxies
func (pm *ProxyManager) GetAllProxies() []*ProxyInfo {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	result := make([]*ProxyInfo, len(pm.proxies))
	copy(result, pm.proxies)
	return result
}

// GetProxy returns a proxy by ID
func (pm *ProxyManager) GetProxy(id string) *ProxyInfo {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, p := range pm.proxies {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// BindAccount binds an account to a proxy
func (pm *ProxyManager) BindAccount(proxyID, accountID string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, p := range pm.proxies {
		if p.ID == proxyID {
			// Already bound?
			for _, id := range p.BoundAccounts {
				if id == accountID {
					return true
				}
			}
			// Check capacity
			if p.MaxAccounts > 0 && len(p.BoundAccounts) >= p.MaxAccounts {
				log.Printf("[ProxyManager] Proxy %s is full (%d/%d)", p.Name, len(p.BoundAccounts), p.MaxAccounts)
				return false
			}
			p.BoundAccounts = append(p.BoundAccounts, accountID)
			pm.saveToFile()
			log.Printf("[ProxyManager] Bound account %s to proxy %s (%d/%d)",
				accountID, p.Name, len(p.BoundAccounts), p.MaxAccounts)
			return true
		}
	}
	return false
}

// UnbindAccount removes an account from a proxy
func (pm *ProxyManager) UnbindAccount(proxyID, accountID string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, p := range pm.proxies {
		if p.ID == proxyID {
			for i, id := range p.BoundAccounts {
				if id == accountID {
					p.BoundAccounts = append(p.BoundAccounts[:i], p.BoundAccounts[i+1:]...)
					pm.saveToFile()
					log.Printf("[ProxyManager] Unbound account %s from proxy %s (%d/%d)",
						accountID, p.Name, len(p.BoundAccounts), p.MaxAccounts)
					return true
				}
			}
			return false
		}
	}
	return false
}

// UnbindAccountFromAll removes an account from all proxies (used when account is deleted/suspended)
func (pm *ProxyManager) UnbindAccountFromAll(accountID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	changed := false
	for _, p := range pm.proxies {
		for i, id := range p.BoundAccounts {
			if id == accountID {
				p.BoundAccounts = append(p.BoundAccounts[:i], p.BoundAccounts[i+1:]...)
				log.Printf("[ProxyManager] Auto-unbound account %s from proxy %s", accountID, p.Name)
				changed = true
				break
			}
		}
	}
	if changed {
		pm.saveToFile()
	}
}

// GetProxyForAccount returns the proxy bound to an account (by account ID)
func (pm *ProxyManager) GetProxyForAccount(accountID string) *ProxyInfo {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	for _, p := range pm.proxies {
		if !p.Enabled {
			continue
		}
		for _, id := range p.BoundAccounts {
			if id == accountID {
				return p
			}
		}
	}
	return nil
}

// AutoAssignProxy finds a proxy with available slots and binds the account
func (pm *ProxyManager) AutoAssignProxy(accountID string) *ProxyInfo {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Check if already bound
	for _, p := range pm.proxies {
		for _, id := range p.BoundAccounts {
			if id == accountID {
				return p
			}
		}
	}

	// Find proxy with most available slots
	var best *ProxyInfo
	bestSlots := 0
	for _, p := range pm.proxies {
		if !p.Enabled {
			continue
		}
		slots := p.AvailableSlots()
		if slots > bestSlots {
			bestSlots = slots
			best = p
		}
	}

	if best != nil {
		best.BoundAccounts = append(best.BoundAccounts, accountID)
		pm.saveToFile()
		log.Printf("[ProxyManager] Auto-assigned account %s to proxy %s (%d/%d)",
			accountID, best.Name, len(best.BoundAccounts), best.MaxAccounts)
	}
	return best
}

// GetTransportForAccount returns an http.Transport for the account's bound proxy
func (pm *ProxyManager) GetTransportForAccount(accountID string) http.RoundTripper {
	p := pm.GetProxyForAccount(accountID)
	if p == nil {
		return nil
	}
	return pm.getCachedTransport(p)
}

// GetTransport returns a transport for any available proxy (fallback for unbound accounts)
func (pm *ProxyManager) GetTransport(purpose string) http.RoundTripper {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, p := range pm.proxies {
		if p.Enabled {
			return pm.getCachedTransport(p)
		}
	}
	return nil
}

// getCachedTransport 获取或创建代理的 Transport（复用连接池）
func (pm *ProxyManager) getCachedTransport(p *ProxyInfo) http.RoundTripper {
	cacheKey := p.ID + ":" + pm.GetPreProxy()

	pm.transportMu.RLock()
	if t, ok := pm.transportCache[cacheKey]; ok {
		pm.transportMu.RUnlock()
		return t
	}
	pm.transportMu.RUnlock()

	// 创建新的 transport
	t := pm.BuildTransport(p)
	if t != nil {
		pm.transportMu.Lock()
		pm.transportCache[cacheKey] = t
		pm.transportMu.Unlock()
	}
	return t
}

// ClearTransportCache 清除 transport 缓存（代理配置变更时调用）
func (pm *ProxyManager) ClearTransportCache() {
	pm.transportMu.Lock()
	defer pm.transportMu.Unlock()
	pm.transportCache = make(map[string]http.RoundTripper)
	log.Printf("[ProxyManager] Transport 缓存已清除")
}

// RecordSuccess records a successful proxy use
func (pm *ProxyManager) RecordSuccess(p *ProxyInfo) {
	if p == nil {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	p.SuccessCount++
	p.LastUsedAt = time.Now()
	p.LastError = ""
}

// RecordError records a failed proxy use
func (pm *ProxyManager) RecordError(p *ProxyInfo, err string) {
	if p == nil {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	p.ErrorCount++
	p.LastUsedAt = time.Now()
	p.LastError = err
}

// BuildTransport creates an http.RoundTripper for a proxy
func (pm *ProxyManager) BuildTransport(p *ProxyInfo) http.RoundTripper {
	proxyURL, err := url.Parse(p.URL)
	if err != nil {
		log.Printf("[ProxyManager] Invalid proxy URL %s: %v", MaskProxyURL(p.URL), err)
		return nil
	}

	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	connectTimeout := 15 * time.Second
	cfg := GlobalRateLimiter.GetConfig()
	if cfg.ConnectTimeoutSeconds > 0 {
		connectTimeout = time.Duration(cfg.ConnectTimeoutSeconds) * time.Second
	}
	dialer := &net.Dialer{
		Timeout:   connectTimeout,
		KeepAlive: 30 * time.Second,
	}
	transportDefaults := func(t *http.Transport) *http.Transport {
		t.TLSClientConfig = tlsConfig
		t.TLSHandshakeTimeout = connectTimeout
		if t.DialContext == nil {
			t.DialContext = dialer.DialContext
		}
		t.DisableCompression = true
		t.ForceAttemptHTTP2 = true
		t.MaxIdleConns = 100
		t.MaxIdleConnsPerHost = 20
		t.IdleConnTimeout = 90 * time.Second
		t.ResponseHeaderTimeout = 120 * time.Second
		return t
	}

	if p.Type == "socks5" || strings.HasPrefix(p.URL, "socks5") {
		var auth *proxy.Auth
		if proxyURL.User != nil {
			pass, _ := proxyURL.User.Password()
			auth = &proxy.Auth{
				User:     proxyURL.User.Username(),
				Password: pass,
			}
		}

		pm.mu.RLock()
		preProxy := pm.PreProxy
		pm.mu.RUnlock()

		// Chain mode: pre_proxy (HTTP CONNECT) → SOCKS5 → target
		if preProxy != "" {
			preProxyURL, err := url.Parse(preProxy)
			if err != nil {
				log.Printf("[ProxyManager] Invalid pre_proxy URL: %v", err)
				return nil
			}
			socksAddr := proxyURL.Host
			return transportDefaults(&http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return chainDial(ctx, preProxyURL.Host, socksAddr, auth, addr)
				},
			})
		}

		// Direct SOCKS5
		dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, proxy.Direct)
		if err != nil {
			log.Printf("[ProxyManager] Failed to create SOCKS5 dialer: %v", err)
			return nil
		}
		return transportDefaults(&http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		})
	}

	return transportDefaults(&http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	})
}

// UpdateProxy updates a proxy's fields using a modifier function
func (pm *ProxyManager) UpdateProxy(id string, modifier func(p *ProxyInfo)) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, p := range pm.proxies {
		if p.ID == id {
			modifier(p)
			pm.saveToFile()
			return true
		}
	}
	return false
}

// HasProxies returns true if any proxies are configured
func (pm *ProxyManager) HasProxies() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return len(pm.proxies) > 0
}

func (pm *ProxyManager) saveToFile() {
	// Save to DB if available
	if pm.db != nil {
		for _, p := range pm.proxies {
			pm.db.SaveProxy(p)
		}
		return
	}
	data, err := json.MarshalIndent(pm.proxies, "", "  ")
	if err != nil {
		log.Printf("[ProxyManager] Failed to marshal proxies: %v", err)
		return
	}
	if err := os.WriteFile(pm.filePath, data, 0644); err != nil {
		log.Printf("[ProxyManager] Failed to save proxies: %v", err)
	}
}

func (pm *ProxyManager) loadFromFile() {
	data, err := os.ReadFile(pm.filePath)
	if err != nil {
		return
	}
	var proxies []*ProxyInfo
	if err := json.Unmarshal(data, &proxies); err != nil {
		log.Printf("[ProxyManager] Failed to parse proxies file: %v", err)
		return
	}
	// Ensure BoundAccounts is never nil
	for _, p := range proxies {
		if p.BoundAccounts == nil {
			p.BoundAccounts = []string{}
		}
	}
	pm.proxies = proxies
	log.Printf("[ProxyManager] Loaded %d proxies from file", len(proxies))
}

// MaskProxyURL masks credentials in proxy URL for display
func MaskProxyURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		if len(rawURL) > 20 {
			return rawURL[:20] + "..."
		}
		return rawURL
	}
	if u.User != nil {
		user := u.User.Username()
		if len(user) > 1 {
			user = user[:1] + "***"
		}
		// Build manually to avoid percent-encoding
		return u.Scheme + "://" + user + ":***@" + u.Host + u.RequestURI()
	}
	return rawURL
}

// SetPreProxy sets the pre-proxy URL for chain proxy mode.
func (pm *ProxyManager) SetPreProxy(preProxy string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.PreProxy = preProxy
	if preProxy != "" {
		log.Printf("[ProxyManager] Pre-proxy set: %s", preProxy)
	} else {
		log.Printf("[ProxyManager] Pre-proxy cleared")
	}
	// Persist to DB
	if pm.db != nil {
		pm.db.SetSetting("pre_proxy", preProxy)
	}
}

// GetPreProxy returns the current pre-proxy URL.
func (pm *ProxyManager) GetPreProxy() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.PreProxy
}

// bufferedConn wraps net.Conn with a bufio.Reader so data buffered by
// http.ReadResponse is not lost during subsequent reads.
type bufferedConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.br.Read(p)
}

// chainDial connects to a target through: pre_proxy (HTTP CONNECT) → SOCKS5 → target.
func chainDial(ctx context.Context, preProxyAddr, socksAddr string, socksAuth *proxy.Auth, targetAddr string) (net.Conn, error) {
	targetHost, targetPort, _ := net.SplitHostPort(targetAddr)

	// Step 1: TCP to pre_proxy
	dialTimeout := 15 * time.Second
	cfg := GlobalRateLimiter.GetConfig()
	if cfg.ConnectTimeoutSeconds > 0 {
		dialTimeout = time.Duration(cfg.ConnectTimeoutSeconds) * time.Second
	}
	d := net.Dialer{Timeout: dialTimeout}
	preConn, err := d.DialContext(ctx, "tcp", preProxyAddr)
	if err != nil {
		return nil, fmt.Errorf("pre_proxy dial %s: %w", preProxyAddr, err)
	}

	// Step 2: HTTP CONNECT tunnel to SOCKS5 server
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", socksAddr, socksAddr)
	if _, err := preConn.Write([]byte(connectReq)); err != nil {
		preConn.Close()
		return nil, fmt.Errorf("pre_proxy CONNECT write: %w", err)
	}
	br := bufio.NewReader(preConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		preConn.Close()
		return nil, fmt.Errorf("pre_proxy CONNECT response: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		preConn.Close()
		return nil, fmt.Errorf("pre_proxy CONNECT status %d", resp.StatusCode)
	}

	// Wrap so buffered data isn't lost
	tunnel := &bufferedConn{Conn: preConn, br: br}

	// Step 3: SOCKS5 handshake
	if err := socks5Handshake(tunnel, socksAuth, targetHost, targetPort); err != nil {
		tunnel.Close()
		return nil, err
	}
	return tunnel, nil
}

// socks5Handshake performs SOCKS5 greeting + auth + CONNECT on a raw connection.
func socks5Handshake(conn net.Conn, auth *proxy.Auth, targetHost, targetPort string) error {
	authMethod := byte(0x00) // no auth
	if auth != nil {
		authMethod = 0x02 // username/password
	}
	if _, err := conn.Write([]byte{0x05, 0x01, authMethod}); err != nil {
		return fmt.Errorf("socks5 greeting: %w", err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fmt.Errorf("socks5 greeting read: %w", err)
	}
	if buf[0] != 0x05 || buf[1] != authMethod {
		return fmt.Errorf("socks5 server rejected (got %x %x)", buf[0], buf[1])
	}

	if auth != nil {
		authPkt := make([]byte, 0, 3+len(auth.User)+len(auth.Password))
		authPkt = append(authPkt, 0x01, byte(len(auth.User)))
		authPkt = append(authPkt, []byte(auth.User)...)
		authPkt = append(authPkt, byte(len(auth.Password)))
		authPkt = append(authPkt, []byte(auth.Password)...)
		if _, err := conn.Write(authPkt); err != nil {
			return fmt.Errorf("socks5 auth write: %w", err)
		}
		if _, err := io.ReadFull(conn, buf[:2]); err != nil {
			return fmt.Errorf("socks5 auth read: %w", err)
		}
		if buf[1] != 0x00 {
			return fmt.Errorf("socks5 auth failed (status %d)", buf[1])
		}
	}

	// CONNECT
	port := 443
	fmt.Sscanf(targetPort, "%d", &port)
	connReq := make([]byte, 0, 7+len(targetHost))
	connReq = append(connReq, 0x05, 0x01, 0x00, 0x03, byte(len(targetHost)))
	connReq = append(connReq, []byte(targetHost)...)
	connReq = append(connReq, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(connReq); err != nil {
		return fmt.Errorf("socks5 connect write: %w", err)
	}

	resp := make([]byte, 4)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5 connect read: %w", err)
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("socks5 connect failed (status %d)", resp[1])
	}
	// Drain bound address
	switch resp[3] {
	case 0x01:
		io.ReadFull(conn, make([]byte, 6))
	case 0x03:
		lb := make([]byte, 1)
		io.ReadFull(conn, lb)
		io.ReadFull(conn, make([]byte, int(lb[0])+2))
	case 0x04:
		io.ReadFull(conn, make([]byte, 18))
	}
	return nil
}
