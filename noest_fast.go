package main

// noest_fast.go — accélération du scoring Noest :
//   - va directement sur la page des commandes (pas de détour par /login quand Chrome est déjà connecté)
//   - ne se connecte que si Noest affiche le formulaire de connexion
//   - remplace les attentes fixes (≈10 s) par une attente active du badge (vérifié toutes les 250 ms)
//   - cache des résultats par numéro (NOEST_CACHE_HOURS, défaut 24 ; ?force=1 pour ignorer)
//   - une seule vérification Noest à la fois (un seul colis modèle)

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

var noestMu sync.Mutex

type noestCached struct {
	Label string
	Level string
	At    time.Time
}

var (
	noestCache   = map[string]noestCached{}
	noestCacheMu sync.RWMutex
)

func noestCacheGet(phone string) (noestCached, bool) {
	ttl := time.Duration(envInt("NOEST_CACHE_HOURS", 24)) * time.Hour
	noestCacheMu.RLock()
	defer noestCacheMu.RUnlock()
	c, ok := noestCache[phone]
	if !ok || time.Since(c.At) > ttl {
		return noestCached{}, false
	}
	return c, true
}

func noestCacheSet(phone, label, level string) {
	noestCacheMu.Lock()
	noestCache[phone] = noestCached{Label: label, Level: level, At: time.Now()}
	noestCacheMu.Unlock()
}

// fastScoring : tout dans un seul chromedp.Run (contrainte connue du projet).
func fastScoring(ctx context.Context, cfg Config, tracking string) (string, string, error) {
	base := strings.TrimRight(cfg.UpstreamBase, "/")
	ordersURL := base + cfg.OrdersPagePath
	loginURL := base + cfg.LoginPagePath

	trackJSON, _ := json.Marshal(tracking)
	// Renvoie "label|level" quand le badge de la ligne du colis modèle est rendu, sinon "".
	probe := fmt.Sprintf(`(() => {
		const t = %s;
		const rows = [...document.querySelectorAll('tr')].filter(r => r.textContent.includes(t));
		const pick = (s) => s && s.dataset.scoringLabel ? s.dataset.scoringLabel + '|' + (s.dataset.scoringLevel || '') : '';
		for (const r of rows) { const v = pick(r.querySelector('span[data-scoring-level]')); if (v) return v; }
		if (!rows.length) { const all = document.querySelectorAll('span[data-scoring-level]'); if (all.length === 1) return pick(all[0]); }
		return '';
	})()`, trackJSON)

	var label, level string

	waitFor := func(ctx context.Context, js string, max time.Duration) (string, error) {
		deadline := time.Now().Add(max)
		for {
			var out string
			if err := chromedp.Evaluate(js, &out).Do(ctx); err == nil && out != "" {
				return out, nil
			}
			if time.Now().After(deadline) {
				return "", errors.New("délai dépassé")
			}
			if err := chromedp.Sleep(250 * time.Millisecond).Do(ctx); err != nil {
				return "", err
			}
		}
	}

	// "login" si le formulaire est là, "orders" si la page des commandes est chargée
	stateJS := `(() => document.querySelector('input[name="email"]') ? 'login'
		: (document.readyState !== 'loading' && document.querySelector('table, tr')) ? 'orders' : '')()`

	flow := chromedp.ActionFunc(func(ctx context.Context) error {
		t0 := time.Now()
		if err := chromedp.Navigate(ordersURL).Do(ctx); err != nil {
			return fmt.Errorf("navigate orders: %w", err)
		}
		state, err := waitFor(ctx, stateJS, 15*time.Second)
		if err != nil {
			return fmt.Errorf("page commandes: %w", err)
		}

		if state == "login" {
			log.Printf("[fast] session Chrome expirée, connexion")
			if err := chromedp.Navigate(loginURL).Do(ctx); err != nil {
				return fmt.Errorf("navigate login: %w", err)
			}
			if _, err := waitFor(ctx, `document.querySelector('input[name="password"]') ? 'ok' : ''`, 10*time.Second); err != nil {
				return fmt.Errorf("formulaire connexion: %w", err)
			}
			for _, a := range []chromedp.Action{
				chromedp.SendKeys(`input[name="email"]`, cfg.NoestEmail, chromedp.ByQuery),
				chromedp.SendKeys(`input[name="password"]`, cfg.NoestPassword, chromedp.ByQuery),
				chromedp.Submit(`input[name="password"]`, chromedp.ByQuery),
			} {
				if err := a.Do(ctx); err != nil {
					return fmt.Errorf("connexion: %w", err)
				}
			}
			if _, err := waitFor(ctx, `document.querySelector('input[name="email"]') ? '' : 'ok'`, 15*time.Second); err != nil {
				return fmt.Errorf("après connexion: %w", err)
			}
			if err := chromedp.Navigate(ordersURL).Do(ctx); err != nil {
				return fmt.Errorf("navigate orders (2): %w", err)
			}
		}

		v, err := waitFor(ctx, probe, 20*time.Second)
		if err != nil {
			return fmt.Errorf("badge introuvable: %w", err)
		}
		parts := strings.SplitN(v, "|", 2)
		label = parts[0]
		if len(parts) > 1 {
			level = parts[1]
		}
		log.Printf("[fast] badge lu en %v : %q (%s)", time.Since(t0), label, level)
		return nil
	})

	if err := runStep(stepCtx(ctx, 60*time.Second), "fast_scoring", flow); err != nil {
		return "", "", err
	}
	return label, level, nil
}
