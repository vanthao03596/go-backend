package app_test

import (
	"slices"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/vanthao03596/go-backend/internal/app"
	"github.com/vanthao03596/go-backend/internal/config"
)

// TestOpenAPIBuildsWithoutDependencies guards the "api openapi" export: it
// assembles the application with no database or cache and must still
// describe every operation.
func TestOpenAPIBuildsWithoutDependencies(t *testing.T) {
	spec := app.OpenAPI(config.Defaults())

	var ids []string
	for _, item := range spec.Paths {
		for _, op := range []string{
			opID(item.Get), opID(item.Post), opID(item.Delete), opID(item.Put), opID(item.Patch),
		} {
			if op != "" {
				ids = append(ids, op)
			}
		}
	}
	slices.Sort(ids)
	want := []string{"create-link", "delete-link", "get-link", "get-link-stats", "list-links"}
	if !slices.Equal(ids, want) {
		t.Errorf("operations = %v, want %v", ids, want)
	}
	if len(spec.Servers) != 1 || spec.Servers[0].URL != "http://localhost:8080/api/v1" {
		t.Errorf("servers = %+v, want the default BASE_URL", spec.Servers)
	}
	if _, err := spec.YAML(); err != nil {
		t.Fatalf("render YAML: %v", err)
	}
}

func opID(op *huma.Operation) string {
	if op == nil {
		return ""
	}
	return op.OperationID
}
