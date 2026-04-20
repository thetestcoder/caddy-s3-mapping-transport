package s3mapping

import (
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("s3_mapping", parseCaddyfile)
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m S3MappingHandler
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return &m, nil
}

// UnmarshalCaddyfile parses the s3_mapping { ... } block.
// Every directive here is optional; environment variables fill the gaps.
func (h *S3MappingHandler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume "s3_mapping"

	for d.NextBlock(0) {
		switch d.Val() {
		case "database_url":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.DatabaseURL = d.Val()

		case "table":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.Table = d.Val()

		case "domain_column":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.DomainColumn = d.Val()

		case "id_column":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.IDColumn = d.Val()

		case "cache_ttl_column":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.CacheTTLColumn = d.Val()

		case "bucket":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.Bucket = d.Val()

		case "region":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.Region = d.Val()

		case "cache_ttl":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid cache_ttl: %v", err)
			}
			h.CacheTTL = caddy.Duration(dur)

		case "negative_cache_ttl":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid negative_cache_ttl: %v", err)
			}
			h.NegCacheTTL = caddy.Duration(dur)

		case "spa_fallback":
			h.SPAFallback = true
			if d.NextArg() {
				b, err := strconv.ParseBool(d.Val())
				if err != nil {
					return d.Errf("spa_fallback: invalid value %q", d.Val())
				}
				h.SPAFallback = b
			}

		case "use_iam_provider":
			h.UseIamProvider = true
			if d.NextArg() {
				b, err := strconv.ParseBool(d.Val())
				if err != nil {
					return d.Errf("use_iam_provider: invalid value %q", d.Val())
				}
				h.UseIamProvider = b
			}

		case "access_id":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.AccessKeyID = d.Val()

		case "secret_key":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.SecretAccessKey = d.Val()

		default:
			return d.Errf("unknown subdirective %q", d.Val())
		}
	}
	return nil
}

var _ caddyfile.Unmarshaler = (*S3MappingHandler)(nil)
