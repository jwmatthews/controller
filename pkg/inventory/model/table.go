package model

import (
	"bytes"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	liberr "github.com/konveyor/controller/pkg/error"
	"github.com/mattn/go-sqlite3"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

const (
	Tag = "sql"
)

//
// DDL templates.
var TableDDL = `
CREATE TABLE IF NOT EXISTS {{.Table}} (
{{ range $i,$f := .Fields -}}
{{ if $i }},{{ end -}}
{{ $f.DDL }}
{{ end -}}
{{ range $i,$c := .Constraints -}}
,{{ $c }}
{{ end -}}
);
`

var IndexDDL = `
CREATE INDEX IF NOT EXISTS {{.Table}}Index
ON {{.Table}}
(
{{ range $i,$f := .Fields -}}
{{ if $i }},{{ end -}}
{{ $f.Name }}
{{ end -}}
);
`

//
// SQL templates.
var InsertSQL = `
INSERT INTO {{.Table}} (
{{ range $i,$f := .Fields -}}
{{ if $i}},{{ end -}}
{{ $f.Name }}
{{ end -}}
)
VALUES (
{{ range $i,$f := .Fields -}}
{{ if $i }},{{ end -}}
{{ $f.Param }}
{{ end -}}
);
`

var UpdateSQL = `
UPDATE {{.Table}}
SET
{{ range $i,$f := .Fields -}}
{{ if $i }},{{ end -}}
{{ $f.Name }} = {{ $f.Param }}
{{ end -}}
WHERE
{{ .Pk.Name }} = {{ .Pk.Param }}
;
`

var DeleteSQL = `
DELETE FROM {{.Table}}
WHERE
{{ .Pk.Name }} = {{ .Pk.Param }}
;
`

var GetSQL = `
SELECT
{{ range $i,$f := .Fields -}}
{{ if $i }},{{ end -}}
{{ $f.Name }}
{{ end -}}
FROM {{.Table}}
WHERE
{{ .Pk.Name }} = {{ .Pk.Param }}
;
`

var ListSQL = `
SELECT
{{ if .Count -}}
COUNT(*)
{{ else -}}
{{ range $i,$f := .Options.Fields -}}
{{ if $i }},{{ end -}}
{{ $f.Name }}
{{ end -}}
{{ end -}}
FROM {{.Table}}
{{ if or .Predicate -}}
WHERE
{{ end -}}
{{ if .Predicate -}}
{{ .Predicate.Expr }}
{{ end -}}
{{ if .Sort -}}
ORDER BY
{{ range $i,$n := .Sort -}}
{{ if $i }},{{ end }}{{ $n }}
{{ end -}}
{{ end -}}
{{ if .Page -}}
LIMIT {{.Page.Limit}} OFFSET {{.Page.Offset}}
{{ end -}}
;
`

//
// Errors
var (
	// Must have PK.
	MustHavePkErr = errors.New("must have PK field")
	// Parameter must be pointer error.
	MustBePtrErr = errors.New("must be pointer")
	// Must be slice pointer.
	MustBeSlicePtrErr = errors.New("must be slice pointer")
	// Parameter must be struct error.
	MustBeObjectErr = errors.New("must be object")
	// Field type error.
	FieldTypeErr = errors.New("field type must be (int, str, bool")
	// PK field type error.
	PkTypeErr = errors.New("pk field must be (int, str)")
	// Generated PK error.
	GenPkTypeErr = errors.New("PK field must be `str` when generated")
	// Invalid field referenced in predicate.
	PredicateRefErr = errors.New("predicate referenced unknown field")
	// Invalid predicate for type of field.
	PredicateTypeErr = errors.New("predicate type not valid for field")
	// Invalid predicate value.
	PredicateValueErr = errors.New("predicate value not valid")
)

//
// Represents a table in the DB.
// Using reflect, the model is inspected to determine the
// table name and columns. The column definition is specified
// using field tags:
//   pk - Primary key.
//   key - Natural key.
//   fk:<table>(field) - Foreign key.
//   unique(<group>) - Unique constraint collated by <group>.
//   const - Not updated.
type Table struct {
	// Database connection.
	DB DBTX
}

//
// Get the table name for the model.
func (t Table) Name(model interface{}) string {
	mt := reflect.TypeOf(model)
	if mt.Kind() == reflect.Ptr {
		mt = mt.Elem()
	}

	return mt.Name()
}

//
// Validate the model.
func (t Table) Validate(fields []*Field) error {
	for _, f := range fields {
		err := f.Validate()
		if err != nil {
			return liberr.Wrap(err)
		}
	}
	pk := t.PkField(fields)
	if pk == nil {
		return liberr.Wrap(MustHavePkErr)
	}

	return nil
}

//
// Get table and index create DDL.
func (t Table) DDL(model interface{}) ([]string, error) {
	list := []string{}
	tpl := template.New("")
	fields, err := t.Fields(model)
	if err != nil {
		return nil, liberr.Wrap(err)
	}
	err = t.Validate(fields)
	if err != nil {
		return nil, liberr.Wrap(err)
	}
	// Table
	tpl, err = tpl.Parse(TableDDL)
	if err != nil {
		return nil, liberr.Wrap(err)
	}
	constraints := t.Constraints(fields)
	bfr := &bytes.Buffer{}
	err = tpl.Execute(
		bfr,
		TmplData{
			Table:       t.Name(model),
			Fields:      t.RealFields(fields),
			Constraints: constraints,
		})
	if err != nil {
		return nil, liberr.Wrap(err)
	}
	list = append(list, bfr.String())
	// Index.
	fields = t.KeyFields(fields)
	if len(fields) > 0 {
		tpl, err = tpl.Parse(IndexDDL)
		if err != nil {
			return nil, liberr.Wrap(err)
		}
		bfr = &bytes.Buffer{}
		err = tpl.Execute(
			bfr,
			TmplData{
				Table:  t.Name(model),
				Fields: t.RealFields(fields),
			})
		if err != nil {
			return nil, liberr.Wrap(err)
		}
		list = append(list, bfr.String())
	}

	return list, nil
}

//
// Insert the model in the DB.
// Expects the primary key (PK) to be set.
func (t Table) Insert(model interface{}) error {
	fields, err := t.Fields(model)
	if err != nil {
		return liberr.Wrap(err)
	}
	t.SetPk(fields)
	stmt, err := t.insertSQL(t.Name(model), fields)
	if err != nil {
		return liberr.Wrap(err)
	}
	params := t.Params(fields)
	r, err := t.DB.Exec(stmt, params...)
	if err != nil {
		if sql3Err, cast := err.(sqlite3.Error); cast {
			if sql3Err.Code == sqlite3.ErrConstraint {
				return t.Update(model)
			}
		}
		return liberr.Wrap(err)
	}
	_, err = r.RowsAffected()
	if err != nil {
		return liberr.Wrap(err)
	}

	return nil
}

//
// Update the model in the DB.
// Expects the primary key (PK) or natural keys to be set.
func (t Table) Update(model interface{}) error {
	fields, err := t.Fields(model)
	if err != nil {
		return liberr.Wrap(err)
	}
	t.SetPk(fields)
	stmt, err := t.updateSQL(t.Name(model), fields)
	if err != nil {
		return liberr.Wrap(err)
	}
	params := t.Params(fields)
	r, err := t.DB.Exec(stmt, params...)
	if err != nil {
		return liberr.Wrap(err)
	}
	nRows, err := r.RowsAffected()
	if err != nil {
		return liberr.Wrap(err)
	}
	if nRows == 0 {
		return liberr.Wrap(NotFound)
	}

	return nil
}

//
// Delete the model in the DB.
// Expects the primary key (PK) or natural keys to be set.
func (t Table) Delete(model interface{}) error {
	fields, err := t.Fields(model)
	if err != nil {
		return liberr.Wrap(err)
	}
	t.SetPk(fields)
	stmt, err := t.deleteSQL(t.Name(model), fields)
	if err != nil {
		return liberr.Wrap(err)
	}
	params := t.Params(fields)
	r, err := t.DB.Exec(stmt, params...)
	if err != nil {
		return liberr.Wrap(err)
	}
	nRows, err := r.RowsAffected()
	if err != nil {
		return liberr.Wrap(err)
	}
	if nRows == 0 {
		return nil
	}

	return nil
}

//
// Get the model in the DB.
// Expects the primary key (PK) or natural keys to be set.
// Fetch the row and populate the fields in the model.
func (t Table) Get(model interface{}) error {
	fields, err := t.Fields(model)
	if err != nil {
		return liberr.Wrap(err)
	}
	t.SetPk(fields)
	stmt, err := t.getSQL(t.Name(model), fields)
	if err != nil {
		return liberr.Wrap(err)
	}
	params := t.Params(fields)
	row := t.DB.QueryRow(stmt, params...)
	err = t.scan(row, fields)

	return liberr.Wrap(err)
}

//
// List the model in the DB.
// Qualified by the list options.
func (t Table) List(list interface{}, options ListOptions) error {
	var model interface{}
	lt := reflect.TypeOf(list)
	lv := reflect.ValueOf(list)
	switch lt.Kind() {
	case reflect.Ptr:
		lt = lt.Elem()
		lv = lv.Elem()
	default:
		return liberr.Wrap(MustBeSlicePtrErr)
	}
	switch lt.Kind() {
	case reflect.Slice:
		model = reflect.New(lt.Elem()).Interface()
	default:
		return liberr.Wrap(MustBeSlicePtrErr)
	}
	fields, err := t.Fields(model)
	if err != nil {
		return liberr.Wrap(err)
	}
	stmt, err := t.listSQL(t.Name(model), fields, &options)
	if err != nil {
		return liberr.Wrap(err)
	}
	params := options.Params()
	cursor, err := t.DB.Query(stmt, params...)
	if err != nil {
		return liberr.Wrap(err)
	}
	defer cursor.Close()
	mList := reflect.MakeSlice(lt, 0, 0)
	for cursor.Next() {
		mt := reflect.TypeOf(model)
		mPtr := reflect.New(mt.Elem())
		mInt := mPtr.Interface()
		mFields, _ := t.Fields(mInt)
		options.fields = mFields
		err = t.scan(cursor, options.Fields())
		if err != nil {
			return liberr.Wrap(err)
		}
		mList = reflect.Append(mList, mPtr.Elem())
	}

	lv.Set(mList)

	return nil
}

//
// Count the models in the DB.
// Qualified by the model field values and list options.
// Expects natural keys to be set.
// Else, ALL models counted.
func (t Table) Count(model interface{}, predicate Predicate) (int64, error) {
	fields, err := t.Fields(model)
	if err != nil {
		return 0, liberr.Wrap(err)
	}
	options := ListOptions{Predicate: predicate}
	stmt, err := t.countSQL(t.Name(model), fields, &options)
	if err != nil {
		return 0, liberr.Wrap(err)
	}
	count := int64(0)
	params := options.Params()
	row := t.DB.QueryRow(stmt, params...)
	if err != nil {
		return 0, liberr.Wrap(err)
	}
	err = row.Scan(&count)
	if err != nil {
		return 0, liberr.Wrap(err)
	}

	return count, nil
}

//
// Get the `Fields` for the model.
func (t Table) Fields(model interface{}) ([]*Field, error) {
	fields := []*Field{}
	mt := reflect.TypeOf(model)
	mv := reflect.ValueOf(model)
	if mt.Kind() == reflect.Ptr {
		mt = mt.Elem()
		mv = mv.Elem()
	} else {
		return nil, liberr.Wrap(MustBePtrErr)
	}
	if mv.Kind() != reflect.Struct {
		return nil, liberr.Wrap(MustBeObjectErr)
	}
	for i := 0; i < mt.NumField(); i++ {
		ft := mt.Field(i)
		fv := mv.Field(i)
		if !fv.CanSet() {
			continue
		}
		switch fv.Kind() {
		case reflect.Struct:
			sqlTag, found := ft.Tag.Lookup(Tag)
			if !found {
				nested, err := t.Fields(fv.Addr().Interface())
				if err != nil {
					return nil, nil
				}
				fields = append(fields, nested...)
			} else {
				fields = append(
					fields,
					&Field{
						Tag:   sqlTag,
						Name:  ft.Name,
						Value: &fv,
					})
			}
		case reflect.Slice,
			reflect.Map,
			reflect.String,
			reflect.Bool,
			reflect.Int,
			reflect.Int8,
			reflect.Int16,
			reflect.Int32,
			reflect.Int64:
			sqlTag, found := ft.Tag.Lookup(Tag)
			if !found {
				continue
			}
			fields = append(
				fields,
				&Field{
					Tag:   sqlTag,
					Name:  ft.Name,
					Value: &fv,
				})
		}
	}

	return fields, nil
}

//
// Get the `Fields` referenced as param in SQL.
func (t Table) Params(fields []*Field) []interface{} {
	list := []interface{}{}
	for _, f := range fields {
		if f.isParam {
			p := sql.Named(f.Name, f.Pull())
			list = append(list, p)
		}
	}

	return list
}

//
// Set PK
// Generated when not already set as sha1
// of the (const) natural keys.
func (t Table) SetPk(fields []*Field) error {
	pk := t.PkField(fields)
	if pk == nil {
		return nil
	}
	switch pk.Value.Kind() {
	case reflect.String:
		if pk.Pull() != "" {
			return nil
		}
	default:
		return liberr.Wrap(GenPkTypeErr)
	}
	h := sha1.New()
	for _, f := range t.KeyFields(fields) {
		f.Pull()
		switch f.Value.Kind() {
		case reflect.String:
			h.Write([]byte(f.string))
		case reflect.Bool,
			reflect.Int,
			reflect.Int8,
			reflect.Int16,
			reflect.Int32,
			reflect.Int64:
			bfr := new(bytes.Buffer)
			binary.Write(bfr, binary.BigEndian, f.int)
			h.Write(bfr.Bytes())
		}
	}
	pk.string = hex.EncodeToString(h.Sum(nil))
	pk.Push()
	return nil
}

//
// Get the mutable `Fields` for the model.
func (t Table) MutableFields(fields []*Field) []*Field {
	list := []*Field{}
	for _, f := range fields {
		if f.Mutable() {
			list = append(list, f)
		}
	}

	return list
}

//
// Get the natural key `Fields` for the model.
func (t Table) KeyFields(fields []*Field) []*Field {
	list := []*Field{}
	for _, f := range fields {
		if f.Key() {
			list = append(list, f)
		}
	}

	return list
}

//
// Get the non-virtual `Fields` for the model.
func (t Table) RealFields(fields []*Field) []*Field {
	list := []*Field{}
	for _, f := range fields {
		if !f.Virtual() {
			list = append(list, f)
		}
	}

	return list
}

//
// Get the PK field.
func (t Table) PkField(fields []*Field) *Field {
	for _, f := range fields {
		if f.Pk() {
			return f
		}
	}

	return nil
}

//
// Get constraint DDL.
func (t Table) Constraints(fields []*Field) []string {
	constraints := []string{}
	unique := map[string][]string{}
	for _, field := range fields {
		for _, name := range field.Unique() {
			list, found := unique[name]
			if found {
				unique[name] = append(list, field.Name)
			} else {
				unique[name] = []string{field.Name}
			}
		}
	}
	for _, list := range unique {
		constraints = append(
			constraints,
			fmt.Sprintf(
				"UNIQUE (%s)",
				strings.Join(list, ",")))
	}
	for _, field := range fields {
		fk := field.Fk()
		if fk == nil {
			continue
		}
		constraints = append(constraints, fk.DDL(field))
	}

	return constraints
}

//
// Build model insert SQL.
func (t Table) insertSQL(table string, fields []*Field) (string, error) {
	tpl := template.New("")
	tpl, err := tpl.Parse(InsertSQL)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	bfr := &bytes.Buffer{}
	err = tpl.Execute(
		bfr,
		TmplData{
			Table:  table,
			Fields: t.RealFields(fields),
		})
	if err != nil {
		return "", liberr.Wrap(err)
	}

	return bfr.String(), nil
}

//
// Build model update SQL.
func (t Table) updateSQL(table string, fields []*Field) (string, error) {
	tpl := template.New("")
	tpl, err := tpl.Parse(UpdateSQL)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	bfr := &bytes.Buffer{}
	err = tpl.Execute(
		bfr,
		TmplData{
			Table:  table,
			Fields: t.MutableFields(fields),
			Pk:     t.PkField(fields),
		})
	if err != nil {
		return "", liberr.Wrap(err)
	}

	return bfr.String(), nil
}

//
// Build model delete SQL.
func (t Table) deleteSQL(table string, fields []*Field) (string, error) {
	tpl := template.New("")
	tpl, err := tpl.Parse(DeleteSQL)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	bfr := &bytes.Buffer{}
	err = tpl.Execute(
		bfr,
		TmplData{
			Table: table,
			Pk:    t.PkField(fields),
		})
	if err != nil {
		return "", liberr.Wrap(err)
	}

	return bfr.String(), nil
}

//
// Build model get SQL.
func (t Table) getSQL(table string, fields []*Field) (string, error) {
	tpl := template.New("")
	tpl, err := tpl.Parse(GetSQL)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	bfr := &bytes.Buffer{}
	err = tpl.Execute(
		bfr,
		TmplData{
			Table:  table,
			Pk:     t.PkField(fields),
			Fields: fields,
		})
	if err != nil {
		return "", liberr.Wrap(err)
	}

	return bfr.String(), nil
}

//
// Build model list SQL.
func (t Table) listSQL(table string, fields []*Field, options *ListOptions) (string, error) {
	tpl := template.New("")
	tpl, err := tpl.Parse(ListSQL)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	err = options.Build(table, fields)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	bfr := &bytes.Buffer{}
	err = tpl.Execute(
		bfr,
		TmplData{
			Table:   table,
			Fields:  fields,
			Options: options,
			Pk:      t.PkField(fields),
		})
	if err != nil {
		return "", liberr.Wrap(err)
	}

	return bfr.String(), nil
}

//
// Build model count SQL.
func (t Table) countSQL(table string, fields []*Field, options *ListOptions) (string, error) {
	tpl := template.New("")
	tpl, err := tpl.Parse(ListSQL)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	err = options.Build(table, fields)
	if err != nil {
		return "", liberr.Wrap(err)
	}
	bfr := &bytes.Buffer{}
	err = tpl.Execute(
		bfr,
		TmplData{
			Table:   table,
			Fields:  fields,
			Options: options,
			Count:   true,
			Pk:      t.PkField(fields),
		})
	if err != nil {
		return "", liberr.Wrap(err)
	}

	return bfr.String(), nil
}

//
// Scan the fetch row into the model.
// The model fields are updated.
func (t Table) scan(row Row, fields []*Field) error {
	list := []interface{}{}
	for _, f := range fields {
		f.Pull()
		list = append(list, f.Ptr())
	}
	err := row.Scan(list...)
	if err == nil {
		for _, f := range fields {
			f.Push()
		}
	}

	return liberr.Wrap(err)
}

//
// Regex used for `unique(group)` tags.
var UniqueRegex = regexp.MustCompile(`(unique)(\()(.+)(\))`)

//
// Regex used for `fk:<table>(field)` tags.
var FkRegex = regexp.MustCompile(`(fk):(.+)(\()(.+)(\))`)

//
// Model (struct) Field
// Tags:
//   `sql:"pk"`
//       The primary key.
//   `sql:"key"`
//       The field is part of the natural key.
//   `sql:"fk:T(F)"`
//       Foreign key `T` = model type, `F` = model field.
//   `sql:"unique(G)"`
//       Unique index. `G` = unique-together fields.
//   `sql:"const"`
//       The field is immutable and not included on update.
//
type Field struct {
	// reflect.Value of the field.
	Value *reflect.Value
	// Tags.
	Tag string
	// Field name.
	Name string
	// Staging (string) values.
	string string
	// Staging (int) values.
	int int64
	// Referenced as a parameter.
	isParam bool
}

//
// Validate.
func (f *Field) Validate() error {
	switch f.Value.Kind() {
	case reflect.String,
		reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64:
	default:
		if f.Pk() {
			return liberr.Wrap(PkTypeErr)
		}
	}

	return nil
}

//
// Pull from model.
// Populate the appropriate `staging` field using the
// model field value.
func (f *Field) Pull() interface{} {
	switch f.Value.Kind() {
	case reflect.Struct:
		object := f.Value.Interface()
		b, err := json.Marshal(&object)
		if err == nil {
			f.string = string(b)
		}
		return f.string
	case reflect.Slice:
		if !f.Value.IsNil() {
			object := f.Value.Interface()
			b, err := json.Marshal(&object)
			if err == nil {
				f.string = string(b)
			}
		} else {
			f.string = "[]"
		}
		return f.string
	case reflect.Map:
		if !f.Value.IsNil() {
			object := f.Value.Interface()
			b, err := json.Marshal(&object)
			if err == nil {
				f.string = string(b)
			}
		} else {
			f.string = "{}"
		}
		return f.string
	case reflect.String:
		f.string = f.Value.String()
		return f.string
	case reflect.Bool:
		b := f.Value.Bool()
		if b {
			f.int = 1
		}
		return f.int
	case reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64:
		f.int = f.Value.Int()
		return f.int
	}

	return nil
}

//
// Pointer used for Scan().
func (f *Field) Ptr() interface{} {
	switch f.Value.Kind() {
	case reflect.Bool,
		reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64:
		return &f.int
	default:
		return &f.string
	}
}

//
// Push to the model.
// Set the model field value using the `staging` field.
func (f *Field) Push() {
	switch f.Value.Kind() {
	case reflect.Struct:
		if len(f.string) == 0 {
			break
		}
		tv := reflect.New(f.Value.Type())
		object := tv.Interface()
		err := json.Unmarshal([]byte(f.string), &object)
		if err == nil {
			tv = reflect.ValueOf(object)
			f.Value.Set(tv.Elem())
		}
	case reflect.Slice,
		reflect.Map:
		if len(f.string) == 0 {
			break
		}
		tv := reflect.New(f.Value.Type())
		object := tv.Interface()
		err := json.Unmarshal([]byte(f.string), object)
		if err == nil {
			tv = reflect.ValueOf(object)
			tv = reflect.Indirect(tv)
			f.Value.Set(tv)
		}
	case reflect.String:
		f.Value.SetString(f.string)
	case reflect.Bool:
		b := false
		if f.int != 0 {
			b = true
		}
		f.Value.SetBool(b)
	case reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64:
		f.Value.SetInt(f.int)
	}
}

//
// Column DDL.
func (f *Field) DDL() string {
	part := []string{
		f.Name, // name
		"",     // type
		"",     // constraint
	}
	switch f.Value.Kind() {
	case reflect.Bool,
		reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64:
		part[1] = "INTEGER"
	default:
		part[1] = "TEXT"
	}
	if f.Pk() {
		part[2] = "PRIMARY KEY"
	} else {
		part[2] = "NOT NULL"
	}

	return strings.Join(part, " ")
}

//
// Get as SQL param.
func (f *Field) Param() string {
	f.isParam = true
	return ":" + f.Name
}

//
// Get whether field is the primary key.
func (f *Field) Pk() bool {
	return f.hasOpt("pk")
}

//
// Get whether field is mutable.
// Only mutable fields will be updated.
func (f *Field) Mutable() bool {
	if f.Pk() || f.Key() || f.Virtual() {
		return false
	}

	return !f.hasOpt("const")
}

//
// Get whether field is a natural key.
func (f *Field) Key() bool {
	return f.hasOpt("key")
}

//
// Get whether field is virtual.
// A `virtual` field is read-only and managed
// internally in the DB.
func (f *Field) Virtual() bool {
	return f.hasOpt("virtual")
}

//
// Get whether the field is unique.
func (f *Field) Unique() []string {
	list := []string{}
	for _, opt := range strings.Split(f.Tag, ",") {
		opt = strings.TrimSpace(opt)
		m := UniqueRegex.FindStringSubmatch(opt)
		if m != nil && len(m) == 5 {
			list = append(list, m[3])
		}
	}

	return list
}

//
// Get whether the field is a foreign key.
func (f *Field) Fk() *FK {
	for _, opt := range strings.Split(f.Tag, ",") {
		opt = strings.TrimSpace(opt)
		m := FkRegex.FindStringSubmatch(opt)
		if m != nil && len(m) == 6 {
			return &FK{
				Table: m[2],
				Field: m[4],
			}
		}
	}

	return nil
}

// Convert the specified `object` to a value
// (type) appropriate for the field.
func (f *Field) AsValue(object interface{}) (value interface{}, err error) {
	val := reflect.ValueOf(object)
	switch val.Kind() {
	case reflect.Ptr:
		val = val.Elem()
	case reflect.Struct,
		reflect.Slice,
		reflect.Map:
		err = liberr.Wrap(PredicateValueErr)
		return
	}
	switch f.Value.Kind() {
	case reflect.String:
		switch val.Kind() {
		case reflect.String:
			value = val.String()
		case reflect.Bool:
			b := val.Bool()
			value = strconv.FormatBool(b)
		case reflect.Int,
			reflect.Int8,
			reflect.Int16,
			reflect.Int32,
			reflect.Int64:
			n := val.Int()
			value = strconv.FormatInt(n, 0)
		default:
			err = liberr.Wrap(PredicateValueErr)
		}
	case reflect.Bool:
		switch val.Kind() {
		case reflect.String:
			s := val.String()
			b, pErr := strconv.ParseBool(s)
			if pErr != nil {
				err = liberr.Wrap(pErr)
				return
			}
			value = b
		case reflect.Bool:
			value = val.Bool()
		case reflect.Int,
			reflect.Int8,
			reflect.Int16,
			reflect.Int32,
			reflect.Int64:
			n := val.Int()
			if n != 0 {
				value = true
			} else {
				value = false
			}
		default:
			err = liberr.Wrap(PredicateValueErr)
		}
	case reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64:
		switch val.Kind() {
		case reflect.String:
			n, err := strconv.ParseInt(val.String(), 0, 64)
			if err != nil {
				err = liberr.Wrap(err)
			}
			value = n
		case reflect.Bool:
			if val.Bool() {
				value = 1
			} else {
				value = 0
			}
		case reflect.Int,
			reflect.Int8,
			reflect.Int16,
			reflect.Int32,
			reflect.Int64:
			value = val.Int()
		default:
			err = liberr.Wrap(PredicateValueErr)
		}
	default:
		err = liberr.Wrap(FieldTypeErr)
	}

	return
}

//
// Get whether the field is `json` encoded.
func (f *Field) Encoded() (encoded bool) {
	switch f.Value.Kind() {
	case reflect.Struct,
		reflect.Slice,
		reflect.Map:
		encoded = true
	}

	return
}

//
// Detail level.
func (f *Field) Detail() (level int) {
	for n := 0; n < 10; n++ {
		if f.hasOpt(fmt.Sprintf("d%d", n)) {
			return n
		}
	}
	level = 2
	if f.Pk() || f.Key() || f.Virtual() {
		level = 0
	}
	if f.Encoded() {
		level = 3
	}

	return
}

//
// Match detail level.
func (f *Field) MatchDetail(level int) bool {
	if level == 1 { // ALL
		return true
	}

	return f.Detail() <= level
}

//
// Get whether field has an option.
func (f *Field) hasOpt(name string) bool {
	for _, opt := range strings.Split(f.Tag, ",") {
		opt = strings.TrimSpace(opt)
		if opt == name {
			return true
		}
	}

	return false
}

//
// FK constraint.
type FK struct {
	// Table name.
	Table string
	// Field name.
	Field string
}

//
// Get DDL.
func (f *FK) DDL(field *Field) string {
	return fmt.Sprintf(
		"FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE CASCADE",
		field.Name,
		f.Table,
		f.Field)
}

//
// Template data.
type TmplData struct {
	// Table name.
	Table string
	// Fields.
	Fields []*Field
	// Constraint DDL.
	Constraints []string
	// Natural key fields.
	Keys []*Field
	// Primary key.
	Pk *Field
	// List options.
	Options *ListOptions
	// Count
	Count bool
}

//
// Predicate
func (t TmplData) Predicate() Predicate {
	return t.Options.Predicate
}

//
// Pagination.
func (t TmplData) Page() *Page {
	return t.Options.Page
}

//
// Sort criteria
func (t TmplData) Sort() []int {
	return t.Options.Sort
}

//
// List options.
type ListOptions struct {
	// Pagination.
	Page *Page
	// Sort by field position.
	Sort []int
	// Field detail level.
	//   0 = core: pk; key and virtual fields.
	//   1 = all fields.
	//   2 = plain fields.
	//   3 = encoded fields.
	Detail int
	// Predicate
	Predicate Predicate
	// Table (name).
	table string
	// Fields.
	fields []*Field
	// Params.
	params []interface{}
}

//
// Validate options.
func (l *ListOptions) Build(table string, fields []*Field) error {
	l.table = table
	l.fields = fields
	if l.Predicate == nil {
		return nil
	}
	err := l.Predicate.Build(l)
	if err != nil {
		return liberr.Wrap(err)
	}

	return nil
}

//
// Get an appropriate parameter name.
// Builds a parameter and adds it to the options.param list.
func (l *ListOptions) Param(name string, value interface{}) string {
	name = fmt.Sprintf("%s%d", name, len(l.params))
	l.params = append(l.params, sql.Named(name, value))
	return ":" + name
}

//
// Fields filtered by detail level.
func (l *ListOptions) Fields() (filtered []*Field) {
	for _, f := range l.fields {
		if f.MatchDetail(l.Detail) {
			filtered = append(filtered, f)
		}
	}

	return
}

//
// Get params referenced by the predicate.
func (l *ListOptions) Params() []interface{} {
	return l.params
}
