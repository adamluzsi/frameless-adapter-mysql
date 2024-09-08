package mysql_test

import (
	"context"
	"testing"

	"go.llib.dev/frameless/adapter/mysql"
	"go.llib.dev/frameless/port/crud/crudcontracts"
	"go.llib.dev/testcase"
	"go.llib.dev/testcase/assert"
)

func TestRepository(t *testing.T) {
	cm, err := mysql.Connect(DatabaseDSN(t))
	assert.NoError(t, err)
	assert.NoError(t, cm.DB.Ping())
	t.Cleanup(func() { assert.NoError(t, cm.Close()) })

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

func TestRepository_Create_id(t *testing.T) {
	cm, err := mysql.Connect(DatabaseDSN(t))
	assert.NoError(t, err)
	assert.NoError(t, cm.DB.Ping())
	t.Cleanup(func() { assert.NoError(t, cm.Close()) })

	mapping := EntityMapping()
	mapping.CreatePrepare = nil // no ID injection to ensure that ID is set by the DB

	subject := &mysql.Repository[Entity, EntityID]{
		Connection: cm,
		Mapping:    mapping,
	}

	MigrateEntity(t, cm)

	ctx := context.Background()
	ent := Entity{
		Foo: rnd.String(),
		Bar: rnd.String(),
		Baz: rnd.String(),
	}

	assert.NoError(t, subject.Create(ctx, &ent))
	assert.NotEmpty(t, ent.ID, "it was expected that ID is set by the Create method")

	got, found, err := subject.FindByID(ctx, ent.ID)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, got, ent)
}

func TestRepository_Upsert_id(t *testing.T) {
	cm, err := mysql.Connect(DatabaseDSN(t))
	assert.NoError(t, err)
	assert.NoError(t, cm.DB.Ping())
	t.Cleanup(func() { assert.NoError(t, cm.Close()) })

	mapping := EntityMapping()
	mapping.CreatePrepare = nil // no ID injection to ensure that ID is set by the DB

	subject := &mysql.Repository[Entity, EntityID]{
		Connection: cm,
		Mapping:    mapping,
	}

	MigrateEntity(t, cm)

	ctx := context.Background()
	ent1 := Entity{
		Foo: rnd.String(),
		Bar: rnd.String(),
		Baz: rnd.String(),
	}
	ent2 := Entity{
		Foo: rnd.String(),
		Bar: rnd.String(),
		Baz: rnd.String(),
	}

	assert.NoError(t, subject.Upsert(ctx, &ent1, &ent2))
	assert.NotEmpty(t, ent1.ID)
	assert.NotEmpty(t, ent2.ID)

	got1, found, err := subject.FindByID(ctx, ent1.ID)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, got1, ent1)

	got2, found, err := subject.FindByID(ctx, ent2.ID)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, got2, ent2)
}
