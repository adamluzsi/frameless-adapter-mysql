package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"go.llib.dev/frameless/adapter/mysql/internal/queries"
	"go.llib.dev/frameless/pkg/cache"
	"go.llib.dev/frameless/pkg/contextkit"
	"go.llib.dev/frameless/pkg/dtokit"
	"go.llib.dev/frameless/pkg/errorkit"
	"go.llib.dev/frameless/pkg/flsql"
	"go.llib.dev/frameless/pkg/logger"
	"go.llib.dev/frameless/pkg/logging"
	"go.llib.dev/frameless/pkg/slicekit"
	"go.llib.dev/frameless/pkg/zerokit"
	"go.llib.dev/frameless/port/comproto"
	"go.llib.dev/frameless/port/crud"
	"go.llib.dev/frameless/port/crud/extid"
	"go.llib.dev/frameless/port/iterators"
	"go.llib.dev/frameless/port/migration"
)

type Connection = flsql.ConnectionAdapter[*sql.DB, *sql.Tx]

func Connect(dsn string) (Connection, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return Connection{}, err
	}
	// SetConnMaxLifetime is required to ensure connections are closed by the driver safely before connection is closed by MySQL server,
	// OS, or other middlewares. Since some middlewares close idle connections by 5 minutes,
	// we recommend timeout shorter than 5 minutes.
	// This setting helps load balancing and changing system variables too.
	db.SetConnMaxLifetime(time.Minute * 3)
	// SetMaxOpenConns is highly recommended to limit the number of connection used by the application.
	// There is no recommended limit number because it depends on application and MySQL server.
	db.SetMaxOpenConns(10)
	// SetMaxIdleConns is recommended to be set same to db.SetMaxOpenConns().
	// When it is smaller than SetMaxOpenConns(), connections can be opened and closed much more frequently than you expect.
	// Idle connections can be closed by the db.SetConnMaxLifetime().
	// If you want to close idle connections more rapidly, you can use db.SetConnMaxIdleTime() since Go 1.15.
	db.SetMaxIdleConns(10)
	return flsql.SQLConnectionAdapter(db), nil
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// Repository implements CRUD operations for a specific entity type in MySQL.
type Repository[Entity, ID any] struct {
	Connection Connection
	Mapping    flsql.Mapping[Entity, ID]
}

func (r Repository[Entity, ID]) Create(ctx context.Context, ptr *Entity) (rErr error) {
	if ptr == nil {
		return fmt.Errorf("nil entity pointer given to Create")
	}
	ctx, err := r.BeginTx(ctx)
	if err != nil {
		return err
	}
	// this will ensure that either commit or rollback is called on the transaction,
	// depending on the value of "rErr".
	defer comproto.FinishOnePhaseCommit(&rErr, r, ctx)

	if err := r.Mapping.OnCreate(ctx, ptr); err != nil {
		return err
	}

	id, ok := r.Mapping.ID.Lookup(*ptr)
	if ok {
		_, found, err := r.FindByID(ctx, id)
		if err != nil {
			return err
		}
		if found {
			return errorkit.With(crud.ErrAlreadyExists).
				Detailf(`%T already exists with id: %v`, *new(Entity), id).
				Context(ctx).
				Unwrap()
		}
	}

	args, err := r.Mapping.ToArgs(*ptr)
	if err != nil {
		return err
	}

	cols, valuesArgs := flsql.SplitArgs(args)
	valueClause := make([]string, len(cols))
	for i := range cols {
		valueClause[i] = "?"
	}

	rcolumns, mapscan := r.Mapping.ToQuery(contextkit.WithoutValues(ctx))

	query := fmt.Sprintf("INSERT INTO `%s` (%s) VALUES (%s) RETURNING %s",
		r.Mapping.TableName,
		flsql.JoinColumnName(cols, "`%s`", ", "),
		strings.Join(valueClause, ", "),
		flsql.JoinColumnName(rcolumns, "`%s`", ", "),
	)

	logger.Debug(ctx, "executing create SQL", logging.Field("query", query))

	row := r.Connection.QueryRowContext(ctx, query, valuesArgs...)

	var got Entity
	if err := mapscan(&got, row); err != nil {
		return err
	}
	*ptr = got

	return nil
}

