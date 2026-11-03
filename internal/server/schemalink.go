package server

import (
	"path"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// withCanonicalSchemaLinks makes the `$schema` URL in Huma's object
// responses absolute against schemasURL (BASE_URL + /api/v1 + SchemasPath +
// "/"), the same URL the spec shows as the property's example, instead of
// deriving it from the request.
//
// Huma's default SchemaLinkTransformer (v2.40.0) takes the origin from
// X-Forwarded-Host, Forwarded or Host, from any peer. For a host starting
// with "localhost" it substitutes Servers[0].URL, which already ends in
// /api/v1, so the link read /api/v1/api/v1/schemas/... and answered 401. For
// any other plain-HTTP host except 127.0.0.1 it assumed https.
//
// The default transformer stays: it adds the property to the spec, sets the
// relative Link header (which is correct) and builds the wrapper type. The
// hook appended here runs after it and replaces only the URL. Huma's docs name
// managing the Transformers and OnAddOperation hooks as the way to customise
// schema links.
func withCanonicalSchemaLinks(cfg huma.Config, schemasURL string) huma.Config {
	cfg.CreateHooks = append(cfg.CreateHooks, func(c huma.Config) huma.Config {
		c.Transformers = append(c.Transformers, canonicalSchemaLink(schemasURL))
		return c
	})
	return cfg
}

// canonicalSchemaLink rewrites the value Huma's transformer returns: a
// pointer to a fresh struct whose first field is `$schema`, ending in the
// schema's file name.
func canonicalSchemaLink(schemasURL string) huma.Transformer {
	return func(_ huma.Context, _ string, v any) (any, error) {
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Pointer || rv.IsNil() {
			return v, nil
		}
		s := rv.Elem()
		if s.Kind() != reflect.Struct || s.NumField() == 0 || s.Type().Field(0).Tag.Get("json") != "$schema" {
			return v, nil
		}
		f := s.Field(0)
		if f.Kind() != reflect.String || !f.CanSet() || f.String() == "" {
			return v, nil
		}
		f.SetString(schemasURL + path.Base(f.String()))
		return v, nil
	}
}
