package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

/*
Env you should set on Render (Docker deploy):
- UPSTREAM_BASE=https://app.noest-dz.com
- API_BEARER=... (optional; enables bearer gate)
- NOEST_EMAIL=...
- NOEST_PASSWORD=...
- DEFAULT_TRACKING=98K-19D-11050075
- DEFAULT_TYPE / DEFAULT_WILAYA / DEFAULT_COMMUNE / DEFAULT_ADRESSE / DEFAULT_CLIENT /
  DEFAULT_REMARQUE / DEFAULT_PRODUCT / DEFAULT_MONTANT / DEFAULT_STOP_DESK /
  DEFAULT_NOT_EXPIDIE / DEFAULT_POIDS / DEFAULT_ALT_PHONE : identiques à avant
- ORDERS_PAGE_PATH=/validation/orders   (page où le badge de scoring est affiché)
- CHROME_PATH=/usr/bin/chromium-browser (chemin du binaire Chromium dans le conteneur)

Optional path overrides (if upstream changes):
- LOGIN_PATH=/login
- LOGIN_PAGE_PATH=/login
- HOME_PATH=/home
- ORDER_UPDATE_PATH=/update/orders/info
*/

type Config struct {
	UpstreamBase  string
	APIBearer     string
	AllowedOrigin string
	Port          string

	NoestEmail    string
	NoestPassword string

	DefaultTracking   string
	DefaultType       string
	DefaultWilaya     string
	DefaultCommune    string
	DefaultAdresse    string
	DefaultClient     string
	DefaultRemarque   string
	DefaultProduct    string
	DefaultMontant    string
	DefaultStopDesk   string
	DefaultNotExpidie string
	DefaultPoids      string
	DefaultAltPhone   string

	LoginPath       string
	LoginPagePath   string
	HomePath        string
	OrderUpdatePath string
	OrdersPagePath  string

	ChromeDebugURL string
}

