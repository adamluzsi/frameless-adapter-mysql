package mysql_test

import (
	"context"
	"strings"
	"testing"

	"go.llib.dev/testcase"
	"go.llib.dev/testcase/assert"
	"go.llib.dev/testcase/random"

	"go.llib.dev/frameless/adapter/memory"
	"go.llib.dev/frameless/adapter/mysql"
	"go.llib.dev/frameless/pkg/cache"
	"go.llib.dev/frameless/pkg/cache/cachecontracts"
	"go.llib.dev/frameless/pkg/flsql"
	"go.llib.dev/frameless/pkg/logger"
	"go.llib.dev/frameless/pkg/zerokit"
	"go.llib.dev/frameless/port/crud/crudcontracts"
	"go.llib.dev/frameless/spechelper/testent"
)

func TestRepository_cacheEntityRepository(t *testing.T) {
	cm := GetConnection(t)
	MigrateFooCache(t, cm)

	conf := cachecontracts.Config[testent.Foo, testent.FooID]{
		CRUD: crudcontracts.Config[testent.Foo, testent.FooID]{
			MakeEntity: func(t testing.TB) testent.Foo {
				f := testent.MakeFoo(t)
				f.ID = testent.FooID(t.(*testcase.T).Random.UUID())
				return f
			},
		},
	}

	cacheRepository := FooCacheRepository{Connection: cm}
	cachecontracts.EntityRepository[testent.Foo, testent.FooID](cacheRepository.Entities(), cm, conf)
	cachecontracts.HitRepository[testent.FooID](cacheRepository.Hits(), cm)
	cachecontracts.Repository(cacheRepository, conf).Test(t)
}

func TestRepository_cacheHitRepository(t *testing.T) {
	cm := GetConnection(t)
	MigrateFooCache(t, cm)
	chcRepo := FooCacheRepository{Connection: cm}
	cachecontracts.HitRepository[testent.FooID](chcRepo.Hits(), cm)
}

func TestRepository_cacheCache(t *testing.T) {
	logger.Testing(t)

	cm := GetConnection(t)
	MigrateFooCache(t, cm)

	src := memory.NewRepository[testent.Foo, testent.FooID](memory.NewMemory())
	chcRepo := FooCacheRepository{Connection: cm}

	chc := &cache.Cache[testent.Foo, testent.FooID]{
		Source:                  src,
		Repository:              chcRepo,
		CachedQueryInvalidators: nil,
	}

	cachecontracts.Cache[testent.Foo, testent.FooID](chc, src, chcRepo).Test(t)
}

func splitQuery(query string) []string {
	var queries []string
	for _, q := range strings.Split(query, ";") {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		queries = append(queries, q)
	}
	return queries
}

func MigrateFooCache(tb testing.TB, c flsql.Connection) {
	ctx := context.Background()

	for _, query := range splitQuery(FooCacheMigrateDOWN) {
		_, err := c.ExecContext(ctx, query)
		assert.Nil(tb, err)
	}

	for _, query := range splitQuery(FooCacheMigrateUP) {
		_, err := c.ExecContext(ctx, query)
		assert.Nil(tb, err)
	}

	tb.Cleanup(func() {
		for _, query := range splitQuery(FooCacheMigrateDOWN) {
			_, err := c.ExecContext(ctx, query)
			assert.Nil(tb, err)
		}
	})
}

const FooCacheMigrateUP = `
CREATE TABLE IF NOT EXISTS cache_foos (
    id  VARCHAR(255) NOT NULL PRIMARY KEY,
	foo LONGTEXT     NOT NULL,
	bar LONGTEXT     NOT NULL,
	baz LONGTEXT     NOT NULL
);

CREATE TABLE IF NOT EXISTS cache_foo_hits (
	id  VARCHAR(255) NOT NULL PRIMARY KEY,
	ids JSON NOT     NULL,
	ts  TIMESTAMP    NOT NULL
);
`

const FooCacheMigrateDOWN = `
DROP TABLE IF EXISTS cache_foos;
DROP TABLE IF EXISTS cache_foo_hits;
`

type FooCacheRepository struct{ mysql.Connection }

func (cr FooCacheRepository) Entities() cache.EntityRepository[testent.Foo, testent.FooID] {
	return mysql.Repository[testent.Foo, testent.FooID]{
		Mapping: flsql.Mapping[testent.Foo, testent.FooID]{
			TableName: "cache_foos",

			QueryID: func(id testent.FooID) (flsql.QueryArgs, error) {
				return flsql.QueryArgs{"id": id}, nil
			},

			ToQuery: func(ctx context.Context) ([]flsql.ColumnName, flsql.MapScan[testent.Foo]) {
				return []flsql.ColumnName{"id", "foo", "bar", "baz"}, func(foo *testent.Foo, s flsql.Scanner) error {
					return s.Scan(&foo.ID, &foo.Foo, &foo.Bar, &foo.Baz)
				}
			},

			ToArgs: func(foo testent.Foo) (flsql.QueryArgs, error) {
				return flsql.QueryArgs{
					"id":  foo.ID,
					"foo": foo.Foo,
					"bar": foo.Bar,
					"baz": foo.Baz,
				}, nil
			},

			CreatePrepare: func(ctx context.Context, f *testent.Foo) error {
				if zerokit.IsZero(f.ID) {
					f.ID = testent.FooID(random.New(random.CryptoSeed{}).UUID())
				}
				return nil
			},

			ID: func(f *testent.Foo) *testent.FooID {
				return &f.ID
			},
		},
		Connection: cr.Connection,
	}
}

func (cr FooCacheRepository) Hits() cache.HitRepository[testent.FooID] {
	return mysql.Repository[cache.Hit[testent.FooID], cache.HitID]{
		Mapping: flsql.Mapping[cache.Hit[testent.FooID], cache.HitID]{
			TableName: "cache_foo_hits",

			QueryID: func(id string) (flsql.QueryArgs, error) {
				return flsql.QueryArgs{"id": id}, nil
			},

			ToArgs: func(h cache.Hit[testent.FooID]) (flsql.QueryArgs, error) {
				return flsql.QueryArgs{
					"id":  h.QueryID,
					"ids": mysql.JSON(&h.EntityIDs),
					"ts":  mysql.Timestamp(&h.Timestamp),
				}, nil
			},

			ToQuery: func(ctx context.Context) ([]flsql.ColumnName, flsql.MapScan[cache.Hit[testent.FooID]]) {
				return []flsql.ColumnName{"id", "ids", "ts"}, func(v *cache.Hit[testent.FooID], s flsql.Scanner) error {
					err := s.Scan(&v.QueryID, mysql.JSON(&v.EntityIDs), mysql.Timestamp(&v.Timestamp))
					if err != nil {
						return err
					}
					if !v.Timestamp.IsZero() {
						v.Timestamp = v.Timestamp.UTC()
					}
					return err
				}
			},

			ID: func(h *cache.Hit[testent.FooID]) *cache.HitID {
				return &h.QueryID
			},
		},
		Connection: cr.Connection,
	}
}
