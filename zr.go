// zr.go — Scoring ZR Express (ReturnMeter) pour mazaouest4
//
// Flux de GET /zr/scoring?phone=0551234567 :
//   1. change le téléphone du colis modèle via l'API publique ZR (X-Api-Key + X-Tenant)
//   2. obtient un jeton de session ZR (connexion e-mail/mot de passe via Clerk, réutilisée ensuite)
//   3. lit le ReturnMeter via la route interne /parcels/portal/search
//
// Variables d'environnement Render :
//   ZR_API_KEY          clé secrète ZR (page "Jetons API")
//   ZR_TENANT_ID        Tenant ID ZR
//   ZR_EMAIL            e-mail de connexion app.zrexpress.app
//   ZR_PASSWORD         mot de passe de connexion
//   ZR_MODEL_TRACKING   tracking du colis modèle (ex. 01-B94E7I3ZZ9-ZR)
//   (auth : le middleware API_BEARER de main.go protège déjà /zr/* ; ZR_ENDPOINT_TOKEN optionnel)
//   ZR_CACHE_HOURS      (optionnel) durée du cache par numéro, défaut 24
//   ZR_SETTLE_MS        (optionnel) attente après changement de téléphone, défaut 1500

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	zrPublicAPI = "https://api.zrexpress.app/api/v1"
	zrPortalAPI = "https://app.zrexpress.app/api/v1.0"
	zrClerkFAPI = "https://clerk.app.zrexpress.app/v1"
	zrOrigin    = "https://app.zrexpress.app"
	zrClerkVer  = "__clerk_api_version=2025-11-10"
)

type ZRReturnMeter struct {
	Score           int    `json:"score"`
	Level           string `json:"level"`
	CompletedOrders int    `json:"completedOrders"`
	Returns         int    `json:"returns"`
	ComputedAt      string `json:"computedAt"`
	WindowMonths    int    `json:"windowMonths"`
	LowMax          int    `json:"lowMax"`
	HighMin         int    `json:"highMin"`
}

type zrResult struct {
	Phone       string         `json:"phone"`
	ReturnMeter *ZRReturnMeter `json:"returnMeter"`
	Label       string         `json:"label"`
	Cached      bool           `json:"cached"`
	CheckedAt   time.Time      `json:"checkedAt"`
}

type zrClient struct {
	mu        sync.Mutex // une seule vérification à la fois (un seul colis modèle)
	http      *http.Client
	jar       *cookiejar.Jar
	sessionID string
	jwt       string
	jwtExp    time.Time
	parcelID  string
	cache     map[string]zrResult
	cacheMu   sync.RWMutex
}

var zr = newZRClient()

func newZRClient() *zrClient {
	jar, _ := cookiejar.New(nil)
	return &zrClient{
		http:  &http.Client{Timeout: 25 * time.Second, Jar: jar},
		jar:   jar,
		cache: map[string]zrResult{},
	}
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

// ---------- Normalisation téléphone ----------

var nonDigit = regexp.MustCompile(`\D`)

// Retourne le format local 10 chiffres (05/06/07…), ou une erreur.
func zrNormalizePhone(raw string) (string, error) {
	d := nonDigit.ReplaceAllString(raw, "")
	switch {
	case strings.HasPrefix(d, "00213"):
		d = "0" + d[5:]
	case strings.HasPrefix(d, "213") && len(d) == 12:
		d = "0" + d[3:]
	case len(d) == 9:
		d = "0" + d
	}
	if len(d) != 10 || d[0] != '0' {
		return "", fmt.Errorf("numéro invalide: %q", raw)
	}
	return d, nil
}

// ---------- API publique (clé API) ----------

func (c *zrClient) publicReq(method, path string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, zrPublicAPI+path, rd)
	req.Header.Set("X-Api-Key", os.Getenv("ZR_API_KEY"))
	req.Header.Set("X-Tenant", os.Getenv("ZR_TENANT_ID"))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return b, res.StatusCode, nil
}

type zrParcel struct {
	ID       string `json:"id"`
	Customer struct {
		Name  string `json:"name"`
		Phone struct {
			Number1 string `json:"number1"`
		} `json:"phone"`
	} `json:"customer"`
	State struct {
		Name string `json:"name"`
	} `json:"state"`
	PhoneChangeCount        int  `json:"phoneChangeCount"`
	PhoneChangePaymentCount int  `json:"phoneChangePaymentCount"`
	IsEligibleForModif      bool `json:"isEligibleForModification"`
}

func (c *zrClient) getModelParcel() (*zrParcel, error) {
	b, st, err := c.publicReq("GET", "/parcels/"+url.PathEscape(os.Getenv("ZR_MODEL_TRACKING")), nil)
	if err != nil {
		return nil, err
	}
	if st != 200 {
		return nil, fmt.Errorf("lecture colis modèle: HTTP %d %s", st, trunc(b))
	}
	var p zrParcel
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	c.parcelID = p.ID
	return &p, nil
}