func getenv(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

func getConfig() Config {
	return Config{
		UpstreamBase:  getenv("UPSTREAM_BASE", "https://app.noest-dz.com"),
		APIBearer:     os.Getenv("API_BEARER"),
		AllowedOrigin: getenv("ALLOWED_ORIGIN", "*"),
		Port:          getenv("PORT", "8080"),

		NoestEmail:    os.Getenv("NOEST_EMAIL"),
		NoestPassword: os.Getenv("NOEST_PASSWORD"),

		DefaultTracking:   os.Getenv("DEFAULT_TRACKING"),
		DefaultType:       getenv("DEFAULT_TYPE", "1"),
		DefaultWilaya:     getenv("DEFAULT_WILAYA", "16"),
		DefaultCommune:    getenv("DEFAULT_COMMUNE", "Alger Centre"),
		DefaultAdresse:    getenv("DEFAULT_ADRESSE", "ALGER"),
		DefaultClient:     getenv("DEFAULT_CLIENT", "CLIENT"),
		DefaultRemarque:   getenv("DEFAULT_REMARQUE", "GIFT"),
		DefaultProduct:    getenv("DEFAULT_PRODUCT", "GIFT"),
		DefaultMontant:    getenv("DEFAULT_MONTANT", "1300.00"),
		DefaultStopDesk:   getenv("DEFAULT_STOP_DESK", "1"),
		DefaultNotExpidie: getenv("DEFAULT_NOT_EXPIDIE", "1"),
		DefaultPoids:      getenv("DEFAULT_POIDS", "2.00"),
		DefaultAltPhone:   os.Getenv("DEFAULT_ALT_PHONE"),

		LoginPath:       getenv("LOGIN_PATH", "/login"),
		LoginPagePath:   getenv("LOGIN_PAGE_PATH", "/login"),
		HomePath:        getenv("HOME_PATH", "/home"),
		OrderUpdatePath: getenv("ORDER_UPDATE_PATH", "/update/orders/info"),
		OrdersPagePath:  getenv("ORDERS_PAGE_PATH", "/validation/orders"),

		ChromeDebugURL: getenv("CHROME_DEBUG_URL", "http://127.0.0.1:9222"),
	}
}

func newHTTPClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout: 25 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Jar: jar,
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func rateLimit(r rate.Limiter) gin.HandlerFunc {
	tokens := make(map[string]*rate.Limiter)
	mu := make(chan struct{}, 1)
	getLimiter := func(ip string) *rate.Limiter {
		mu <- struct{}{}
		lim, ok := tokens[ip]
		if !ok {
			cp := r
			lim = rate.NewLimiter(cp.Limit(), cp.Burst())
			tokens[ip] = lim
		}
		<-mu
		return lim
	}
	return func(c *gin.Context) {
		if !getLimiter(clientIP(c.Request)).Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		c.Next()
	}
}

// --------- Parsers & helpers for tokens ----------

var loginTokenRe = regexp.MustCompile(`(?is)<input[^>]*name=['"]?_token['"]?[^>]*value=['"]([^'"]+)['"]`)
var metaCSRFRe = regexp.MustCompile(`(?is)<meta[^>]+name=['"]csrf-token['"][^>]*content=['"]([^'"]+)['"][^>]*>`)

func extractLoginToken(html []byte) (string, bool) {
	m := loginTokenRe.FindSubmatch(html)
	if len(m) < 2 {
		return "", false
	}
	return string(m[1]), true
}
func extractMetaCSRF(html []byte) (string, bool) {
	m := metaCSRFRe.FindSubmatch(html)
	if len(m) < 2 {
		return "", false
	}
	return string(m[1]), true
}
func cookieVal(j http.CookieJar, u *url.URL, name string) (string, bool) {
	for _, ck := range j.Cookies(u) {
		if ck.Name == name && ck.Value != "" {
			return ck.Value, true
		}
	}
	return "", false
}

// --------- Session (cookies + CSRF meta) ----------

type session struct {
	client     *http.Client
	csrfHeader string
	expiresAt  time.Time
}

var cached *session

func ensureSession(cfg Config) (*session, bool, bool, error) {
	if cached != nil && time.Now().Before(cached.expiresAt.Add(-5*time.Minute)) {
		return cached, true, true, nil
	}
	if cfg.NoestEmail == "" || cfg.NoestPassword == "" {
		return nil, false, false, errors.New("NOEST_EMAIL/NOEST_PASSWORD not set")
	}

	cl := newHTTPClient()
	base := strings.TrimRight(cfg.UpstreamBase, "/")
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/118.0.0.0 Safari/537.36"

	loginPageURL := base + cfg.LoginPagePath
	req0, _ := http.NewRequest(http.MethodGet, loginPageURL, nil)
	req0.Header.Set("User-Agent", ua)
	req0.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req0.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	resp0, err := cl.Do(req0)
	if err != nil {
		return nil, false, false, err
	}
	page, _ := io.ReadAll(resp0.Body)
	resp0.Body.Close()

	hidden, ok := extractLoginToken(page)
	if !ok {
		u0, _ := url.Parse(loginPageURL)
		if raw, ok2 := cookieVal(cl.Jar, u0, "XSRF-TOKEN"); ok2 {
			if dec, err := url.QueryUnescape(raw); err == nil && dec != "" {
				hidden, ok = dec, true
			}
		}
	}
	if !ok {
		return nil, false, false, errors.New("login _token not found in login page")
	}

	loginURL := base + cfg.LoginPath
	form := url.Values{
		"email":    {cfg.NoestEmail},
		"password": {cfg.NoestPassword},
		"_token":   {hidden},
	}
	req1, _ := http.NewRequest(http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	req1.Header.Set("User-Agent", ua)
	req1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req1.Header.Set("Accept", "*/*")
	req1.Header.Set("Origin", base)
	req1.Header.Set("Referer", loginPageURL)
	req1.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	resp1, err := cl.Do(req1)
	if err != nil {
		return nil, false, false, err
	}
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	loginOK := true

	homeURL := base + cfg.HomePath
	req2, _ := http.NewRequest(http.MethodGet, homeURL, nil)
	req2.Header.Set("User-Agent", ua)
	req2.Header.Set("Accept", "*/*")
	req2.Header.Set("Referer", base)
	req2.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	resp2, err := cl.Do(req2)
	if err != nil {
		return nil, loginOK, false, err
	}
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	csrf, ok := extractMetaCSRF(body)
	if !ok {
		return nil, loginOK, false, errors.New("csrf meta not found in /home")
	}
	homeOK := true

	cached = &session{client: cl, csrfHeader: csrf, expiresAt: time.Now().Add(110 * time.Minute)}
	return cached, loginOK, homeOK, nil
}

// --------- Upstream actions ----------

// PUT /update/orders/info (unaffected by the Noest scoring encryption change)
func updateOrderPhone(cfg Config, sess *session, formVals url.Values) error {
	u := strings.TrimRight(cfg.UpstreamBase, "/") + cfg.OrderUpdatePath
	req, _ := http.NewRequest(http.MethodPut, u, strings.NewReader(formVals.Encode()))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/139.0.0.0 Safari/537.36")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Csrf-Token", sess.csrfHeader)
	req.Header.Set("X-CSRF-TOKEN", sess.csrfHeader)
	req.Header.Set("Origin", strings.TrimRight(cfg.UpstreamBase, "/"))
	req.Header.Set("Referer", strings.TrimRight(cfg.UpstreamBase, "/")+cfg.HomePath)
	req.Header.Set("Accept-Language", "en-GB,en;q=0.9")

	resp, err := sess.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return errors.New("order update failed: " + resp.Status + " - " + string(b))
	}
	type upd struct {
		Update string `json:"update"`
	}
	var ok upd
	if err := json.Unmarshal(bytes.TrimSpace(b), &ok); err == nil && ok.Update != "" && !strings.EqualFold(ok.Update, "success") {
		return errors.New("order update unexpected response: " + string(b))
	}
	return nil
}

