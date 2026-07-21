# mazaouest — v4 (Chromium local léger, "headless-shell", plan Render Free)

## Architecture

Noest chiffre la réponse de son endpoint interne `/get/scoring` (déchiffrée
seulement côté navigateur). Le login et la mise à jour du téléphone
(`PUT /update/orders/info`) fonctionnent toujours normalement via HTTP direct
et n'ont pas changé.

Pour lire le score, on a testé deux approches avant celle-ci :
1. Chromium complet dans Docker (Alpine) → a fait planter le plan Free par
   manque de RAM.
2. Navigateur distant Browserless.io (gratuit) → limité à 30 secondes de
   session sur le plan gratuit, ce qui coupait le flux avant qu'il ait fini.

**Cette version** utilise `chromedp/headless-shell` — une version de Chrome
spécifiquement allégée pour l'automatisation headless (pas de Chromium
desktop complet), qui tourne **dans le même conteneur** que l'appli Go, sans
dépendre d'un service externe ni de ses limites de temps. C'est beaucoup plus
léger en RAM que l'installation Alpine précédente, avec de bonnes chances de
tenir dans le plan Render **Free**.

Au démarrage du conteneur (`start.sh`) :
1. `headless-shell` démarre en tâche de fond, en écoute uniquement sur
   `127.0.0.1:9222` (pas exposé à l'extérieur du conteneur).
2. L'appli Go démarre ensuite et s'y connecte pour chaque requête `/scoring`.

## Déploiement sur Render

1. Remplace le contenu de ton repo par `main.go`, `go.mod`, `Dockerfile`,
   `start.sh`, `render.yaml` de ce dossier.
2. Le service doit être en type **Docker** (comme `mazaouest5-2` ou
   `mazaouest7` avant) — pas besoin de `CHROME_PATH`, `BROWSERLESS_TOKEN` ni
   `BROWSERLESS_WS_URL` cette fois, retire-les s'ils sont encore configurés.
3. Variables d'environnement à garder : `NOEST_EMAIL`, `NOEST_PASSWORD`,
   `API_BEARER`, `DEFAULT_TRACKING`, `UPSTREAM_BASE`, `ALLOWED_ORIGIN`, etc.
   (les mêmes que dans ton `.env` existant, moins celles de Browserless).
4. Plan **Free** — à tester en premier ; si `/scoring` fait planter le
   service par manque de mémoire malgré cette version plus légère, il faudra
   passer sur **Starter**, mais c'est notre meilleure chance de rester
   gratuit.

## Réponse de /scoring

```json
{
  "phone": "0555793595",
  "tracking": "OFA-45A-12045295",
  "probabilite": "Très élevée",
  "label_complet": "Probabilité de livraison Très élevée",
  "niveau": "excellent",
  "steps": { "login": true, "home_csrf": true, "order_update": true, "scoring": true }
}
```

## Limites connues

- Je n'ai pas d'accès réseau pour tester ce code en conditions réelles — si
  une erreur apparaît (le JSON `/scoring` inclut toujours `steps` + `error`
  précis pour localiser l'étape en cause), colle-la-moi et je corrige.
- Si le service redémarre pour cause de mémoire (message Render identique à
  celui vu précédemment), ce sera le signe que même cette version allégée ne
  tient pas dans les ~512 Mo du plan Free — dans ce cas, Starter (~7$/mois)
  restera la solution la plus fiable.
