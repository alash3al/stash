# Getting Started with Stash

You ran `docker compose up`. Here's what to do next.

## 1. Verify Stash is running

```bash
docker compose ps
docker compose logs stash | tail -20
```

You should see the MCP server listening on port **8080**. The SSE endpoint is:

```
http://localhost:8080/sse
```

Quick check (expects HTTP 200 or SSE handshake):

```bash
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/sse
```

## 2. Connect your MCP client

Point any MCP-over-SSE client at `http://localhost:8080/sse`.

**Cursor** — create or edit `~/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "stash": {
      "url": "http://localhost:8080/sse"
    }
  }
}
```

Restart Cursor (or reload MCP). Stash should appear in your MCP tool list.

See [README MCP Client Setup](../README.md#mcp-client-setup) for Claude Desktop, Windsurf, and OpenCode configs.

## 3. First session workflow

Once connected, ask your agent to use Stash tools in this order:

| Step | MCP tool | What it does |
|------|----------|--------------|
| 1 | `init` | Creates `/self` namespace scaffold (capabilities, limits, preferences) |
| 2 | `remember` | Store an episode or fact ("User prefers Python", "Project uses Postgres") |
| 3 | `recall` | Semantic search over what you stored |
| 4 | `consolidate` | Run the pipeline that turns episodes into structured facts |

Example prompts to your agent:

- *"Call Stash `init` to set up memory namespaces."*
- *"Use Stash `remember` to store: this project uses Stash on port 8080 with pgvector."*
- *"Use Stash `recall` to find what you know about this project's stack."*
- *"Run Stash `consolidate` to process recent episodes into facts."*

## 4. Background consolidation

When started with `mcp serve --with-consolidation` (the Docker Compose default), Stash consolidates in the background. You can also trigger manually via the `consolidate` MCP tool or CLI:

```bash
docker compose exec stash /stash consolidate run
```

## 5. Configuration checklist

If tools fail, check `.env`:

| Variable | Purpose |
|----------|---------|
| `STASH_OPENAI_API_KEY` | Embeddings + reasoner (OpenAI, OpenRouter, or compatible) |
| `STASH_OPENAI_BASE_URL` | API base URL |
| `STASH_EMBEDDING_MODEL` | Must match `STASH_VECTOR_DIM` (1536 for `text-embedding-3-small`) |
| `STASH_REASONER_MODEL` | Model used during consolidation |
| `STASH_REASONER_API_KEY` | Optional separate key for the consolidation reasoner |
| `STASH_REASONER_BASE_URL` | Optional separate OpenAI-compatible reasoner endpoint |
| `STASH_RETRIEVAL_LEARNING_ENABLED` | Enable bounded outcome-aware recall; defaults to `false` |

**Running fully local?** See [LOCAL_OLLAMA.md](LOCAL_OLLAMA.md) — Ollama on the host, no cloud API key.

**Using Atlas Cloud?** Stash already supports it through the OpenAI-compatible API:

```bash
STASH_OPENAI_API_KEY=your-atlas-cloud-api-key
STASH_OPENAI_BASE_URL=https://api.atlascloud.ai/v1
STASH_EMBEDDING_MODEL=text-embedding-3-small
STASH_REASONER_MODEL=deepseek-ai/DeepSeek-V3-0324
STASH_VECTOR_DIM=1536
```

Atlas Cloud docs: [https://www.atlascloud.ai/docs](https://www.atlascloud.ai/docs)

## Troubleshooting

**MCP client can't connect**

- Confirm `docker compose up` finished and port 8080 is not in use elsewhere.
- Use `http://localhost:8080/sse` (not `https`) for local Docker.

**Empty recall results**

- Run `init` first, then `remember`, then `recall`.
- Embeddings require a valid API key in `.env`.

**Consolidation does nothing**

- Needs episodes via `remember` first.
- Check logs: `docker compose logs stash`.

**Provider returns auth or model errors**

- Confirm `STASH_OPENAI_BASE_URL` points to the correct provider endpoint.
- Confirm the embedding and reasoner model names match what your provider exposes.
- Atlas Cloud users should use `https://api.atlascloud.ai/v1` and a valid API key.

## Next steps

- Explore namespaces: `list_namespaces`, `create_namespace`
- Track goals and failures: `create_goal`, `create_failure`
- Full tool list: see the [docs site MCP section](https://alash3al.github.io/stash/#mcp)