func (r Repository[Entity, ID]) FindByID(ctx context.Context, id ID) (Entity, bool, error) {
	var queryArgs []any

	idArgs, err := r.Mapping.QueryID(id)
	if err != nil {
		return *new(Entity), false, err
	}

	cols, scan := r.Mapping.ToQuery(ctx)

	idWhereClauseCols, idWhereClauseArgs := flsql.SplitArgs(idArgs)
	queryArgs = append(queryArgs, idWhereClauseArgs...)

	query := fmt.Sprintf("SELECT %s FROM `%s` WHERE %s LIMIT 1",
		flsql.JoinColumnName(cols, "`%s`", ", "),
		r.Mapping.TableName,
		flsql.JoinColumnName(idWhereClauseCols, "`%s` = ?", " AND "),
	)

	row := r.Connection.QueryRowContext(ctx, query, queryArgs...)

	var v Entity
	err = scan(&v, row)
	if errors.Is(err, sql.ErrNoRows) {
		return *new(Entity), false, nil
	}

	if err != nil {
		return *new(Entity), false, err
	}

	return v, true, nil
}

func (r Repository[Entity, ID]) DeleteAll(ctx context.Context) (rErr error) {
	ctx, err := r.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer comproto.FinishOnePhaseCommit(&rErr, r, ctx)

	query := fmt.Sprintf("DELETE FROM `%s`", r.Mapping.TableName)

	if _, err := r.Connection.ExecContext(ctx, query); err != nil {
		return err
	}

	return nil
}

func (r Repository[Entity, ID]) DeleteByID(ctx context.Context, id ID) (rErr error) {
	idArgs, err := r.Mapping.QueryID(id)
	if err != nil {
		return err
	}

	whereClauseQuery, whereClauseArgs := r.buildWhereClause(idArgs)

	query := fmt.Sprintf("DELETE FROM `%s` WHERE %s",
		r.Mapping.TableName,
		whereClauseQuery,
	)

	ctx, err = r.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer comproto.FinishOnePhaseCommit(&rErr, r, ctx)

	result, err := r.Connection.ExecContext(ctx, query, whereClauseArgs...)
	if err != nil {
		return err
	}

	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return crud.ErrNotFound
	}

	return nil
}

func (r Repository[Entity, ID]) Update(ctx context.Context, ptr *Entity) (rErr error) {
	if ptr == nil {
		return fmt.Errorf("nil entity pointer received in Update")
	}

	id, ok := r.Mapping.ID.Lookup(*ptr)
	if !ok {
		return fmt.Errorf("missing entity ID for Update")
	}

	idArgs, err := r.Mapping.QueryID(id)
	if err != nil {
		return err
	}

	setArgs, err := r.Mapping.ToArgs(*ptr)
	if err != nil {
		return err
	}

	cols, values := flsql.SplitArgs(setArgs)
	whereClauseQuery, whereClauseArgs := r.buildWhereClause(idArgs)
	args := append(values, whereClauseArgs...)

	// Corrected part: Removed the `setClause` variable and directly inserted the mapped columns into the query.
	query := fmt.Sprintf("UPDATE `%s` SET %s WHERE %s",
		r.Mapping.TableName,
		flsql.JoinColumnName(cols, "`%s` = ?", ", "),
		whereClauseQuery,
	)

	ctx, err = r.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer comproto.FinishOnePhaseCommit(&rErr, r, ctx)

	res, err := r.Connection.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}

	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return crud.ErrNotFound
	}

	got, found, err := r.FindByID(ctx, id)
	if err != nil {
		return fmt.Errorf("error while looking up the entity: %w", err)
	}
	if !found {
		return fmt.Errorf("expected that updated entity is findable")
	}
	*ptr = got

	return nil
}

func (r Repository[Entity, ID]) FindAll(ctx context.Context) iterators.Iterator[Entity] {
	cols, scan := r.Mapping.ToQuery(ctx)

	query := fmt.Sprintf("SELECT %s FROM `%s`",
		flsql.JoinColumnName(cols, "`%s`", ", "),
		r.Mapping.TableName,
	)

	rows, err := r.Connection.QueryContext(ctx, query)
	if err != nil {
		return iterators.Error[Entity](err)
	}

	return flsql.MakeSQLRowsIterator[Entity](rows, scan)
}

func (r Repository[Entity, ID]) FindByIDs(ctx context.Context, ids ...ID) iterators.Iterator[Entity] {
	var (
		whereClauses []string
		queryArgs    []interface{}
	)

	for _, id := range ids {
		idArgs, err := r.Mapping.QueryID(id)
		if err != nil {
			return iterators.Error[Entity](err)
		}
		whereClauseQuery, whereClauseArgs := r.buildWhereClause(idArgs)
		whereClauses = append(whereClauses, fmt.Sprintf("(%s)", whereClauseQuery))
		queryArgs = append(queryArgs, whereClauseArgs...)
	}

	cols, scan := r.Mapping.ToQuery(ctx)

	query := fmt.Sprintf("SELECT `%s` FROM `%s` WHERE %s",
		strings.Join(slicekit.Map(cols, func(c flsql.ColumnName) string { return string(c) }), "`, `"),
		r.Mapping.TableName,
		strings.Join(whereClauses, " OR "),
	)

	rows, err := r.Connection.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return iterators.Error[Entity](err)
	}

	return flsql.MakeSQLRowsIterator[Entity](rows, scan)
}

