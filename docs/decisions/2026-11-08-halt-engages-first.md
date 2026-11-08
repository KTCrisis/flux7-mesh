# The emergency stop engages before it is written

- **Problem**: `halt.Store.Start` wrote the halt to SQLite before activating it; a failed write returned an error and no halt was in force, not even in the process that received the order. Found while testing the store's database-failure paths.
- **Decision**: engage first, persist second. On a failed write the halt is active in this process, `Start` returns it with `ErrNotShared`, `POST /halts` answers 201 with a `warning`, and every reload keeps the unwritten halt and writes it again until the database accepts it.
- **Why**: an emergency stop must not depend on the database; the cost of the change is a halt that other processes see a little later, never a halt that does not exist.
- **Where**: `halt/halt.go` (`Start`, `reload`, `ErrNotShared`), `proxy/halts.go`; test `TestHaltEngagesWhenDatabaseRefusesTheWrite` in `halt/halt_test.go`.
