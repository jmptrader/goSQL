package dbx

import (
	"database/sql"
	"fmt"
	"reflect"

	tk "github.com/quintans/toolkit"
	coll "github.com/quintans/toolkit/collection"
	"github.com/quintans/toolkit/log"
)

var logger = log.LoggerFor("github.com/quintans/goSQL/dbx")

// Class that simplifies the execution o Database Access
type SimpleDBA struct {
	// The connection to execute the query in.
	connection IConnection
}

func NewSimpleDBA(connection IConnection) *SimpleDBA {
	this := new(SimpleDBA)
	this.connection = connection
	return this
}

func closeResources(rows *sql.Rows, stmt *sql.Stmt) error {
	var err error
	if rows != nil {
		err = rows.Close()
		if err != nil {
			return err
		}
	}

	if stmt != nil {
		err = stmt.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

func (this *SimpleDBA) fetchRows(sql string, params ...interface{}) (*sql.Rows, *sql.Stmt, error) {
	stmt, err := this.connection.Prepare(sql)
	if err != nil {
		logger.Errorf("%T.fetchRows PREPARE %s", this, err)
		return nil, nil, rethrow(FAULT_PREP_STATEMENT, err, sql, params...)
	}

	rows, err := stmt.Query(params...)
	if err != nil {
		stmt.Close()
		logger.Errorf("%T.fetchRows QUERY %s: %s %s", this, err, sql, params)
		return nil, nil, rethrow(FAULT_QUERY, err, sql, params...)
	}

	return rows, stmt, nil
}

// Execute an SQL SELECT with named replacement parameters.<br>
// The caller is responsible for closing the connection.
//
// param sql: The query to execute.
// param params: The replacement parameters.
// param rt: The handler that converts the results into an object.
// return The Collection returned by the handler and a Fail if a database access error occurs
func (this *SimpleDBA) QueryCollection(
	sql string,
	rt IRowTransformer,
	params ...interface{},
) (coll.Collection, error) {
	rows, stmt, fail := this.fetchRows(sql, params...)
	if fail != nil {
		return nil, fail
	}
	defer closeResources(rows, stmt)

	result := rt.BeforeAll()
	defer rt.AfterAll(result)

	for rows.Next() {
		instance, err := rt.Transform(rows)
		if err != nil {
			return nil, rethrow(FAULT_TRANSFORM, err, sql, params...)
		}
		rt.OnTransformation(result, instance)
	}

	return result, nil
}

func (this *SimpleDBA) Query(
	sql string,
	transformer func(rows *sql.Rows) (interface{}, error),
	params ...interface{},
) ([]interface{}, error) {
	rows, stmt, fail := this.fetchRows(sql, params...)
	if fail != nil {
		return nil, fail
	}
	defer closeResources(rows, stmt)

	results := make([]interface{}, 0, 10)
	for rows.Next() {
		result, err := transformer(rows)
		if err != nil {
			return nil, rethrow(FAULT_PARSE_STATEMENT, err, sql, params...)
		}
		results = append(results, result)
	}

	return results, nil
}

// the transformer will be responsible for creating  the result list
func (this *SimpleDBA) QueryClosure(
	query string,
	transformer func(rows *sql.Rows) error,
	params ...interface{},
) error {
	rows, stmt, fail := this.fetchRows(query, params...)
	if fail != nil {
		return fail
	}
	defer closeResources(rows, stmt)

	for rows.Next() {
		err := transformer(rows)
		if err != nil {
			return rethrow(FAULT_PARSE_STATEMENT, err, query, params...)
		}
	}

	return nil
}

//List using the closure arguments.
//A function is used to build the result list.
//The types for scanning are supplied by the function arguments. Arguments can be pointers or not.
//Reflection is used to determine the arguments types.
//
//ex:
//  roles = make([]string, 0)
//  var role string
//  q.QueryInto(func(role *string) {
//	  roles = append(roles, *role)
//  })
func (this *SimpleDBA) QueryInto(
	query string,
	closure interface{},
	params ...interface{},
) ([]interface{}, error) {
	// determine types and instanciate them
	ftype := reflect.TypeOf(closure)
	if ftype.Kind() != reflect.Func {
		return nil, fmt.Errorf("goSQL: Expected a function with the signature func(primitive1, ..., primitiveN) [anything]. Got %s.", ftype.String())
	}

	size := ftype.NumIn() // number of input variables
	instances := make([]interface{}, size)
	targets := make([]reflect.Type, size)
	for i := 0; i < size; i++ {
		arg := ftype.In(i) // type of input variable i
		targets[i] = arg   // collects the target types
		// the scan elements must be all pointers
		if arg.Kind() == reflect.Ptr {
			// Instanciates a pointer. Interface() returns the pointer instance.
			instances[i] = reflect.New(arg).Interface()
		} else {
			// creates a pointer of the type of the zero type
			instances[i] = reflect.New(reflect.PtrTo(arg)).Interface()
		}
	}

	var results []interface{}
	// output must be at most 1
	if ftype.NumOut() > 1 {
		return nil, fmt.Errorf("goSQL: A function must have at most one output. Got %s outputs.", ftype.NumOut())
	} else if ftype.NumOut() == 1 {
		results = make([]interface{}, 0)
	}

	err := this.QueryClosure(query, func(rows *sql.Rows) error {
		err := rows.Scan(instances...)
		if err != nil {
			return err
		}
		values := make([]reflect.Value, size)
		for k, v := range instances {
			// Elem() gets the underlying object of the interface{}
			e := reflect.ValueOf(v).Elem()
			if targets[k].Kind() == reflect.Ptr {
				// if pointer type use directly
				values[k] = e
			} else {
				if e.IsNil() {
					// was nil, so we must create its zero value
					values[k] = reflect.Zero(targets[k])
				} else {
					// use underlying value of the pointer
					values[k] = e.Elem()
				}
			}
		}
		res := reflect.ValueOf(closure).Call(values)
		if results != nil { // expects result. ftype.NumOut() == 1
			results = append(results, res[0].Interface())
		}
		return nil
	}, params...)

	if err != nil {
		return nil, err
	}
	return results, nil
}

// Execute an SQL SELECT query with named parameters returning the first result.
//
// param <T>
//            the result object type
// param conn
//            The connection to execute the query in.
// param sql
//            The query to execute.
// param rt
//            The handler that converts the results into an object.
// param params
//            The named parameters.
// @return The transformed result
func (this *SimpleDBA) QueryFirst(
	sql string,
	params map[string]interface{},
	rt IRowTransformer,
) (interface{}, error) {
	result, fail1 := this.QueryCollection(sql, rt, params)
	if fail1 != nil {
		return nil, fail1
	}

	if result.Size() > 0 {
		return result.Enumerator().Next(), nil
	}
	return nil, nil
}

// Execute an SQL SELECT query with named parameters returning the first result.
//
// param conn
//            The connection to execute the query in.
// param sql
//            The query to execute.
// param params
//            The named parameters.
// @return if there was a row scan and error
func (this *SimpleDBA) QueryRow(
	sql string,
	params []interface{},
	dest ...interface{},
) (bool, error) {
	rows, stmt, err := this.fetchRows(sql, params...)
	if err != nil {
		return false, err
	}
	defer closeResources(rows, stmt)

	var ok bool
	if rows.Next() {
		err = rows.Scan(dest...)
		if err != nil {
			return false, err
		}
		ok = true
	}

	return ok, nil
}

////////////////////////////////////////////////////////////////////////

// Execute an SQL INSERT, UPDATE, or DELETE query.
//
// param conn
//            The connection to use to run the query.
// param sql
//            The SQL to execute.
// param params
//            The query replacement parameters.
// @return The number of rows affected.
func (this *SimpleDBA) execute(sql string, params ...interface{}) (sql.Result, *sql.Stmt, error) {
	stmt, err := this.connection.Prepare(sql)
	if err != nil {
		return nil, nil, rethrow(FAULT_PREP_STATEMENT, err, sql, params...)
	}

	result, err := stmt.Exec(params...)
	if err != nil {
		stmt.Close()
		return nil, nil, rethrow(FAULT_EXEC_STATEMENT, err, sql, params...)
	}

	return result, stmt, nil
}

///**
// Execute an SQL INSERT, UPDATE, or DELETE query.
//
// param conn
//            The connection to use to run the query.
// param sql
//            The SQL to execute.
// param params
//            The query named parameters.
// @return The number of rows affected.
// */
func (this *SimpleDBA) Update(sql string, params ...interface{}) (int64, error) {
	result, stmt, err := this.execute(sql, params...)
	if err != nil {
		return 0, err
	}
	defer closeResources(nil, stmt)
	return result.RowsAffected()
}

func (this *SimpleDBA) Delete(sql string, params ...interface{}) (int64, error) {
	return this.Update(sql, params...)
}

func (this *SimpleDBA) Insert(sql string, params ...interface{}) (int64, error) {
	_, stmt, err := this.execute(sql, params...)
	if err != nil {
		return 0, err
	}
	defer closeResources(nil, stmt)
	// not supported in all drivers (ex: pq)
	// return result.LastInsertId()
	return 0, nil
}

func (this *SimpleDBA) InsertReturning(sql string, params ...interface{}) (int64, error) {
	var id int64
	_, err := this.QueryRow(sql, params, &id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Throws a new exception with a more informative error message.
//
// param cause
//            The original exception that will be chained to the new
//            exception when it's rethrown.
//
// param sql
//            The query that was executing when the exception happened.
//
// param params
//            The query replacement parameters; <code>nil</code> is a
//            valid value to pass in.

func rethrow(code string, cause error, sql string, params ...interface{}) error {
	causeMessage := cause.Error()

	msg := tk.NewStrBuffer(causeMessage, "\nSQL: ", sql, "\nParameters: ")
	if params != nil {
		msg.Add(fmt.Sprintf("%v", params))
	}

	return NewPersistenceFail(code, msg.String())
}
