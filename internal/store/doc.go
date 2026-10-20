// Package store owns the relational SQLite data model, migrations, and
// transactional event and outbox persistence.
//
// Use Tx methods inside a Store.Tx callback. Tx.Context carries a marker so
// nested Store calls using it return ErrTxReentry instead of waiting for the
// transaction's single connection.
package store