func (c *zrClient) setModelPhone(local string) error {
	p, err := c.getModelParcel()
	if err != nil {
		return err
	}
	// Sécurité : on ne touche jamais un colis expédié (le changement deviendrait payant).
	if p.State.Name != "commande_recue" {
		return fmt.Errorf("colis modèle dans l'état %q, changement refusé (risque de frais)", p.State.Name)
	}
	if strings.HasSuffix(p.Customer.Phone.Number1, local[1:]) {
		return nil // déjà le bon numéro
	}
	body := map[string]any{"parcelId": p.ID, "name": p.Customer.Name, "phone": local}
	b, st, err := c.publicReq("PATCH", "/parcels/"+p.ID+"/customer", body)
	if err != nil {
		return err
	}
	if st >= 300 {
		return fmt.Errorf("changement téléphone: HTTP %d %s", st, trunc(b))
	}
	return nil
}

// ---------- Session ZR (Clerk) ----------

type clerkClientResp struct {
	Response json.RawMessage `json:"response"`
	Client   struct {
		Sessions []struct {
			ID              string `json:"id"`
			Status          string `json:"status"`
			LastActiveToken struct {
				JWT string `json:"jwt"`
			} `json:"last_active_token"`
		} `json:"sessions"`
	} `json:"client"`
	Errors []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		LongMsg string `json:"long_message"`
	} `json:"errors"`
}

