package mysql_test

import (
	"context"
	"fmt"
	"testing"

	"go.llib.dev/frameless/adapter/mysql"
	"go.llib.dev/frameless/adapter/mysql/internal/queries"
	"go.llib.dev/frameless/pkg/cache/cachecontracts"
	"go.llib.dev/frameless/pkg/dtokit"
	"go.llib.dev/frameless/pkg/flsql"
	"go.llib.dev/frameless/pkg/logger"
	"go.llib.dev/frameless/port/crud/crudcontracts"
	"go.llib.dev/frameless/port/migration"
	"go.llib.dev/frameless/port/migration/migrationcontracts"
	"go.llib.dev/frameless/spechelper/testent"
	"go.llib.dev/testcase"
	"go.llib.dev/testcase/assert"
)

func TestRepository(t *testing.T) {
	logger.Testing(t)

	cm := GetConnection(t)

	subject := &mysql.Repository[Entity, EntityID]{
		Connection: cm,
		Mapping:    EntityMapping(),
	}

	MigrateEntity(t, cm)

	config := crudcontracts.Config[Entity, EntityID]{
		MakeContext:     context.Background,
		SupportIDReuse:  true,
		SupportRecreate: true,
		ChangeEntity:    nil, // test entity can be freely changed
	}

	testcase.RunSuite(t,
		crudcontracts.Creator[Entity, EntityID](subject, config),
		crudcontracts.Finder[Entity, EntityID](subject, config),
		crudcontracts.Updater[Entity, EntityID](subject, config),
		crudcontracts.Deleter[Entity, EntityID](subject, config),
		crudcontracts.OnePhaseCommitProtocol[Entity, EntityID](subject, subject.Connection),
	)
}

func TestCacheRepository(t *testing.T) {
	// logger.Testing(t)
	subject := mysql.CacheRepository[testent.Foo, testent.FooID]{
		Connection: GetConnection(t),
		ID:         "foo",
		JSONDTOM:   testent.FooJSONMapping(),
		IDA: func(f *testent.Foo) *testent.FooID {
			return &f.ID
		},
		IDM: dtokit.Mapping[testent.FooID, string]{
			ToENT: func(ctx context.Context, dto string) (testent.FooID, error) {
				return testent.FooID(dto), nil
			},
			ToDTO: func(ctx context.Context, ent testent.FooID) (string, error) {
				return ent.String(), nil
			},
		},
	}
	assert.NoError(t, subject.Migrate(context.Background()))
	c := cachecontracts.Config[testent.Foo, testent.FooID]{
		CRUD: crudcontracts.Config[testent.Foo, testent.FooID]{
			MakeEntity: func(tb testing.TB) testent.Foo {
				foo := testent.MakeFoo(tb)
				foo.ID = testent.FooID(testcase.ToT(&tb).Random.UUID())
				return foo
			},
		},
	}
	cachecontracts.Repository(subject, c).Test(t)
}

func TestMigrationStateRepository(t *testing.T) {
	logger.Testing(t)
	ctx := context.Background()
	conn := GetConnection(t)

	repo := mysql.MakeMigrationStateRepository(conn)
	repo.Mapping.TableName += "_test"

	_, err := conn.ExecContext(ctx, fmt.Sprintf(queries.CreateTableSchemaMigrationsTmpl, repo.Mapping.TableName))
	assert.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.ExecContext(ctx, fmt.Sprintf(queries.DropTableTmpl, repo.Mapping.TableName)) })

	migrationcontracts.StateRepository(repo).Test(t)
}

func TestMigrationStateRepository_smoke(t *testing.T) {
	logger.Testing(t)
	ctx := context.Background()
	conn := GetConnection(t)

	repo := mysql.MakeMigrationStateRepository(conn)
	repo.Mapping.TableName += "_test"

	_, err := conn.ExecContext(ctx, fmt.Sprintf(queries.CreateTableSchemaMigrationsTmpl, repo.Mapping.TableName))
	assert.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.ExecContext(ctx, fmt.Sprintf(queries.DropTableTmpl, repo.Mapping.TableName)) })

	ent1 := migration.State{
		ID: migration.StateID{
			Namespace: "ns",
			Version:   "0",
		},
		Dirty: false,
	}

	assert.NoError(t, repo.Create(ctx, &ent1))

	gotEnt1, found, err := repo.FindByID(ctx, ent1.ID)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, gotEnt1, ent1)

	assert.NoError(t, repo.DeleteByID(ctx, ent1.ID))

	assert.NoError(t, repo.Create(ctx, &ent1))
	assert.NoError(t, repo.DeleteAll(ctx))
}

func TestRepository_intIDDefaultBehaviourWithoutMapping(t *testing.T) {
	logger.Testing(t)
	ctx := context.Background()
	conn := GetConnection(t)

	const tableName = "test_auto_inc_id_table"
	const CreateTableSQL = `CREATE TABLE IF NOT EXISTS %s ( id INT AUTO_INCREMENT, val TEXT, PRIMARY KEY (id) );`
	_, err := conn.ExecContext(ctx, fmt.Sprintf(CreateTableSQL, tableName))
	assert.NoError(t, err)
	defer conn.ExecContext(ctx, fmt.Sprintf(queries.DropTableTmpl, tableName))

	type E struct {
		ID  int `ext:"id"`
		Val string
	}

	repo := mysql.Repository[E, int]{
		Connection: conn,
		Mapping: flsql.Mapping[E, int]{
			TableName: tableName,
			ToQuery: func(ctx context.Context) ([]flsql.ColumnName, flsql.MapScan[E]) {
				return []flsql.ColumnName{"id", "val"},
					func(v *E, s flsql.Scanner) error {
						return s.Scan(&v.ID, &v.Val)
					}
			},
			QueryID: func(id int) (flsql.QueryArgs, error) {
				return flsql.QueryArgs{
					"id": id,
				}, nil
			},
			ToArgs: func(e E) (flsql.QueryArgs, error) {
				return flsql.QueryArgs{
					"id":  e.ID,
					"val": e.Val,
				}, nil
			},
			ID: func(e *E) *int {
				return &e.ID
			},
		},
	}

	var v = E{Val: "42"}
	assert.NoError(t, repo.Create(ctx, &v))
	assert.NotEmpty(t, v.ID)

	got, found, err := repo.FindByID(ctx, v.ID)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, got, v)
}
