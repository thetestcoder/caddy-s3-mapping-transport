package s3mapping

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(new(S3MappingHandler))
}

var safeIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// S3MappingHandler resolves incoming Host → mapping UUID via Postgres,
// then serves S3 objects jailed under that UUID prefix.
type S3MappingHandler struct {
	// Postgres
	DatabaseURL    string `json:"database_url,omitempty"`
	Table          string `json:"table,omitempty"`
	DomainColumn   string `json:"domain_column,omitempty"`
	IDColumn       string `json:"id_column,omitempty"`
	CacheTTLColumn string `json:"cache_ttl_column,omitempty"`

	// S3
	Bucket string `json:"bucket,omitempty"`
	Region string `json:"region,omitempty"`

	// Cache
	CacheTTL    caddy.Duration `json:"cache_ttl,omitempty"`
	NegCacheTTL caddy.Duration `json:"negative_cache_ttl,omitempty"`

	// Behaviour
	SPAFallback bool `json:"spa_fallback,omitempty"`

	// AWS credentials (Caddyfile overrides)
	UseIamProvider bool   `json:"use_iam_provider,omitempty"`
	AccessKeyID    string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`

	// Runtime (not serialised)
	pool   *pgxpool.Pool
	cache  *domainCache
	s3     *s3Client
	logger *zap.Logger
	query  string
}

func (*S3MappingHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.s3_mapping",
		New: func() caddy.Module { return new(S3MappingHandler) },
	}
}

// Provision sets up the database pool, domain cache, and S3 client.
func (h *S3MappingHandler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	h.loadEnvDefaults()

	// Required fields
	for name, val := range map[string]string{
		"database_url":  h.DatabaseURL,
		"table":         h.Table,
		"domain_column": h.DomainColumn,
		"id_column":     h.IDColumn,
		"bucket":        h.Bucket,
		"region":        h.Region,
	} {
		if val == "" {
			return fmt.Errorf("s3_mapping: %s is required (set via Caddyfile or environment)", name)
		}
	}

	// SQL identifier safety
	for name, val := range map[string]string{
		"table": h.Table, "domain_column": h.DomainColumn, "id_column": h.IDColumn,
	} {
		if !safeIdentifier.MatchString(val) {
			return fmt.Errorf("s3_mapping: invalid %s %q (must match %s)", name, val, safeIdentifier.String())
		}
	}
	if h.CacheTTLColumn != "" && !safeIdentifier.MatchString(h.CacheTTLColumn) {
		return fmt.Errorf("s3_mapping: invalid cache_ttl_column %q", h.CacheTTLColumn)
	}

	// Pre-build SQL query
	if h.CacheTTLColumn != "" {
		h.query = fmt.Sprintf(
			`SELECT "%s", "%s" FROM "%s" WHERE "%s" = $1 LIMIT 1`,
			h.IDColumn, h.CacheTTLColumn, h.Table, h.DomainColumn,
		)
	} else {
		h.query = fmt.Sprintf(
			`SELECT "%s" FROM "%s" WHERE "%s" = $1 LIMIT 1`,
			h.IDColumn, h.Table, h.DomainColumn,
		)
	}

	// Postgres pool
	pool, err := pgxpool.New(context.Background(), h.DatabaseURL)
	if err != nil {
		return fmt.Errorf("s3_mapping: connect to database: %w", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		return fmt.Errorf("s3_mapping: ping database: %w", err)
	}
	h.pool = pool
	h.logger.Info("database connected", zap.String("database_url", redactDatabaseURL(h.DatabaseURL)))

	// Domain cache
	h.cache = newDomainCache()

	// S3 client
	s3c, err := newS3Client(h.Bucket, h.Region, h.UseIamProvider, h.AccessKeyID, h.SecretAccessKey)
	if err != nil {
		h.pool.Close()
		return fmt.Errorf("s3_mapping: %w", err)
	}
	h.s3 = s3c
	if err := h.s3.ping(context.Background()); err != nil {
		h.pool.Close()
		return fmt.Errorf("s3_mapping: %w", err)
	}
	h.logger.Info("s3 connected",
		zap.String("bucket", h.Bucket),
		zap.String("region", h.Region),
	)

	h.logger.Info("s3_mapping provisioned",
		zap.String("bucket", h.Bucket),
		zap.String("region", h.Region),
		zap.String("table", h.Table),
		zap.Duration("cache_ttl", time.Duration(h.CacheTTL)),
		zap.Bool("spa_fallback", h.SPAFallback),
	)
	return nil
}

func (h *S3MappingHandler) loadEnvDefaults() {
	envOr := func(field *string, key string) {
		if *field == "" {
			*field = os.Getenv(key)
		}
	}
	envOr(&h.DatabaseURL, "S3_MAPPING_DATABASE_URL")
	envOr(&h.Table, "S3_MAPPING_TABLE")
	envOr(&h.DomainColumn, "S3_MAPPING_DOMAIN_COLUMN")
	envOr(&h.IDColumn, "S3_MAPPING_ID_COLUMN")
	envOr(&h.Bucket, "S3_MAPPING_BUCKET")
	envOr(&h.Region, "S3_MAPPING_REGION")
	envOr(&h.CacheTTLColumn, "S3_MAPPING_CACHE_TTL_COLUMN")
	envOr(&h.AccessKeyID, "S3_MAPPING_ACCESS_ID")
	envOr(&h.SecretAccessKey, "S3_MAPPING_SECRET_KEY")

	if h.CacheTTL == 0 {
		if s := os.Getenv("S3_MAPPING_CACHE_TTL"); s != "" {
			if d, err := caddy.ParseDuration(s); err == nil {
				h.CacheTTL = caddy.Duration(d)
			}
		}
	}
	if h.NegCacheTTL == 0 {
		if s := os.Getenv("S3_MAPPING_NEGATIVE_CACHE_TTL"); s != "" {
			if d, err := caddy.ParseDuration(s); err == nil {
				h.NegCacheTTL = caddy.Duration(d)
			}
		}
	}
	if !h.SPAFallback {
		if b, err := strconv.ParseBool(os.Getenv("S3_MAPPING_SPA_FALLBACK")); err == nil {
			h.SPAFallback = b
		}
	}
	if !h.UseIamProvider {
		if b, err := strconv.ParseBool(os.Getenv("S3_USE_IAM_PROVIDER")); err == nil {
			h.UseIamProvider = b
		}
	}
}

func (h *S3MappingHandler) Validate() error {
	if h.pool == nil || h.s3 == nil || h.cache == nil {
		return fmt.Errorf("s3_mapping: not properly provisioned")
	}
	return nil
}

func (h *S3MappingHandler) Cleanup() error {
	if h.pool != nil {
		h.pool.Close()
	}
	return nil
}

// ServeHTTP resolves the domain, builds a safe S3 key, and streams the object.
func (h *S3MappingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	host := normalizeHost(r.Host)
	if host == "" {
		return caddyhttp.Error(http.StatusBadRequest, fmt.Errorf("missing Host header"))
	}

	mappingID, found, err := h.lookupMapping(r.Context(), host)
	if err != nil {
		h.logger.Error("domain lookup failed", zap.String("host", host), zap.Error(err))
		return caddyhttp.Error(http.StatusBadGateway, err)
	}
	if !found {
		h.logger.Info("hostname not mapped", zap.String("host", host))
		return caddyhttp.Error(http.StatusNotFound, fmt.Errorf("domain %q not mapped", host))
	}
	h.logger.Info("hostname mapped",
		zap.String("host", host),
		zap.String("mapping_id", mappingID),
	)

	objectKey, err := buildObjectKey(mappingID, r.URL.Path)
	if err != nil {
		return caddyhttp.Error(http.StatusBadRequest, err)
	}

	resp, err := h.s3.getObject(r.Context(), objectKey)
	if err != nil {
		h.logger.Error("s3 fetch failed", zap.String("key", objectKey), zap.Error(err))
		return caddyhttp.Error(http.StatusBadGateway, err)
	}

	// SPA fallback: on S3 404 for an HTML-navigation request, serve index.html instead.
	if resp.StatusCode == http.StatusNotFound && h.SPAFallback && looksLikeNavigation(r) {
		resp.Body.Close()
		indexKey := mappingID + "/index.html"
		resp, err = h.s3.getObject(r.Context(), indexKey)
		if err != nil {
			return caddyhttp.Error(http.StatusBadGateway, err)
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return caddyhttp.Error(http.StatusNotFound, fmt.Errorf("object not found: %s", objectKey))
	}
	if resp.StatusCode != http.StatusOK {
		h.logger.Warn("s3 non-200",
			zap.String("key", objectKey),
			zap.Int("status", resp.StatusCode),
		)
		return caddyhttp.Error(http.StatusBadGateway, fmt.Errorf("upstream returned %d", resp.StatusCode))
	}

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, resp.Body); err != nil {
		h.logger.Debug("streaming body", zap.Error(err))
	}
	return nil
}

// lookupMapping resolves host → mapping ID, using the in-memory cache first.
func (h *S3MappingHandler) lookupMapping(ctx context.Context, host string) (string, bool, error) {
	if mappingID, found, hit := h.cache.get(host); hit {
		return mappingID, found, nil
	}

	var mappingID string
	var dbTTL *int64

	row := h.pool.QueryRow(ctx, h.query, host)

	if h.CacheTTLColumn != "" {
		var ttlVal *int64
		if err := row.Scan(&mappingID, &ttlVal); err != nil {
			if err == pgx.ErrNoRows {
				h.cacheNegative(host)
				return "", false, nil
			}
			return "", false, fmt.Errorf("query domain mapping: %w", err)
		}
		dbTTL = ttlVal
	} else {
		if err := row.Scan(&mappingID); err != nil {
			if err == pgx.ErrNoRows {
				h.cacheNegative(host)
				return "", false, nil
			}
			return "", false, fmt.Errorf("query domain mapping: %w", err)
		}
	}

	ttl := resolveTTL(dbTTL, time.Duration(h.CacheTTL))
	h.cache.set(host, mappingID, true, ttl)
	return mappingID, true, nil
}

func (h *S3MappingHandler) cacheNegative(host string) {
	ttl := time.Duration(h.NegCacheTTL)
	if ttl <= 0 {
		ttl = resolveTTL(nil, time.Duration(h.CacheTTL))
	}
	h.cache.set(host, "", false, ttl)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func redactDatabaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid database_url)"
	}
	return u.Redacted()
}

func normalizeHost(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	return strings.ToLower(strings.TrimSpace(h))
}

// buildObjectKey constructs a safe S3 key under the mapping prefix.
// Root path → {mappingID}/index.html. Path traversal is rejected.
func buildObjectKey(mappingID, reqPath string) (string, error) {
	cleaned := path.Clean("/" + reqPath)

	segments := strings.Split(cleaned, "/")
	var safe []string
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("path traversal not allowed")
		}
		safe = append(safe, seg)
	}

	if len(safe) == 0 {
		return mappingID + "/index.html", nil
	}

	key := mappingID + "/" + strings.Join(safe, "/")
	if !strings.HasPrefix(key, mappingID+"/") {
		return "", fmt.Errorf("path escaped mapping prefix")
	}
	return key, nil
}

func looksLikeNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false
	}
	ext := path.Ext(r.URL.Path)
	return ext == "" || ext == ".html"
}

// copyHeaders copies a curated set of upstream response headers.
func copyHeaders(dst, src http.Header) {
	for _, k := range []string{
		"Content-Type", "Content-Length", "ETag",
		"Cache-Control", "Last-Modified", "Content-Encoding",
		"Content-Disposition", "Accept-Ranges",
	} {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

// Interface guards.
var (
	_ caddy.Module                = (*S3MappingHandler)(nil)
	_ caddy.Provisioner           = (*S3MappingHandler)(nil)
	_ caddy.Validator             = (*S3MappingHandler)(nil)
	_ caddy.CleanerUpper          = (*S3MappingHandler)(nil)
	_ caddyhttp.MiddlewareHandler = (*S3MappingHandler)(nil)
)
