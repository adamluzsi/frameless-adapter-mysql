package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"go.llib.dev/frameless/pkg/contextkit"
	"go.llib.dev/frameless/pkg/errorkit"
	"go.llib.dev/frameless/pkg/flsql"
	"go.llib.dev/frameless/pkg/logger"
	"go.llib.dev/frameless/pkg/logging"
	"go.llib.dev/frameless/pkg/slicekit"
	"go.llib.dev/frameless/port/comproto"
	"go.llib.dev/frameless/port/crud"
	"go.llib.dev/frameless/port/iterators"
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

	id, isIDOK := r.Mapping.LookupID(*ptr)
	if isIDOK {
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
		flsql.JoinColumnName(cols, ", ", "`%s`"),
		strings.Join(valueClause, ", "),
		flsql.JoinColumnName(rcolumns, ", ", "`%s`"),
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

	var idWhereClause []string
	for col, arg := range idArgs {
		idWhereClause = append(idWhereClause, fmt.Sprintf("`%s` = ?", col))
		queryArgs = append(queryArgs, arg)
	}

	query := fmt.Sprintf("SELECT %s FROM `%s` WHERE %s",
		flsql.JoinColumnName(cols, ", ", "`%s`"),
		r.Mapping.TableName,
		strings.Join(idWhereClause, ", "),
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

	query := fmt.Sprintf("DELETE FROM `%s` WHERE %s",
		r.Mapping.TableName,
		r.buildWhereClause(idArgs),
	)

	ctx, err = r.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer comproto.FinishOnePhaseCommit(&rErr, r, ctx)

	result, err := r.Connection.ExecContext(ctx, query, r.getArgsFromMap(idArgs)...)
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

	id, ok := r.Mapping.LookupID(*ptr)
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
	whereClause := r.buildWhereClause(idArgs)
	args := append(values, r.getArgsFromMap(idArgs)...)

	// Corrected part: Removed the `setClause` variable and directly inserted the mapped columns into the query.
	query := fmt.Sprintf("UPDATE `%s` SET %s WHERE %s",
		r.Mapping.TableName,
		strings.Join(slicekit.Map(cols, func(c flsql.ColumnName) string { return fmt.Sprintf("`%s` = ?", c) }), ", "),
		whereClause,
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
		flsql.JoinColumnName(cols, ", ", "`%s`"),
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
		whereClauses = append(whereClauses, fmt.Sprintf("(%s)", r.buildWhereClause(idArgs)))
		queryArgs = append(queryArgs, r.getArgsFromMap(idArgs)...)
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

func (r Repository[Entity, ID]) buildWhereClause(args map[flsql.ColumnName]interface{}) string {
	var whereClauses []string
	for col := range args {
		whereClauses = append(whereClauses, fmt.Sprintf("`%s` = ?", col))
	}
	return strings.Join(whereClauses, " AND ")
}

func (r Repository[Entity, ID]) getArgsFromMap(args map[flsql.ColumnName]interface{}) []interface{} {
	values := make([]interface{}, 0, len(args))
	for _, value := range args {
		values = append(values, value)
	}
	return values
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

		if _, ok := r.Mapping.LookupID(*ptr); !ok && r.Mapping.CreatePrepare != nil {
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
			flsql.JoinColumnName(rcolumns, ", ", "`%s`"),
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
