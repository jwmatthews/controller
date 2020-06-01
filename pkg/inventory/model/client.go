package model

import (
	"database/sql"
	"errors"
	"os"
	"reflect"
	"sync"
)

const (
	Pragma = "PRAGMA foreign_keys = ON"
)

//
// Tx.Commit()
// Tx.Rollback()
// Called and the transaction is not in progress by
// the associated Client.
var TxInvalidError = errors.New("transaction not valid")

//
// Database client.
type DB interface {
	// Open and build the schema.
	Open(bool) error
	// Close.
	Close(bool) error
	// Get the specified model.
	Get(Model) error
	// List models based on `selector` model.
	List(Model, ListOptions, interface{}) error
	// Begin a transaction.
	Begin() (*Tx, error)
	// Insert a model.
	Insert(Model) error
	// Update a model.
	Update(Model) error
	// Delete a model.
	Delete(Model) error
}

//
// Database client.
type Client struct {
	// Protect internal state.
	sync.Mutex
	// The sqlite3 database will not support
	// concurrent write operations.
	mutex sync.Mutex
	// file path.
	path string
	// Model
	models []interface{}
	// Database connection.
	db *sql.DB
	// Current database transaction.
	tx *sql.Tx
}

//
// Create the database.
// Build the schema to support the specified models.
// Optionally `purge` (delete) the DB first.
func (r *Client) Open(purge bool) error {
	if purge {
		os.Remove(r.path)
	}
	db, err := sql.Open("sqlite3", r.path)
	if err != nil {
		panic(err)
	}
	statements := []string{Pragma}
	r.models = append(r.models, &Label{})
	for _, m := range r.models {
		ddl, err := Table{}.DDL(m)
		if err != nil {
			panic(err)
		}
		statements = append(statements, ddl...)
	}
	for _, ddl := range statements {
		_, err = db.Exec(ddl)
		if err != nil {
			Log.Trace(err)
			db.Close()
			return err
		}
	}

	r.db = db

	Log.Info("Database opened.", "path", r.path)

	return nil
}

//
// Close the database.
// Optionally purge (delete) the DB.
func (r *Client) Close(purge bool) error {
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	if err != nil {
		Log.Trace(err)
		return err
	}
	r.db = nil
	if purge {
		os.Remove(r.path)
	}

	return nil
}

//
// Get the model.
func (r *Client) Get(model Model) error {
	return Table{r.db}.Get(model)
}

//
// List models.
func (r *Client) List(model Model, options ListOptions, list interface{}) error {
	mv := reflect.TypeOf(model)
	switch mv.Kind() {
	case reflect.Ptr:
		mv = mv.Elem()
	default:
		return nil
	}
	lv := reflect.ValueOf(list)
	lt := reflect.TypeOf(list)
	switch lt.Kind() {
	case reflect.Ptr:
		lv = lv.Elem()
		lt = lt.Elem()
	default:
		return nil
	}
	switch lv.Kind() {
	case reflect.Slice:
		l, err := Table{r.db}.List(model, options)
		if err != nil {
			Log.Trace(err)
			return err
		}
		concrete := reflect.MakeSlice(lv.Type(), 0, 0)
		for i := 0; i < len(l); i++ {
			m := reflect.ValueOf(l[i]).Elem()
			concrete = reflect.Append(concrete, m)
		}
		lv.Set(concrete)
	}

	return nil
}

//
// Begin a transaction.
// Example:
//   tx, _ := client.Begin()
//   defer tx.Rollback()
//   client.Insert(model)
//   client.Insert(model)
//   tx.Commit()
func (r *Client) Begin() (*Tx, error) {
	r.Lock()
	defer r.Unlock()
	r.mutex.Lock()
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	r.tx = tx
	return &Tx{client: r, ref: tx}, nil
}

//
// Insert the model.
func (r *Client) Insert(model Model) error {
	r.Lock()
	defer r.Unlock()
	model.SetPk()
	table := Table{}
	if r.tx == nil {
		r.mutex.Lock()
		defer r.mutex.Unlock()
		table.Db = r.db
	} else {
		table.Db = r.tx
	}

	return table.Insert(model)
}

//
// Update the model.
func (r *Client) Update(model Model) error {
	r.Lock()
	defer r.Unlock()
	model.SetPk()
	table := Table{}
	if r.tx == nil {
		r.mutex.Lock()
		defer r.mutex.Unlock()
		table.Db = r.db
	} else {
		table.Db = r.tx
	}

	return table.Update(model)
}

//
// Delete the model.
func (r *Client) Delete(model Model) error {
	r.Lock()
	defer r.Unlock()
	model.SetPk()
	table := Table{}
	if r.tx == nil {
		r.mutex.Lock()
		defer r.mutex.Unlock()
		table.Db = r.db
	} else {
		table.Db = r.tx
	}

	return table.Delete(model)
}

//
// Commit a transaction.
// This MUST be preceeded by Begin() which returns
// the `tx` transaction token.
func (r *Client) commit(tx *Tx) error {
	r.Lock()
	defer r.Unlock()
	if r.tx == nil || r.tx != tx.ref {
		return TxInvalidError
	}
	defer func() {
		r.mutex.Unlock()
		r.tx = nil
	}()
	return r.tx.Commit()
}

//
// Rollback a transaction.
// This MUST be preceeded by Begin() which returns
// the `tx` transaction token.
func (r *Client) rollback(tx *Tx) error {
	r.Lock()
	defer r.Unlock()
	if r.tx == nil || r.tx != tx.ref {
		return TxInvalidError
	}
	defer func() {
		r.mutex.Unlock()
		r.tx = nil
	}()

	return r.tx.Rollback()
}

//
// Database transaction.
type Tx struct {
	// Associated client.
	client *Client
	// Reference to sql.Tx.
	ref *sql.Tx
}

//
// Commit a transaction.
func (r *Tx) Commit() error {
	return r.client.commit(r)
}

//
// Rollback a transaction.
func (r *Tx) rollback() {
	r.client.rollback(r)
}
