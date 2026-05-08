# Agent Mesh — Documentation technique

## En une phrase

Agent Mesh est un **proxy sidecar** qui s'insère entre les agents IA et leurs outils pour ajouter politique d'accès, approbation humaine et traçabilité — sans modifier le code des agents.

Un binaire. Un fichier YAML. Politique fermée par défaut.

---

## Le problème

Les agents IA (Claude Code, Cursor, LangChain, CrewAI) se connectent directement à des outils : système de fichiers, APIs, bases de données, emails, CLI. Sans couche intermédiaire :

- **Pas de politique** — l'agent peut tout faire, y compris supprimer des fichiers ou envoyer des emails
- **Pas de trace** — impossible de savoir ce que l'agent a fait, quand, avec quels paramètres
- **Pas de contrôle** — si l'agent boucle ou hallucine, aucun garde-fou
- **Pas d'identité** — impossible de distinguer quel agent a fait quoi dans un contexte multi-agent

C'est l'équivalent de donner un accès root à un script non audité.

## La solution

Agent Mesh se place entre l'agent et ses outils. L'agent voit une surface d'outils normale. Le proxy applique les règles et trace chaque appel.

```
Agent IA ──> agent-mesh ──> filesystem (lecture: autorisé, écriture: approbation, suppression: interdit)
                       ├──> gmail      (lecture: autorisé, envoi: approbation, suppression: interdit)
                       ├──> météo      (autorisé)
                       └──> terraform  (plan: autorisé, apply: approbation, destroy: interdit)
                          │
                    politique · approbation · trace
```

### Analogie

C'est le même pattern qu'**Envoy** dans le monde des microservices. Envoy se place entre les services pour ajouter observabilité, authentification et rate limiting sans modifier le code des services. Agent Mesh fait la même chose pour les agents IA et leurs outils.

| Service mesh (Envoy) | Agent mesh |
|----------------------|------------|
| Service A → Service B | Agent → Outil |
| mTLS, authz, rate limit | Policy, approval, rate limit |
| Traces distribuées | Traces par appel d'outil |
| Sidecar proxy | Sidecar proxy |

---

## Architecture

```
                       agent-mesh (sidecar proxy)
                ┌──────────────────────────────────────────┐
                │                                          │
Agent IA ──────>│  Registre ──> Politique ──> Forward ─────│──> Outils
          <─────│  (outils)     (règles)     (proxy)  <────│──<  (MCP, HTTP, CLI)
                │                   │            │         │
                │              Approbation   Trace         │
                │              (humain/auto) (JSONL)       │
                └──────────────────────────────────────────┘
```

### Flux d'un appel

Chaque appel d'outil suit le même chemin, quel que soit le transport :

1. **Identification** — extraction de l'identité agent (`Authorization: Bearer agent:<nom>`)
2. **Rate limit** — vérification appels/min, budget total, détection de boucle
3. **Registre** — recherche de l'outil dans le catalogue
4. **Politique** — évaluation des règles (autorisé / interdit / approbation requise)
5. **Approbation** — si nécessaire, attente de validation humaine ou automatique
6. **Forward** — envoi vers le backend (MCP, HTTP ou CLI)
7. **Trace** — enregistrement complet (agent, outil, paramètres, décision, latence, tokens estimés)
8. **Réponse** — retour du résultat à l'agent

---

## Sources d'outils

Agent Mesh importe des outils depuis 3 types de sources et les expose de manière unifiée.

### MCP (Model Context Protocol)

Le standard émergent pour connecter des agents IA à des outils. Agent Mesh se connecte en tant que client MCP à des serveurs existants (stdio ou SSE) et les gouverne.

```yaml
mcp_servers:
  - name: filesystem
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/home/projets"]

  - name: gmail
    transport: stdio
    command: npx
    args: ["-y", "@monsoft/mcp-gmail"]

  - name: service-distant
    transport: sse
    url: "https://mcp.example.com/sse"
    headers:
      Authorization: "Bearer <token>"
```

Chaque outil est nommé `<serveur>.<outil>` : `filesystem.write_file`, `gmail.gmail_send_email`.

### OpenAPI / REST

Agent Mesh lit un spec OpenAPI (Swagger) et transforme chaque endpoint en outil gouverné.

```bash
agent-mesh --openapi https://api.example.com/swagger.json --config config.yaml
```