// --------- Scoring badge reading (chromedp) ----------

// stepCtx bounds a single chromedp step with its own timeout, derived from
// the long-lived browser context. Using one context with one big deadline
// for the whole flow meant that once it expired, *every* subsequent call —
// including diagnostic ones like reading the current URL/title after a
// failure — died instantly too (a dead context stays dead). Giving each step
// its own budget keeps diagnostics usable even when a previous step timed
// out.
//
// Deliberately NOT returning/calling an early cancel(): chromedp appears to
// tie the browser tab's lifecycle to whichever context was first used to
// drive it, so cancelling a short-lived per-step context as soon as that
// step finishes was closing the tab out from under later steps (seen as
// "context canceled" on the very next action). The parent browser context
// (bctx in the handler) still gets cancelled exactly once, at the end of the
// request, which cleans all of these up together.
func stepCtx(parent context.Context, d time.Duration) context.Context {
	ctx, _ := context.WithTimeout(parent, d)
	return ctx
}

// browserLogin logs into Noest directly inside the remote browser (fills the
// real login form), instead of trying to transplant cookies from the Go HTTP
// session — that transplant proved fragile (cookie attributes don't always
// survive the trip, so Noest kept showing the login page instead of orders).
//
// Each step runs as its own chromedp.Run call (with its own timeout) and
// returns a distinct error, so that if something hangs/fails we know exactly
// which step it was — including, when the email field never appears, the
// URL/title actually reached (helps tell "still loading" from "wrong
// selector" from "redirected somewhere unexpected").
func browserLogin(ctx context.Context, cfg Config) error {
	loginURL := strings.TrimRight(cfg.UpstreamBase, "/") + cfg.LoginPagePath

	// WaitVisible's internal polling turned out to be the actual culprit
	// (confirmed via /debug-login-shot: the exact same page, with the exact
	// same selector, renders correctly and quickly with a plain Navigate +
	// Sleep — WaitVisible was the only thing that never returned). Using
	// that proven-working approach here instead.
	if err := chromedp.Run(stepCtx(ctx, 15*time.Second),
		chromedp.Navigate(loginURL),
		chromedp.Sleep(4*time.Second),
	); err != nil {
		return fmt.Errorf("navigate to login page: %w", err)
	}

	if err := chromedp.Run(stepCtx(ctx, 10*time.Second),
		chromedp.SendKeys(`input[name="email"]`, cfg.NoestEmail, chromedp.ByQuery),
		chromedp.SendKeys(`input[name="password"]`, cfg.NoestPassword, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("filling login form: %w", err)
	}

	if err := chromedp.Run(stepCtx(ctx, 15*time.Second), chromedp.Submit(`input[name="password"]`, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("submitting login form: %w", err)
	}

	if err := chromedp.Run(stepCtx(ctx, 5*time.Second), chromedp.Sleep(2500*time.Millisecond)); err != nil {
		return fmt.Errorf("post-submit wait: %w", err)
	}
	return nil
}

// readScoringBadge loads the orders page (already authenticated via cookies)
// and reads the data-scoring-label attribute that Noest's own JS decrypts
// and renders client-side for the given tracking number.
func readScoringBadge(ctx context.Context, cfg Config, tracking string) (label string, level string, err error) {
	ordersURL := strings.TrimRight(cfg.UpstreamBase, "/") + cfg.OrdersPagePath

	if err = chromedp.Run(stepCtx(ctx, 15*time.Second),
		chromedp.Navigate(ordersURL),
		chromedp.Sleep(4*time.Second),
	); err != nil {
		return "", "", fmt.Errorf("navigate to orders page: %w", err)
	}

	if err = chromedp.Run(stepCtx(ctx, 5*time.Second), chromedp.Sleep(1500*time.Millisecond)); err != nil {
		return "", "", fmt.Errorf("post-load wait: %w", err)
	}

	readCtx := stepCtx(ctx, 10*time.Second)

	// Prefer the badge inside the row that mentions this tracking number.
	rowSel := fmt.Sprintf(`//tr[.//*[contains(., %q)]]//span[@data-scoring-level]`, tracking)
	err = chromedp.Run(readCtx,
		chromedp.AttributeValue(rowSel, "data-scoring-label", &label, nil, chromedp.BySearch),
	)
	if err != nil || label == "" {
		// Fallback: only one order on the page (single-template workflow) -> take the first badge.
		if err2 := chromedp.Run(readCtx,
			chromedp.AttributeValue(`span[data-scoring-level]`, "data-scoring-label", &label, nil, chromedp.ByQuery),
		); err2 != nil {
			return "", "", fmt.Errorf("read badge label: %w", err2)
		}
	}

	_ = chromedp.Run(readCtx,
		chromedp.AttributeValue(`span[data-scoring-level]`, "data-scoring-level", &level, nil, chromedp.ByQuery),
	)

	if label == "" {
		return "", "", errors.New("scoring badge not found on orders page")
	}
	return label, level, nil
}

// newBrowserContext connects to the local headless-shell instance running in
// the same container (started by start.sh before this Go binary launches).
// Using chromedp's HTTP discovery here (not NoModifyURL) is correct — unlike
// Browserless's shared-fleet gateway, a local Chrome DevTools endpoint really
// does answer GET /json/version with proper JSON.
func newBrowserContext(cfg Config) (context.Context, context.CancelFunc) {
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), cfg.ChromeDebugURL)
	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	return ctx, func() { cancelCtx(); cancelAlloc() }
}