// BeginTx implements the comproto.OnePhaseCommitter interface.
func (r Repository[Entity, ID]) BeginTx(ctx context.Context) (context.Context, error) {
	return r.Connection.BeginTx(ctx)
}

// CommitTx implements the comproto.OnePhaseCommitter interface.
func (r Repository[Entity, ID]) CommitTx(ctx context.Context) error {
	return r.Connection.CommitTx(ctx)
}

// RollbackTx implements the comproto.OnePhaseCommitter interface.
func (r Repository[Entity, ID]) RollbackTx(ctx context.Context) error {
	return r.Connection.RollbackTx(ctx)
}

func (r Repository[Entity, ID]) buildWhereClause(qargs flsql.QueryArgs) (string, []any) {
	cols, args := flsql.SplitArgs(qargs)
	return flsql.JoinColumnName(cols, "`%s` = ?", " AND "), args
}

// Upsert inserts new entities or updates existing ones if they already exist.
func (r Repository[Entity, ID]) Upsert(ctx context.Context, entities ...*Entity) (rErr error) {
	if len(entities) == 0 {
		return fmt.Errorf("no entities provided to Upsert")
	}

	ctx, err := r.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer comproto.FinishOnePhaseCommit(&rErr, r, ctx)

	for _, ptr := range entities {
		if ptr == nil {
			return fmt.Errorf("nil entity pointer given to Upsert")
		}

		if _, ok := r.Mapping.ID.Lookup(*ptr); !ok && r.Mapping.CreatePrepare != nil {
			// if ID is not found, we assume it was never created before, so create peparation is required.
			if err := r.Mapping.CreatePrepare(ctx, ptr); err != nil {
				return err
			}
		}

		// Prepare the entity's arguments
		args, err := r.Mapping.ToArgs(*ptr)
		if err != nil {
			return err
		}

		cols, values := flsql.SplitArgs(args)
		valuesClause := make([]string, len(cols))
		for i := range cols {
			valuesClause[i] = "?"
		}

		// Prepare update clause for ON DUPLICATE KEY UPDATE
		updateClause := strings.Join(slicekit.Map(cols, func(c flsql.ColumnName) string { return fmt.Sprintf("`%s` = VALUES(`%s`)", c, c) }), ", ")

		rcolumns, mapscan := r.Mapping.ToQuery(contextkit.WithoutValues(ctx))

		// Construct the UPSERT query
		query := fmt.Sprintf(
			"INSERT INTO `%s` (`%s`) VALUES (%s) ON DUPLICATE KEY UPDATE %s RETURNING %s",
			r.Mapping.TableName,
			strings.Join(slicekit.Map(cols, func(c flsql.ColumnName) string { return string(c) }), "`, `"),
			strings.Join(valuesClause, ", "),
			updateClause,
			flsql.JoinColumnName(rcolumns, "`%s`", ", "),
		)

		logger.Debug(ctx, "mysql Repository Upsert", logging.Fields{
			"query": query,
			"args":  values,
		})

		// Execute the query
		if err := mapscan(ptr, r.Connection.QueryRowContext(ctx, query, values...)); err != nil {
			return err
		}
	}

	return nil
}

// Timestamp is a MySQL DTO Model for the timestamp type mapping.
// Use it from your scan and argument mapping.
func Timestamp(ptr *time.Time) flsql.DTO {
	const layout = "2006-01-02 15:04:05"
	return flsql.Timestamp(ptr, layout, time.UTC)
}

func JSON[T any](ptr *T) flsql.DTO {
	return flsql.JSON[T](ptr)
}

// CacheRepository is a generic implementation for using mysql as a caching backend with `frameless/pkg/cache.Cache`.
// CacheRepository implements `cache.Repository[ENT,ID]`
type CacheRepository[ENT, ID any] struct {
	Connection Connection
	// ID [required] is unique identifier.  the table name prefix used to create the cache repository tables.
	//
	// Example:
	// 		ID: "foo"
	// 			-> "foo_cache_entities"
	//
	ID string
	// JSONDTOM [optional] is the mapping between an ENT type and a JSON DTO type,
	// which is used to encode entities within the entity repository.
	// This mapping is important because if the entity type changes during refactoring,
	// the previously cached data can still be correctly decoded using the JSON DTO.
	// This means you won’t need to delete cached data or worry about data corruption.
	// It provides a safeguard, ensuring smooth transitions without affecting stored data.
	JSONDTOM dtokit.Mapper[ENT]
	// IDA is the ID accessor, that explains how the ID field of the ENT can be accessed.
	IDA extid.Accessor[ENT, ID]
	// IDM is the mapping between ID and the string type which is used in the CacheRepository tables to represent the ID value.
	// If the ID is a string type, then this field can be ignored.
	IDM dtokit.MapperTo[ID, string]
}