Un `POST /pets` devient l'outil `addPet`, un `GET /pets/{id}` devient `getPetById`. Les paramètres (path, query, body) sont extraits automatiquement.

### CLI (binaires locaux)

Agent Mesh enveloppe n'importe quel binaire CLI (terraform, kubectl, docker, gh, aws) derrière les mêmes politiques et traces. Trois modes :

| Mode | Comportement | Exemple |
|------|-------------|---------|
| **Simple** | Toutes les sous-commandes, action par défaut | `gh` avec `default_action: allow` |
| **Affiné** | Commandes spécifiques avec contraintes | `terraform plan` (timeout 120s), `terraform apply` (args restreints) |
| **Strict** | Uniquement les commandes déclarées, le reste interdit | `kubectl get` et `kubectl describe` uniquement |

```yaml
cli_tools:
  - name: terraform
    bin: terraform
    default_action: human_approval
    commands:
      plan:
        timeout: 120s
      apply:
        allowed_args: ["-target"]

  - name: kubectl
    bin: kubectl
    strict: true
    commands:
      get:
        allowed_args: ["-n", "--namespace", "-o"]
      describe:
        allowed_args: ["-n", "--namespace"]
```

Sécurité : exécution directe sans shell (`exec.Command`), liste blanche d'arguments, rejet des métacaractères, timeout, isolation d'environnement, limite de sortie (1 Mo).

### Export

Agent Mesh expose tous les outils gouvernés via :

- **MCP stdio** — pour Claude Code, Cursor, ou tout client MCP
- **HTTP API** — pour LangChain, CrewAI, scripts Python, cURL

Tous les modes composent : importer REST + MCP + CLI, appliquer une politique unifiée, exporter via MCP ou HTTP.

---

## Politique d'accès

### Principe

Les politiques sont définies en YAML. Chaque agent est associé à une politique qui contient des règles. **Première règle qui matche = décision appliquée.** Si aucune règle ne matche, l'accès est **interdit** (fail-closed).

### Trois actions

| Action | Comportement |
|--------|-------------|
| `allow` | Appel autorisé, envoyé au backend, résultat retourné |
| `deny` | Appel bloqué, l'agent reçoit un refus |
| `human_approval` | Appel suspendu jusqu'à validation humaine (ou automatique via supervisor) |

### Patterns glob

Les noms d'outils et d'agents supportent les patterns glob :

- `*` — matche tout
- `filesystem.*` — tous les outils du serveur filesystem
- `gmail.gmail_read_*` — toutes les opérations de lecture Gmail
- `support-*` — tous les agents dont le nom commence par "support-"

### Conditions

Les règles peuvent inclure des conditions sur les paramètres :

```yaml
rules:
  - tools: ["create_refund"]
    action: allow
    condition:
      field: "params.amount"
      operator: "<"
      value: 500

  - tools: ["create_refund"]
    action: human_approval  # montant >= 500 → validation requise
```

### Exemple complet

```yaml
policies:
  - name: claude
    agent: "claude"
    rate_limit:
      max_per_minute: 60
      max_total: 5000
    rules:
      # Lectures — toujours autorisées
      - tools: ["filesystem.read_*", "filesystem.list_*", "filesystem.search_*"]
        action: allow

      # Écritures — approbation humaine requise
      - tools: ["filesystem.write_file", "filesystem.edit_file"]
        action: human_approval

      # Suppressions — interdites
      - tools: ["filesystem.move_file"]
        action: deny

      # Gmail — lecture OK, envoi avec approbation, suppression interdite
      - tools: ["gmail.gmail_read_*", "gmail.gmail_list_*"]
        action: allow
      - tools: ["gmail.gmail_send_email", "gmail.gmail_draft_email"]
        action: human_approval
      - tools: ["gmail.gmail_delete_*"]
        action: deny

  # Politique par défaut : tout interdit
  - name: default
    agent: "*"
    rules:
      - tools: ["*"]
        action: deny
```

---

## Approbation humaine

Quand une politique exige `human_approval`, le flux est **non-bloquant** pour l'agent :

```
Claude appelle filesystem.write_file
  → agent-mesh : "Approbation requise (id: a1b2c3d4)"
  → Claude montre le message à l'utilisateur
  → L'utilisateur approuve dans le chat
  → Claude appelle approval.resolve(id: a1b2c3d4, decision: approve)
  → agent-mesh rejoue l'appel original vers le backend
  → Résultat retourné à Claude
```

