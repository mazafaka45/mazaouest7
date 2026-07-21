# mazaouest4 — v3 (lecture du score via navigateur distant Browserless.io)

## Architecture

Noest a chiffré la réponse de son endpoint interne `/get/scoring` (déchiffrée
seulement côté navigateur). Le login et la mise à jour du téléphone
(`PUT /update/orders/info`) fonctionnent toujours normalement et n'ont pas changé.

Donc dans cette version :
- Login + mise à jour du téléphone : HTTP direct, comme avant (`ensureSession`,
  `updateOrderPhone`).
- Lecture du score : le service Go se connecte à un **Chromium hébergé par
  Browserless.io** (gratuit jusqu'à 1000 unités/mois) via websocket, au lieu
  d'en lancer un localement. Ton service Render reste en Go pur, léger,
  **compatible avec le plan Free**. Le navigateur distant charge
  `/validation/orders` (avec les cookies de la session déjà connectée) et lit
  l'attribut `data-scoring-label` du badge déjà déchiffré par le JS de Noest.

## Mise en place de Browserless.io

1. Crée un compte gratuit sur https://www.browserless.io
2. Récupère ton **API token** dans leur dashboard
3. Ajoute-le comme variable d'environnement sur Render : `BROWSERLESS_TOKEN`
4. `BROWSERLESS_WS_URL` a une valeur par défaut (`wss://production-sfo.browserless.io`)
   — à ajuster uniquement si Browserless te donne une autre région/URL dans
   ton dashboard.

## Déploiement sur Render

Retour à un déploiement **Go natif** (comme au tout début) :

1. Remplace le contenu de ton repo par `main.go`, `go.mod`, `render.yaml`
   de ce dossier (plus de `Dockerfile` à ce stade).
2. Si ton service Render actuel est en Docker, recrée-le en type **Go** (ou
   repars du service Go d'origine, `mazaouest4`) — l'environnement d'un
   service existant ne peut pas être changé après coup sur Render.
3. Variables d'environnement à définir : `NOEST_EMAIL`, `NOEST_PASSWORD`,
   `API_BEARER`, `DEFAULT_TRACKING`, `BROWSERLESS_TOKEN`, etc. (voir ton
   fichier `.env` existant, il reste valable + `BROWSERLESS_TOKEN` en plus).
4. Plan **Free** suffit — il n'y a plus de Chromium local à faire tourner.

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

- Le free tier Browserless (1000 unités/mois, 2 sessions simultanées) doit
  largement suffire pour un usage ponctuel — au-delà, ça passe en payant
  (à partir de ~25$/mois).
- Je n'ai pas d'accès réseau pour tester ce code en conditions réelles contre
  Noest ni Browserless — si une erreur apparaît (le JSON `/scoring` inclut
  toujours `steps` + `error` précis), colle-la-moi et je corrige.
