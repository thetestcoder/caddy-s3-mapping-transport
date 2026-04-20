# caddy-s3-mapping-transport

A Caddy v2 handler that serves **private S3 objects** based on a **per-domain mapping stored in PostgreSQL**.

For each incoming request the handler:

1. Looks up the request `Host` in a Postgres table to find a **mapping ID** (UUID).
2. Constructs a safe S3 object key under that UUID prefix (e.g. `3a4be247-…/index.html`).
3. Signs the request with **AWS Signature V4** and streams the object to the client.

Root requests (`/`) always resolve to `{mapping_id}/index.html`, making this ideal for **single-page applications** deployed as static builds into per-tenant S3 prefixes.

## Expected S3 layout

```
my-bucket/
├── 3a4be247-8922-467c-9844-31c71207e8f4/
│   ├── assets/
│   │   └── ...
│   └── index.html
├── 9b910a83-d873-487a-a96a-47ae30684837/
│   ├── assets/
│   │   └── ...
│   └── index.html
└── ...
```

Each top-level folder is a mapping ID (UUID) that corresponds to a row in the Postgres table. The handler **never** serves objects outside the resolved prefix — path traversal is rejected.

## Build

You must build Caddy with this plugin using [xcaddy](https://github.com/caddyserver/xcaddy):

```bash
xcaddy build --with github.com/thetestcoder/caddy-s3-mapping-transport
```

For local development, point xcaddy at your checkout:

```bash
xcaddy build --with github.com/thetestcoder/caddy-s3-mapping-transport=./
```

Verify the module is loaded:

```bash
./caddy list-modules | grep s3_mapping
# http.handlers.s3_mapping
```

## Environment variables

All configuration can be provided via environment variables. Caddyfile directives override the corresponding env var when both are set.

| Variable | Required | Description |
|---|---|---|
| `S3_MAPPING_DATABASE_URL` | **yes** | Postgres connection URL (e.g. `postgres://user:pass@host:5432/db`) |
| `S3_MAPPING_TABLE` | **yes** | Table name containing domain mappings |
| `S3_MAPPING_DOMAIN_COLUMN` | **yes** | Column holding the incoming domain / hostname |
| `S3_MAPPING_ID_COLUMN` | **yes** | Column holding the mapping UUID (S3 prefix) |
| `S3_MAPPING_BUCKET` | **yes** | S3 bucket name |
| `S3_MAPPING_REGION` | **yes** | AWS region (e.g. `ap-south-1`) |
| `S3_MAPPING_CACHE_TTL` | no | Global domain-cache TTL (e.g. `30m`, `1800s`). Default: **30 minutes** |
| `S3_MAPPING_CACHE_TTL_COLUMN` | no | Postgres column (same table) with per-row cache TTL in **seconds** |
| `S3_MAPPING_NEGATIVE_CACHE_TTL` | no | TTL for caching "domain not found" results. Default: same as main TTL |
| `S3_MAPPING_SPA_FALLBACK` | no | `true` to serve `index.html` on 404 for HTML navigation requests |
| `S3_MAPPING_ACCESS_ID` | no | AWS access key ID (static credentials) |
| `S3_MAPPING_SECRET_KEY` | no | AWS secret access key (static credentials) |
| `S3_USE_IAM_PROVIDER` | no | `true` to use MinIO IAM provider (EC2 instance metadata) |

### AWS credential resolution (same as caddy-s3-transport)

1. **IAM provider** — `S3_USE_IAM_PROVIDER=true` or Caddyfile `use_iam_provider`. Uses EC2 instance metadata via MinIO IAM.
2. **Static keys** — `S3_MAPPING_ACCESS_ID` + `S3_MAPPING_SECRET_KEY` (or Caddyfile `access_id` / `secret_key`). Uses MinIO static V4.
3. **AWS SDK default chain** — If neither of the above is configured, falls back to the standard AWS credential chain (`AWS_ACCESS_KEY_ID`, shared config, IMDS, etc.).

## Cache TTL precedence

The domain → mapping-ID cache uses the **first matching** TTL source:

1. **Per-row database column** — enabled by `S3_MAPPING_CACHE_TTL_COLUMN`. Value is integer seconds. If the column is `NULL` on a given row, falls through.
2. **Caddyfile** — `cache_ttl` subdirective.
3. **Environment** — `S3_MAPPING_CACHE_TTL`.
4. **Default** — **30 minutes**.

Changes to mapping rows propagate after the cached entry expires. Reloading or restarting Caddy clears the cache immediately.

## Caddyfile

The handler directive is `s3_mapping`. Use it inside a `route` block or add `order s3_mapping before respond` to global options.

### Minimal (env-only configuration)

```caddyfile
{
    order s3_mapping before respond
}

:443 {
    tls /path/cert.pem /path/key.pem
    s3_mapping
}
```

### Full (all directives shown)

```caddyfile
{
    order s3_mapping before respond
}

:443 {
    tls /path/cert.pem /path/key.pem

    route {
        s3_mapping {
            database_url  "postgres://user:pass@localhost:5432/mydb"
            table         site_mappings
            domain_column domain
            id_column     mapping_id
            bucket        my-spa-bucket
            region        ap-south-1

            cache_ttl        30m
            cache_ttl_column cache_seconds
            negative_cache_ttl 5m
            spa_fallback

            use_iam_provider
        }
    }
}
```

### Directives

| Directive | Description |
|---|---|
| `database_url` | Postgres connection URL |
| `table` | Table name |
| `domain_column` | Column with the domain |
| `id_column` | Column with the mapping UUID |
| `cache_ttl_column` | Column with per-row TTL (seconds) |
| `bucket` | S3 bucket name |
| `region` | AWS region |
| `cache_ttl` | Global cache TTL (duration string) |
| `negative_cache_ttl` | Negative-cache TTL (duration string) |
| `spa_fallback` | Enable SPA fallback (bare keyword = true, or explicit `true`/`false`) |
| `use_iam_provider` | Use IAM / instance role (bare keyword = true) |
| `access_id` | AWS access key ID |
| `secret_key` | AWS secret access key |

## How it works

```
Client request (Host: app.example.com, GET /assets/main.js)
  │
  ▼
┌──────────────────────────┐
│  Normalize Host          │
│  "app.example.com"       │
└──────────┬───────────────┘
           │
           ▼
┌──────────────────────────┐  hit
│  In-memory domain cache  │ ──────► mapping_id
└──────────┬───────────────┘
           │ miss / expired
           ▼
┌──────────────────────────┐
│  SELECT mapping_id       │
│  FROM table              │
│  WHERE domain = $1       │
└──────────┬───────────────┘
           │
           ▼
┌──────────────────────────┐
│  Build safe S3 key       │
│  {uuid}/assets/main.js   │
│  (reject .., traversal)  │
└──────────┬───────────────┘
           │
           ▼
┌──────────────────────────┐
│  SigV4-sign GET          │
│  s3.{region}.amazonaws…  │
└──────────┬───────────────┘
           │
           ▼
     Stream response
```

## SPA fallback

When `spa_fallback` is enabled and an S3 object returns **404**, the handler checks whether the request looks like browser navigation (`Accept` includes `text/html`, path has no file extension or is `.html`). If so, it retries with `{mapping_id}/index.html` — the standard pattern for client-side routed SPAs.

Static assets (`.js`, `.css`, `.png`, etc.) are **not** retried and return 404 normally.

## Security

- **Path jailing**: All object keys are forced under `{mapping_id}/`. Segments containing `..` are rejected outright after `path.Clean` normalization.
- **SQL identifier validation**: Table and column names from env/config are validated against `^[a-zA-Z_][a-zA-Z0-9_]*$` and double-quoted in SQL. The domain value is bound as a parameterized `$1`.
- **No ListBucket**: The handler only calls `GetObject`. It never lists bucket contents.

## Requirements

- **Caddy v2** built with this plugin (see Build above).
- **PostgreSQL** accessible from the Caddy host, with a table containing domain and mapping-ID columns.
- **S3 bucket** with the expected UUID-prefix layout. The IAM role / credentials must have `s3:GetObject` on the bucket (or on `arn:aws:s3:::bucket/${mapping_id}/*` for least privilege).

## Troubleshooting

### "module not registered: http.handlers.s3_mapping"

The running Caddy binary was not built with this plugin. Rebuild with `xcaddy` and use the resulting binary.

### 502 Bad Gateway

Check Caddy logs. Common causes:
- Postgres is unreachable or credentials are wrong.
- AWS credentials are missing / expired.
- The S3 bucket or region is wrong.

### Domain returns 404 but row exists

The domain-cache may still hold a stale negative entry. Wait for the TTL to expire, or restart Caddy to clear the cache.

## License

MIT