### Trois façons de résoudre une approbation

| Méthode | Cas d'usage |
|---------|------------|
| **Dans le chat** (approval.resolve) | Dev solo — l'utilisateur valide dans la conversation |
| **CLI** (`mesh approve <id>`) | Depuis un autre terminal |
| **HTTP API** (`POST /approvals/{id}/approve`) | Automatisation, CI/CD, supervisor agent |

### Timeout

Les approbations expirent après 5 minutes (configurable). Timeout = refus.

### Grants temporels (sudo pour agents)

Quand l'utilisateur approuve le même type d'appel de façon répétitive, il peut créer un **grant temporel** — une autorisation temporaire qui bypass l'approbation :

```
"Accorde filesystem.write_* pour 30 minutes"
→ Grant créé, expire dans 30 min
→ Tous les filesystem.write_* sont auto-approuvés
→ Tracés comme "grant:<id>" (audit complet)
```

Les grants expirent automatiquement. Chaque appel qui utilise un grant est tracé avec l'ID du grant.

---

## Rate limiting

Protection contre les agents qui tournent en boucle ou épuisent un budget.

| Protection | Ce qu'elle empêche | Réponse |
|------------|-------------------|---------|
| `max_per_minute` | Agent trop rapide (boucle infinie) | HTTP 429 |
| `max_total` | Agent qui épuise son budget sur la durée | HTTP 429 |
| Détection de boucle | Même outil + mêmes paramètres > 3× en 10s | HTTP 429 `loop_detected` |

La détection de boucle est **toujours active**. Les rate limits sont optionnels par politique.

---

## Traçabilité

Chaque appel d'outil est enregistré avec :

| Champ | Description |
|-------|------------|
| `trace_id` | Identifiant unique (compatible W3C Traceparent) |
| `agent_id` | Quel agent a fait l'appel |
| `tool` | Quel outil a été appelé |
| `params` | Les paramètres envoyés |
| `policy` | Décision appliquée (allow, deny, human_approval, rate_limited) |
| `policy_rule` | Quelle règle a matché |
| `status_code` | Code de retour du backend |
| `latency_ms` | Temps de réponse total |
| `estimated_input_tokens` | Tokens estimés en entrée (chars/4) |
| `estimated_output_tokens` | Tokens estimés en sortie (chars/4) |
| `approval_id` | ID de l'approbation (si applicable) |
| `approval_status` | Résultat de l'approbation |
| `approved_by` | Qui a approuvé (humain, supervisor, grant) |

### Stockage

- **In-memory** — buffer circulaire (10 000 entrées par défaut)
- **JSONL** — persistance fichier, rotation automatique à 10 Mo

### Requêtes

```bash
# Toutes les traces d'un agent
curl http://localhost:9090/traces?agent=claude

# Traces d'un outil spécifique
curl http://localhost:9090/traces?tool=filesystem.write_file

# Stats agrégées (incluant tokens estimés)
curl http://localhost:9090/health | jq '.traces'
```

### Estimation de tokens

Chaque trace inclut une estimation du nombre de tokens (entrée et sortie) basée sur l'heuristique 1 token ≈ 4 caractères. Ce n'est pas exact (le vrai tokenizer dépend du modèle) mais suffisant pour estimer les coûts et détecter les appels anormalement lourds.

Les totaux sont agrégés dans `GET /health` :

```json
{
  "traces": {
    "total": 1234,
    "estimated_input_tokens": 45000,
    "estimated_output_tokens": 12000
  }
}
```

---

## Protocole supervisor

Pour les cas multi-agents ou les pipelines autonomes, un agent **supervisor externe** peut résoudre les approbations à la place de l'humain.

### Principe

```
Agent de travail ──> agent-mesh ──> file d'approbation
                                         │
                              Supervisor (poll toutes les 2s)
                              ├── Règles rapides (0ms)
                              ├── LLM local (Ollama, ~20s)
                              └── Escalade vers l'humain
```

### Hiérarchie de confiance

| Niveau | Rôle | Exemple |
|--------|------|---------|
| **Politique (L0)** | Règles statiques, instantanées | `allow` lectures, `deny` suppressions |
| **Supervisor (L1)** | Évaluation dynamique, jugement borné | Écriture dans le répertoire projet → approuver |
| **Humain (L2)** | Jugement complet, coûteux en attention | Envoi d'email externe, écriture hors scope |