func (c *zrClient) clerkPost(path string, form url.Values) (*clerkClientResp, int, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, _ := http.NewRequest("POST", zrClerkFAPI+path+sep+zrClerkVer, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", zrOrigin)
	req.Header.Set("Referer", zrOrigin+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var r clerkClientResp
	_ = json.Unmarshal(b, &r)
	if len(r.Errors) > 0 {
		e := r.Errors[0]
		return &r, res.StatusCode, fmt.Errorf("clerk %s: %s %s", e.Code, e.Message, e.LongMsg)
	}
	if res.StatusCode >= 300 {
		return &r, res.StatusCode, fmt.Errorf("clerk HTTP %d %s", res.StatusCode, trunc(b))
	}
	return &r, res.StatusCode, nil
}

func (c *zrClient) login() error {
	c.sessionID, c.jwt = "", ""
	form := url.Values{
		"identifier": {os.Getenv("ZR_EMAIL")},
		"strategy":   {"password"},
		"password":   {os.Getenv("ZR_PASSWORD")},
	}
	r, _, err := c.clerkPost("/client/sign_ins", form)
	if err != nil {
		return fmt.Errorf("connexion ZR: %w", err)
	}
	for _, s := range r.Client.Sessions {
		if s.Status == "active" {
			c.sessionID = s.ID
			c.jwt = s.LastActiveToken.JWT
			c.jwtExp = time.Now().Add(45 * time.Second)
		}
	}
	if c.sessionID == "" {
		return errors.New("connexion ZR: aucune session active (vérification supplémentaire demandée ?)")
	}
	return nil
}

func (c *zrClient) token() (string, error) {
	if c.jwt != "" && time.Now().Before(c.jwtExp) {
		return c.jwt, nil
	}
	if c.sessionID != "" {
		req, _ := http.NewRequest("POST", zrClerkFAPI+"/client/sessions/"+c.sessionID+"/tokens?"+zrClerkVer, nil)
		req.Header.Set("Origin", zrOrigin)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if res, err := c.http.Do(req); err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			var t struct {
				JWT string `json:"jwt"`
			}
			if res.StatusCode == 200 && json.Unmarshal(b, &t) == nil && t.JWT != "" {
				c.jwt, c.jwtExp = t.JWT, time.Now().Add(45*time.Second)
				return c.jwt, nil
			}
		}
	}
	if err := c.login(); err != nil {
		return "", err
	}
	return c.jwt, nil
}

// ---------- Lecture du ReturnMeter (route interne) ----------

func (c *zrClient) readReturnMeter() (*ZRReturnMeter, string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.token()
		if err != nil {
			return nil, "", err
		}
		body, _ := json.Marshal(map[string]any{"pageNumber": 1, "pageSize": 5, "keyword": os.Getenv("ZR_MODEL_TRACKING")})
		req, _ := http.NewRequest("POST", zrPortalAPI+"/parcels/portal/search", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Tenant", os.Getenv("ZR_TENANT_ID"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		res, err := c.http.Do(req)
		if err != nil {
			return nil, "", err
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode == 401 {
			c.jwt, c.sessionID = "", "" // session expirée → reconnexion
			continue
		}
		if res.StatusCode != 200 {
			return nil, "", fmt.Errorf("portal search: HTTP %d %s", res.StatusCode, trunc(b))
		}
		var out struct {
			Items []struct {
				TrackingNumber string `json:"trackingNumber"`
				Customer       struct {
					Phone struct {
						Number1 string `json:"number1"`
					} `json:"phone"`
				} `json:"customer"`
				ReturnMeter *ZRReturnMeter `json:"returnMeter"`
			} `json:"items"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, "", err
		}
		for _, it := range out.Items {
			if it.TrackingNumber == os.Getenv("ZR_MODEL_TRACKING") {
				return it.ReturnMeter, it.Customer.Phone.Number1, nil
			}
		}
		return nil, "", errors.New("colis modèle introuvable dans portal search")
	}
	return nil, "", errors.New("session ZR refusée après reconnexion")
}

// ---------- Orchestration ----------

func zrLabel(rm *ZRReturnMeter) string {
	if rm == nil || rm.Level == "" {
		return "Aucune information"
	}
	switch strings.ToLower(rm.Level) {
	case "low":
		return "Risque de retour faible"
	case "medium":
		return "Risque de retour moyen"
	case "high":
		return "Risque de retour élevé"
	}
	return rm.Level
}

func (c *zrClient) Check(rawPhone string, force bool) (zrResult, error) {
	local, err := zrNormalizePhone(rawPhone)
	if err != nil {
		return zrResult{}, err
	}
	ttl := time.Duration(envInt("ZR_CACHE_HOURS", 24)) * time.Hour
	if !force {
		c.cacheMu.RLock()
		r, ok := c.cache[local]
		c.cacheMu.RUnlock()
		if ok && time.Since(r.CheckedAt) < ttl {
			r.Cached = true
			return r, nil
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.setModelPhone(local); err != nil {
		return zrResult{}, err
	}
	time.Sleep(time.Duration(envInt("ZR_SETTLE_MS", 1500)) * time.Millisecond)

	var rm *ZRReturnMeter
	for i := 0; i < 3; i++ { // attend que le colis porte bien le nouveau numéro
		var phone string
		rm, phone, err = c.readReturnMeter()
		if err != nil {
			return zrResult{}, err
		}
		if strings.HasSuffix(phone, local[1:]) {
			break
		}
		time.Sleep(1500 * time.Millisecond)
	}

	r := zrResult{Phone: local, ReturnMeter: rm, Label: zrLabel(rm), CheckedAt: time.Now()}
	c.cacheMu.Lock()
	c.cache[local] = r
	c.cacheMu.Unlock()
	return r, nil
}

func trunc(b []byte) string {
	s := string(b)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// ---------- Routes Gin ----------

func zrAuth(ctx *gin.Context) bool {
	want := os.Getenv("ZR_ENDPOINT_TOKEN")
	if want == "" {
		return true // protégé par le middleware API_BEARER global de main.go
	}
	got := strings.TrimPrefix(ctx.GetHeader("Authorization"), "Bearer ")
	if got == "" {
		got = ctx.Query("bearer")
	}
	return want != "" && got == want
}

// RegisterZRRoutes : à appeler dans main() → RegisterZRRoutes(r)
func RegisterZRRoutes(r *gin.Engine) {
	r.GET("/zr/scoring", func(ctx *gin.Context) {
		if !zrAuth(ctx) {
			ctx.JSON(401, gin.H{"success": false, "error": "non autorisé"})
			return
		}
		res, err := zr.Check(ctx.Query("phone"), ctx.Query("force") == "1")
		if err != nil {
			ctx.JSON(502, gin.H{"success": false, "error": err.Error()})
			return
		}
		ctx.JSON(200, gin.H{"success": true, "data": res})
	})

	// Diagnostic sans modification : état du colis modèle + connexion + score actuel
	r.GET("/zr/health", func(ctx *gin.Context) {
		if !zrAuth(ctx) {
			ctx.JSON(401, gin.H{"success": false, "error": "non autorisé"})
			return
		}
		zr.mu.Lock()
		defer zr.mu.Unlock()
		out := gin.H{}
		if p, err := zr.getModelParcel(); err != nil {
			out["apiKey"] = "ERREUR: " + err.Error()
		} else {
			out["apiKey"] = "OK"
			out["modelState"] = p.State.Name
			out["modelPhone"] = p.Customer.Phone.Number1
			out["phoneChangeCount"] = p.PhoneChangeCount
			out["phoneChangePaymentCount"] = p.PhoneChangePaymentCount
		}
		if rm, _, err := zr.readReturnMeter(); err != nil {
			out["session"] = "ERREUR: " + err.Error()
		} else {
			out["session"] = "OK"
			out["returnMeter"] = rm
		}
		ctx.JSON(200, out)
	})
}