func (r CacheRepository[ENT, ID]) getIDM() dtokit.MapperTo[ID, string] {
	if r.IDM != nil {
		return r.IDM
	}
	// fallback mapping logic
	return dtokit.Mapping[ID, string]{}
}

func (r CacheRepository[ENT, ID]) tableName(name string) string {
	var prefix = r.ID
	if prefix == "" {
		const format = "implementation error: missing CacheRepository.ID field (%#v)"
		panic(fmt.Errorf(format, r))
	}
	return strings.Join([]string{prefix, "cache", name}, "_")
}

func (r CacheRepository[ENT, ID]) tableNameEntities() string {
	return r.tableName("entities")
}

func (r CacheRepository[ENT, ID]) tableNameHits() string {
	return r.tableName("hits")
}

func (r CacheRepository[ENT, ID]) jsonDTOM() dtokit.Mapper[ENT] {
	return zerokit.Coalesce[dtokit.Mapper[ENT]](r.JSONDTOM, dtokit.Mapping[ENT, ENT]{})
}

func (r CacheRepository[ENT, ID]) BeginTx(ctx context.Context) (context.Context, error) {
	return r.Connection.BeginTx(ctx)
}

func (r CacheRepository[ENT, ID]) CommitTx(ctx context.Context) error {
	return r.Connection.CommitTx(ctx)
}

func (r CacheRepository[ENT, ID]) RollbackTx(ctx context.Context) error {
	return r.Connection.RollbackTx(ctx)
}

func (r CacheRepository[ENT, ID]) Migrate(ctx context.Context) error {
	entitiesTableName := r.tableNameEntities()
	hitsTableName := r.tableNameHits()
	m := MakeMigrator(r.Connection, r.tableName("migration"), migration.Steps[Connection]{
		"1": flsql.MigrationStep[Connection]{
			UpQuery:   fmt.Sprintf(queries.CreateTableCacheEntitiesTmpl, entitiesTableName),
			DownQuery: fmt.Sprintf(queries.DropTableTmpl, entitiesTableName),
		},
		"2": flsql.MigrationStep[Connection]{
			UpQuery:   fmt.Sprintf(queries.CreateTableCacheHitsTmpl, hitsTableName),
			DownQuery: fmt.Sprintf(queries.DropTableTmpl, hitsTableName),
		},
	})
	return m.Migrate(ctx)
}

func (r CacheRepository[ENT, ID]) Entities() cache.EntityRepository[ENT, ID] {
	return Repository[ENT, ID]{
		Connection: r.Connection,
		Mapping: flsql.Mapping[ENT, ID]{
			TableName: r.tableNameEntities(),
			ID:        r.IDA,
			ToQuery: func(ctx context.Context) ([]flsql.ColumnName, flsql.MapScan[ENT]) {
				return []flsql.ColumnName{"id", "data"},
					func(v *ENT, s flsql.Scanner) error {
						if v == nil {
							return fmt.Errorf("nil %T pointer given for scanning", v)
						}
						var (
							idDTO      string
							dataDTOPtr = r.jsonDTOM().NewiDTO()
						)
						if err := s.Scan(&idDTO, JSON(&dataDTOPtr)); err != nil {
							return err
						}
						id, err := r.getIDM().MapToENT(ctx, idDTO)
						if err != nil {
							return err
						}
						ent, err := r.jsonDTOM().MapFromiDTOPtr(ctx, dataDTOPtr)
						if err != nil {
							return err
						}
						*v = ent
						return r.IDA.Set(v, id)
					}
			},
			QueryID: func(id ID) (flsql.QueryArgs, error) {
				ctx := context.Background()
				idDTO, err := r.getIDM().MapToDTO(ctx, id)
				if err != nil {
					return nil, err
				}
				return flsql.QueryArgs{"id": idDTO}, nil
			},
			ToArgs: func(e ENT) (flsql.QueryArgs, error) {
				ctx := context.Background()
				id, _ := r.IDA.Lookup(e)
				idDTO, err := r.getIDM().MapToDTO(ctx, id)
				if err != nil {
					return nil, err
				}
				return flsql.QueryArgs{
					"id":   idDTO,
					"data": JSON(&e),
				}, nil
			},
		},
	}
}