L'objectif n'est pas de remplacer l'humain mais de **protéger son attention** pour les décisions qui en ont vraiment besoin.

### API supervisor

| Endpoint | Description |
|----------|------------|
| `GET /approvals?status=pending&tool=filesystem.*` | Liste les approbations en attente (filtrable par outil) |
| `GET /approvals/{id}` | Détail avec historique récent de l'agent et grants actifs |
| `POST /approvals/{id}/approve` | Approuver avec `reasoning` et `confidence` optionnels |
| `POST /approvals/{id}/deny` | Refuser avec `reasoning` et `confidence` optionnels |

### Sécurité

- **Isolation du contenu** — le supervisor reçoit des métadonnées (taille, hash SHA256, type MIME) au lieu du contenu brut. Réduit la surface d'attaque par injection de prompt.
- **Détection d'injection** — chaque approbation inclut un flag `injection_risk` quand des patterns suspects sont détectés dans les paramètres.
- **Mode supervisor** — `supervisor.enabled: true` masque les outils `approval.resolve`/`approval.pending` des agents pour que seul le supervisor puisse résoudre.

### Implémentation de référence

Une implémentation Python complète est disponible dans le projet [agent7](https://github.com/KTCrisis/flux7-console) :

- Moteur de règles (fast path, 0ms)
- Fallback LLM via Ollama (configurable, ~20s)
- Gestion du cycle de vie agent-mesh (spawn, health check, restart)
- Mémoire persistante des décisions via memory-mcp
- 44 tests

---

## Sécurité

### OWASP Agentic Top 10

Agent Mesh couvre 6 des 10 risques du [OWASP Top 10 pour les systèmes agentiques](https://owasp.org/www-project-top-10-for-large-language-model-applications/) :

| Risque OWASP | Couverture agent-mesh |
|-------------|----------------------|
| Excessive Agency | Politique d'accès par outil, fail-closed |
| Tool Misuse | Rate limiting, détection de boucle |
| Privilege Escalation | Politique par agent, pas de shell execution |
| Prompt Injection (indirect) | Détection d'injection dans les paramètres |
| Insufficient Logging | Trace complète de chaque appel |
| Insecure Output Handling | Isolation du contenu pour le supervisor |

### Exécution CLI sécurisée

Les outils CLI sont exécutés sans shell (`exec.Command` direct) avec :

- Liste blanche d'arguments
- Rejet des métacaractères shell (`|`, `;`, `$()`, `` ` ``)
- Timeout configurable par commande
- Isolation d'environnement (variables d'env spécifiques)
- Limite de sortie (1 Mo)

---

## Modes de déploiement

### Dev solo + CLI IA (recommandé pour commencer)

```
Claude Code ──stdio──> agent-mesh ──> outils
```

L'agent lance agent-mesh automatiquement. L'utilisateur approuve dans le chat. Zéro config supplémentaire.

### Dev solo + supervisor passif

```
Claude Code ──stdio──> agent-mesh :9090 ──> outils
                           │
                    supervisor (poll)
```

Le supervisor résout automatiquement les approbations routinières. L'utilisateur ne voit que les escalades.

### Multi-agent partagé

```
Claude Code ──stdio──> agent-mesh :9090 ──> outils
                           │
Agent Python ───HTTP───────┘
```

Plusieurs agents partagent la même instance, les mêmes politiques, les mêmes traces.

### Pipeline autonome (sans humain)

```
supervisor ──spawn──> agent-mesh :9090 ──> outils
                           │
scripts/cron ───HTTP───────┘
```

Le supervisor gère le cycle de vie d'agent-mesh et résout toutes les approbations. Pour les batch jobs, CI/CD, exécutions overnight.

Voir [docs/deployment-modes.md](deployment-modes.md) pour la matrice complète des 7 configurations.

---

## Installation

### Binaire précompilé

```bash
VERSION=$(curl -s https://api.github.com/repos/KTCrisis/agent-mesh/releases/latest | grep tag_name | cut -d '"' -f4)

# Linux (amd64)
curl -L "https://github.com/KTCrisis/flux7-mesh/releases/download/${VERSION}/agent-mesh_${VERSION#v}_linux_amd64.tar.gz" | tar xz
sudo mv agent-mesh /usr/local/bin/

# macOS (Apple Silicon)
curl -L "https://github.com/KTCrisis/flux7-mesh/releases/download/${VERSION}/agent-mesh_${VERSION#v}_darwin_arm64.tar.gz" | tar xz
sudo mv agent-mesh /usr/local/bin/
```

### Depuis les sources

```bash
git clone https://github.com/KTCrisis/flux7-mesh.git
cd agent-mesh
go build -o agent-mesh .
```

Requiert Go 1.24+. Aucune dépendance externe (seul `gopkg.in/yaml.v3`).

## Démarrage rapide

### 1. Générer une config

```bash
# Depuis des serveurs MCP existants
agent-mesh discover --config config.yaml --generate-policy

# Depuis un spec OpenAPI
agent-mesh discover --openapi https://api.example.com/swagger.json --generate-policy > config.yaml
```

### 2. Brancher sur Claude Code

```bash
claude mcp add agent-mesh -- agent-mesh --mcp --config config.yaml
```

### 3. Utiliser normalement

L'agent voit les outils. Agent Mesh applique les règles. Chaque appel est tracé.

---

## Spécifications techniques

| Caractéristique | Détail |
|----------------|--------|
| **Langage** | Go 1.24 |
| **Dépendance unique** | `gopkg.in/yaml.v3` |
| **Taille du binaire** | ~9 Mo (compilation statique) |
| **Protocole agent** | MCP stdio (JSON-RPC 2.0) + HTTP REST |
| **Protocole upstream** | MCP (stdio + SSE) + HTTP + CLI exec |
| **Politique** | YAML, first-match-wins, glob patterns, fail-closed |
| **Trace** | In-memory (buffer 10k) + JSONL (rotation 10 Mo) |
| **Approbation** | Channel-based blocking, timeout 5 min, prefix match |
| **Rate limiting** | Sliding window/min + total budget + loop detection |
| **Tests** | 222 tests, 14 packages, race detector clean |
| **Licence** | Apache 2.0 |

---

## Ce qu'Agent Mesh n'est pas

| Il est | Il n'est pas |
|--------|-------------|
| Un proxy de gouvernance pour les appels d'outils | Un API gateway (Kong, Apigee) |
| Un sidecar léger et local | Un framework d'agents (LangChain, CrewAI) |
| De la config-as-code en YAML | Une plateforme cloud |
| Une couche d'observabilité pour les actions des agents | Un service d'hébergement MCP |
| Compatible avec tout agent (Claude, GPT, Gemini, Ollama) | Lié à un fournisseur spécifique |

---

## Positionnement

### Comparaison avec les approches existantes

| Système | Modèle | Limitation par rapport à agent-mesh |
|---------|--------|-------------------------------------|
| **Claude Code** (built-in) | Permission prompt par appel | Lié à un agent, pas de délégation, pas d'audit |
| **LangChain** (`HumanApprovalCallbackHandler`) | In-process, middleware | Pas de séparation des responsabilités |
| **OpenAI Agents SDK** (Guardrails) | Validation input/output | Règles uniquement, pas de jugement délégué |
| **CrewAI** (Manager agent) | Pattern manager intégré | Spécifique au framework |
| **Microsoft Agent Governance Toolkit** | Middleware in-process | Pas de sidecar, pas d'observabilité externe |

Agent Mesh sépare le **plan de gouvernance** du **plan d'exécution**. L'agent ne sait pas que le proxy existe. La gouvernance est invisible pour l'agent, visible pour l'opérateur.

### Spectre d'autonomie

Un même outil peut évoluer sur ce spectre au fil du temps :

```
interdit ◄──── approbation humaine ◄──── supervisor ◄──── autorisé

"jamais"    "l'humain décide"    "l'agent décide,     "toujours"
                                  humain si escalade"
```

Exemple pour `gmail.send` :

1. **Jour 1** : `deny` — pas prêt
2. **Jour 30** : `human_approval` — l'humain valide chaque envoi
3. **Jour 90** : supervisor — l'agent supervisor évalue, escalade les destinataires externes
4. **Jour 180** : `allow` pour les destinataires internes — pattern de confiance établi

Les politiques évoluent à mesure que la confiance s'établit grâce aux données de trace. C'est la **confiance progressive**.
