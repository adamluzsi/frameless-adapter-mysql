package queries

const CreateTableSchemaMigrationsTmpl = `CREATE TABLE IF NOT EXISTS %s (
	namespace  TEXT    NOT NULL,
	version    TEXT    NOT NULL,
	dirty      BOOLEAN NOT NULL
)`

const DropTableTmpl = `DROP TABLE IF EXISTS %s`

const CreateTableCacheEntitiesTmpl = `CREATE TABLE %s (
    id VARCHAR(255) PRIMARY KEY,
    data JSON NOT NULL
)`

const CreateTableCacheHitsTmpl = `CREATE TABLE %s (
    query_id  VARCHAR(255) PRIMARY KEY,
    ent_ids   JSON NOT NULL,
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP NOT NULL
)`
