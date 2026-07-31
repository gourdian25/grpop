// File: contract_helpers_test.go

package grpop

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// contractCollectionName derives a Mongo collection name from the
// currently-executing subtest, and cleanupContractCollection (registered
// alongside every call site) drops it at test end.
//
// t.Name() alone is NOT sufficient for isolation: it's stable *within* one
// `go test` invocation (avoiding collisions between subtests running in the
// same process) but identical *across* separate invocations, so without an
// explicit drop, a real MongoDB instance accumulates every prior run's
// documents under the same collection name.
func contractCollectionName(t *testing.T) string {
	t.Helper()
	name := "contract_" + t.Name()
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, " ", "_")
	return name
}

// cleanupContractCollection registers a t.Cleanup that drops the named
// Mongo collection, via its own short-lived connection decoupled from the
// store's lifecycle — see cleanupContractRows' doc comment for why a defer
// in the test body closing the store cannot be relied on to have already
// run: t.Cleanup callbacks fire in LIFO order after the test function
// returns, but a `defer store.Close()` inside that same function runs
// before the function returns, which is earlier still — reusing the
// store's own (by-then-closed) connection here would silently no-op.
func cleanupContractCollection(t *testing.T, database, collection string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		client, err := mongo.Connect(ctx, options.Client().ApplyURI(testMongoURI))
		if err != nil {
			return
		}
		defer client.Disconnect(ctx)
		if err := client.Database(database).Collection(collection).Drop(ctx); err != nil {
			t.Logf("cleanupContractCollection: %v", err)
		}
	})
}

// cleanupContractRows registers a t.Cleanup that deletes rows whose
// idColumn starts with "contract-" from table, scoped so it never touches
// data owned by the individual (non-contract) backend test files sharing
// the same live Postgres instance. Deliberately opens its own short-lived
// pool rather than taking the store's — see cleanupContractCollection's
// doc comment for why reusing the store's own connection would silently
// no-op once the test body's own `defer store.Close()` has already run.
func cleanupContractRows(t *testing.T, table, idColumn string) {
	t.Helper()
	t.Cleanup(func() {
		pool, err := pgxpool.New(context.Background(), testPostgresDSN)
		if err != nil {
			return
		}
		defer pool.Close()
		if _, err := pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE "+idColumn+" LIKE 'contract-%'"); err != nil {
			t.Logf("cleanupContractRows: %v", err)
		}
	})
}
