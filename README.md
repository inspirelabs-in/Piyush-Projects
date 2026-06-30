# Project Synapse

**An AI-powered codebase intelligence, visual topology, and product-blueprinting engine.**

Synapse ingests source code, builds an absolute Abstract Syntax Tree (AST) dependency graph, and marries it with a vector semantic layer — letting you visually explore, chat with, and map capability reuse across your engineering assets through a neuroscience-inspired interface.

---

## Features

| Module | What it does |
| --- | --- |
| **Cortex Perspectives** | Toggle the canvas between an *Executive* (folders / macro-flows) and *Synaptic* (deep AST: file cards + `ƒ` function anchors) zoom depth. |
| **Axon Pathways** | Dependency-sorted, topological code walkthroughs with fluid GSAP camera-fly onboarding tours. |
| **Dendrite Callouts** | Contextual markers for complex idioms (closures, HOFs, decorators, transactional blocks). |
| **Myelin Insulation** | Ingests Markdown / `/docs` wikis, vectorizes them, and overlays human docs onto the code graph. |
| **Synaptic Pruning** | Confidence-tiered dead / unreachable code detection (orphan files, unused exports/functions) with evidence. |
| **Bug & Anti-Pattern Engine** | Two-tier diagnostics: deterministic graph scans (circular deps via Tarjan SCC, resource leaks) + an adversarial LLM red-team pass over pgvector-retrieved context. Surfaced in a hacker-terminal dashboard. |
| **RAG Chat + Blueprints** | Hybrid vector + structural retrieval to answer questions and discover reusable capability blueprints. |

Multi-language parsing: **TypeScript / JavaScript** (real `tsc` AST via a Node sub-process), **Go** (`go/parser`), and **Rust** (lexical item scanner).

---

## Tech Stack

- **Backend** — Go (concurrent AST ingestion, RAG, graph + diagnostics engines) over `pgx`.
- **Frontend** — Next.js 16 (App Router, TypeScript), React Flow canvas, GSAP motion, Tailwind v4.
- **Data** — PostgreSQL 16 + `pgvector` (HNSW) + `pg_trgm`, via Docker.
- **Auth** — Auth.js (next-auth) with a frictionless "Continue as Local Developer" bypass for local use.

```
backend/    Go modules — ingestion, parsers, RAG, graph, prune, bugs, HTTP API
frontend/   Next.js app — canvas, workspace panels, diagnostics terminal
docker/     docker-compose (Postgres + pgvector) + schema init SQL
docs/       Architecture logs & system schemas
```

---

## Quickstart

**Prerequisites:** Docker, Go 1.26+, Node 20+.

### 1. Start the database
```bash
cd docker
docker compose up -d        # Postgres 16 + pgvector on :5432, schema auto-applied
```

### 2. Configure
```bash
cp backend/.env.example backend/.env          # set provider keys (or run offline)
cp frontend/.env.example frontend/.env.local  # optional: OAuth; defaults work locally
```
The app runs **100% offline** with no keys (deterministic embeddings + a template LLM responder). Add an embedding/LLM provider key in `backend/.env` to enable the semantic layer, docs, and the Tier-2 bug analysis. See `backend/.env.example` for every option.

### 3. Run the backend
```bash
cd backend
./run.ps1                   # Windows: loads .env, then `go run ./cmd/server`
# or, cross-platform, export the vars and:  go run ./cmd/server
```
API listens on `http://localhost:8080`.

### 4. Run the frontend
```bash
cd frontend
npm install
npm run dev                 # next dev --webpack  →  http://localhost:3000
```

Open http://localhost:3000, click **Continue as Local Developer**, and ingest a local repository path to build its graph.

---

## Configuration

All backend config is environment-driven (`backend/internal/config`). Copy `backend/.env.example` and set what you need — database URL, ingestion root, embedding/LLM provider + keys, RAG tuning, and bug-scan options (`SYNAPSE_BUGS_LLM`, `SYNAPSE_BUGS_MAX_LLM`).

> **Secrets:** real `.env` / `.env.local` files are gitignored. Never commit API keys — only the `.env.example` templates are tracked.

---

## Development

```bash
# Backend
cd backend && go build ./... && go test ./...

# Frontend
cd frontend && npx tsc --noEmit && npm run lint
```
