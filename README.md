# MySQL Adapter Package

This package provides a MySQL adapter for the Frameless.
It allows you to interact with MySQL databases using a standardized interface.

## **Features**

* Connection management: Establish and manage connections to MySQL databases.
* CRUD operations: Perform create, read, update, and delete operations on MySQL tables.
* Transaction support: Use transactions to ensure atomicity and consistency of database operations.
* Migration support: Use migrations to manage schema changes and versioning of your database.

**Getting Started**

To use this package, you need to install it using Go's package manager:
```bash
go get go.llib.dev/frameless/adapter/mysql
```

Then, import the package in your Go program:
```go
import "go.llib.dev/frameless/adapter/mysql"
```

Create a connection to a MySQL database using the `Connect` function:
```go
conn, err := mysql.Connect("user:password@tcp(localhost:3306)/database")
```

Use the `Repository` type to perform CRUD operations on a table:
```go
repo := mysql.Repository[Entity, ID]{
    Connection: conn,
    Mapping:    EntityMapping(),
}

// Create an entity
entity := Entity{Name: "John Doe"}
err := repo.Create(context.Background(), &entity)
```

**License**

This package is licensed under the MIT License.