// --------- Main server ----------

var phoneRe = regexp.MustCompile(`^[0-9+]{6,20}$`)

// extractProbability strips the "Probabilité de livraison " prefix if present,
// returning just the level word(s) Noest displays (e.g. "Très élevée").
func extractProbability(label string) string {
	const prefix = "Probabilité de livraison "
	if strings.HasPrefix(label, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(label, prefix))
	}
	return label
}

func main() {
	cfg := getConfig()

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", cfg.AllowedOrigin)
		c.Writer.Header().Set("Vary", "Origin")
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})

	// TEMPORARY diagnostic route — registered before the bearer middleware
	// below, so it's NOT auth-protected (Gin only attaches middlewares
	// registered via r.Use() so far to routes declared after them). Checks
	// whether the local headless-shell instance is actually reachable,
	// independent of any of our own chromedp/login logic.
	// Remove this route once things are stable.
	r.GET("/chrome", func(c *gin.Context) {
		resp, err := http.Get(cfg.ChromeDebugURL + "/json/version")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		c.Data(resp.StatusCode, "application/json", body)
	})

	// TEMPORARY diagnostic route — actually screenshots whatever page Chrome
	// lands on after navigating to the login page and waiting a few seconds.
	// Lets us literally see what's being rendered (e.g. a bot-detection
	// interstitial that never resolves) instead of guessing from error text.
	// Remove once things are stable.
	r.GET("/debug-login-shot", func(c *gin.Context) {
		bctx, cancel := newBrowserContext(cfg)
		defer cancel()

		loginURL := strings.TrimRight(cfg.UpstreamBase, "/") + cfg.LoginPagePath
		var buf []byte
		var curURL, curTitle, htmlSrc string

		shotCtx, shotCancel := context.WithTimeout(bctx, 30*time.Second)
		defer shotCancel()

		err := chromedp.Run(shotCtx,
			chromedp.Navigate(loginURL),
			chromedp.Sleep(6*time.Second), // let any JS challenge/render finish
			chromedp.Location(&curURL),
			chromedp.Title(&curTitle),
			chromedp.OuterHTML("html", &htmlSrc),
			chromedp.CaptureScreenshot(&buf),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": err.Error(), "url": curURL, "title": curTitle,
			})
			return
		}

		if c.Query("html") == "1" {
			c.Header("X-Landed-URL", curURL)
			c.Header("X-Landed-Title", curTitle)
			c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(htmlSrc))
			return
		}

		c.Header("X-Landed-URL", curURL)
		c.Header("X-Landed-Title", curTitle)
		c.Data(http.StatusOK, "image/png", buf)
	})

	r.Use(func(c *gin.Context) {
		if cfg.APIBearer == "" {
			return
		}
		token := ""
		if h := c.GetHeader("Authorization"); h != "" {
			parts := strings.SplitN(h, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				token = strings.TrimSpace(parts[1])
			}
		}
		if token == "" {
			token = strings.TrimSpace(c.Query("bearer"))
		}
		want := strings.TrimSpace(cfg.APIBearer)
		if token != want {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
	})

	r.Use(rateLimit(*rate.NewLimiter(5, 20)))

	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	r.GET("/scoring", func(c *gin.Context) {
		type Steps struct {
			Login       bool `json:"login"`
			HomeCSRF    bool `json:"home_csrf"`
			OrderUpdate bool `json:"order_update"`
			Scoring     bool `json:"scoring"`
		}
		steps := Steps{}

		phone := strings.TrimSpace(c.Query("phone"))
		if phone == "" || !phoneRe.MatchString(phone) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or missing phone", "steps": steps})
			return
		}
		alt := strings.TrimSpace(c.Query("alt"))
		if alt == "" {
			alt = strings.TrimSpace(cfg.DefaultAltPhone)
		}

		tracking := strings.TrimSpace(c.Query("tracking"))
		if tracking == "" {
			tracking = strings.TrimSpace(cfg.DefaultTracking)
		}
		if tracking == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tracking missing (set DEFAULT_TRACKING or pass ?tracking=)", "steps": steps})
			return
		}

		sess, loginOK, homeOK, err := ensureSession(cfg)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "step": "session", "steps": steps})
			return
		}
		steps.Login, steps.HomeCSRF = loginOK, homeOK

		form := url.Values{
			"tracking":    {tracking},
			"type":        {cfg.DefaultType},
			"wilaya":      {cfg.DefaultWilaya},
			"commune":     {cfg.DefaultCommune},
			"adresse":     {cfg.DefaultAdresse},
			"client":      {cfg.DefaultClient},
			"tel":         {phone},
			"tel2":        {alt},
			"remarque":    {cfg.DefaultRemarque},
			"product":     {cfg.DefaultProduct},
			"montant":     {cfg.DefaultMontant},
			"stop_desk":   {cfg.DefaultStopDesk},
			"not_expidie": {cfg.DefaultNotExpidie},
			"poids":       {cfg.DefaultPoids},
		}
		if err := updateOrderPhone(cfg, sess, form); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "step": "order_update", "steps": steps})
			return
		}
		steps.OrderUpdate = true

		bctx, cancel := newBrowserContext(cfg)
		defer cancel()

		if err := browserLogin(bctx, cfg); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "browser login failed: " + err.Error(), "step": "scoring", "steps": steps})
			return
		}

		label, level, err := readScoringBadge(bctx, cfg, tracking)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "step": "scoring", "steps": steps})
			return
		}
		steps.Scoring = true

		c.JSON(http.StatusOK, gin.H{
			"phone":        phone,
			"tracking":     tracking,
			"probabilite":  extractProbability(label),
			"label_complet": label,
			"niveau":       level,
			"steps":        steps,
		})
	})

	log.Printf("listening on :%s (upstream=%s)", cfg.Port, cfg.UpstreamBase)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