func (r CacheRepository[ENT, ID]) Hits() cache.HitRepository[ID] {
	return Repository[cache.Hit[ID], cache.HitID]{
		Connection: r.Connection,
		Mapping: flsql.Mapping[cache.Hit[ID], cache.HitID]{
			TableName: r.tableNameHits(),
			ID: func(h *cache.Hit[ID]) *string {
				return &h.QueryID
			},
			ToQuery: func(ctx context.Context) ([]flsql.ColumnName, flsql.MapScan[cache.Hit[ID]]) {
				return []flsql.ColumnName{"query_id", "ent_ids", "timestamp"},
					func(v *cache.Hit[ID], s flsql.Scanner) error {
						if v == nil {
							return fmt.Errorf("nil %T was given for scanning", v)
						}
						var idDTOs []string
						if err := s.Scan(&v.QueryID, JSON(&idDTOs), Timestamp(&v.Timestamp)); err != nil {
							return err
						}
						v.EntityIDs = nil
						for _, idDTO := range idDTOs {
							id, err := r.getIDM().MapToENT(ctx, idDTO)
							if err != nil {
								return err
							}
							v.EntityIDs = append(v.EntityIDs, id)
						}
						return nil
					}
			},
			QueryID: func(id cache.HitID) (flsql.QueryArgs, error) {
				return flsql.QueryArgs{"query_id": id}, nil
			},
			ToArgs: func(h cache.Hit[ID]) (flsql.QueryArgs, error) {
				ctx := context.Background()
				var idDTOs []string
				for _, id := range h.EntityIDs {
					idDTO, err := r.getIDM().MapToDTO(ctx, id)
					if err != nil {
						return nil, err
					}
					idDTOs = append(idDTOs, idDTO)
				}
				return flsql.QueryArgs{
					"query_id":  h.QueryID,
					"ent_ids":   JSON(&idDTOs),
					"timestamp": Timestamp(&h.Timestamp),
				}, nil
			},
			CreatePrepare: func(ctx context.Context, h *cache.Hit[ID]) error {
				if h == nil {
					return fmt.Errorf("nil %T was sent for %T.Hits().Create", h, r)
				}
				if h.QueryID == "" {
					return fmt.Errorf("empty query id was given for %T", h)
				}
				return nil
			},
		},
	}
}

// migration //

func MakeMigrator(conn Connection, namespace string, steps migration.Steps[Connection]) migration.Migrator[Connection] {
	return migration.Migrator[Connection]{
		Namespace:       namespace,
		Resource:        conn,
		StateRepository: MakeMigrationStateRepository(conn),
		EnsureStateRepository: func(ctx context.Context) error {
			_, err := conn.ExecContext(ctx, fmt.Sprintf(queries.CreateTableSchemaMigrationsTmpl, tableNameSchemaMigrations))
			return err
		},
		Steps: steps,
	}
}

const tableNameSchemaMigrations = "frameless_schema_migrations"

func MakeMigrationStateRepository(conn Connection) Repository[migration.State, migration.StateID] {
	return Repository[migration.State, migration.StateID]{
		Connection: conn,
		Mapping: flsql.Mapping[migration.State, migration.StateID]{
			TableName: "frameless_schema_migrations",
			ToQuery: func(ctx context.Context) ([]flsql.ColumnName, flsql.MapScan[migration.State]) {
				return []flsql.ColumnName{"namespace", "version", "dirty"},
					func(v *migration.State, s flsql.Scanner) error {
						return s.Scan(&v.ID.Namespace, &v.ID.Version, &v.Dirty)
					}
			},
			QueryID: func(id migration.StateID) (flsql.QueryArgs, error) {
				return flsql.QueryArgs{
					"namespace": id.Namespace,
					"version":   id.Version,
				}, nil
			},

			ToArgs: func(s migration.State) (flsql.QueryArgs, error) {
				return flsql.QueryArgs{
					"namespace": s.ID.Namespace,
					"version":   s.ID.Version,
					"dirty":     s.Dirty,
				}, nil
			},

			CreatePrepare: func(ctx context.Context, s *migration.State) error {
				if s.ID.Namespace == "" {
					return fmt.Errorf("MigrationStateRepository requires a non-empty namespace for Create")
				}
				if s.ID.Version == "" {
					return fmt.Errorf("MigrationStateRepository requires a non-empty version for Create")
				}
				return nil
			},

			ID: func(s *migration.State) *migration.StateID { return &s.ID },
		},
	}
}
