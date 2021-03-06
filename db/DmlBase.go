package db

import (
	"fmt"
	"github.com/quintans/goSQL/dbx"
	tk "github.com/quintans/toolkit"
	"github.com/quintans/toolkit/log"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var lgr = log.LoggerFor("github.com/quintans/goSQL/db")

// instead of setting globaly the logger with the caller at 1,
// it is defined locally with method CallerAt()
//func init() {
//	lgr.SetCallerAt(1)
//}

type RawSql struct {
	// original sql
	OriSql string
	// the converted SQL with the Database specific placeholders
	Sql string
	// the parameters values
	Names []string
}

// Convert a Map of named parameter values to a corresponding array.
//
// return the array of values
func (this *RawSql) BuildValues(paramMap map[string]interface{}) []interface{} {
	paramArray := make([]interface{}, len(this.Names))
	var ok bool
	for i, name := range this.Names {
		paramArray[i], ok = paramMap[name]
		if !ok {
			panic(fmt.Sprintf("[%s] No value supplied for the SQL parameter '%s' for the SQL %s",
				dbx.FAULT_VALUES_STATEMENT, name, this.OriSql))
		}
	}
	return paramArray
}

func (this *RawSql) Clone() interface{} {
	other := new(RawSql)
	other.OriSql = this.OriSql
	other.Sql = this.Sql
	if this.Names != nil {
		other.Names = make([]string, len(this.Names))
		copy(other.Names, this.Names)
	}
	return other
}

type PathCriteria struct {
	Criterias []*Criteria
	Columns   []Tokener
}

const JOIN_PREFIX = "j"
const PREFIX = "t"

type DmlBase struct {
	db IDb

	table                  *Table
	tableAlias             string
	joins                  []*Join
	criteria               *Criteria
	parameters             map[string]interface{}
	joinBag                *AliasBag
	lastFkAlias            string
	lastJoin               *Join
	discriminatorCriterias []*Criteria
	rawIndex               int
	// stores the paths (associations) already traveled
	cachedAssociation [][]*PathElement
	// list with the associations of the current path
	path []*PathElement

	rawSQL *RawSql
	dba    *dbx.SimpleDBA
}

func NewDmlBase(DB IDb, table *Table) *DmlBase {
	this := new(DmlBase)
	this.Super(DB, table)
	return this
}

func (this *DmlBase) Super(DB IDb, table *Table) {
	this.db = DB
	this.table = table
	this.alias(PREFIX + "0")

	if table != nil {
		this.discriminatorCriterias = table.GetCriterias()
	}
	this.parameters = make(map[string]interface{})

	this.dba = dbx.NewSimpleDBA(DB.GetConnection())
}

func (this *DmlBase) NextRawIndex() int {
	this.rawIndex++
	return this.rawIndex
}

func (this *DmlBase) GetDb() IDb {
	return this.db
}

func (this *DmlBase) GetDba() *dbx.SimpleDBA {
	return this.dba
}

func (this *DmlBase) GetTable() *Table {
	return this.table
}

func (this *DmlBase) GetTableAlias() string {
	return this.tableAlias
}

func (this *DmlBase) SetTableAlias(alias string) {
	this.alias(alias)
}

func (this *DmlBase) alias(a string) {
	if a != "" {
		this.joinBag = NewAliasBag(a + "_" + JOIN_PREFIX)
		this.tableAlias = a
		this.rawSQL = nil
	}
}

func (this *DmlBase) GetJoins() []*Join {
	return this.joins
}

func (this *DmlBase) SetParameter(key string, parameter interface{}) {
	this.parameters[key] = parameter
}

func (this *DmlBase) GetParameters() map[string]interface{} {
	return this.parameters
}

func (this *DmlBase) GetParameter(column *Column) interface{} {
	return this.parameters[column.GetAlias()]
}

func (this *DmlBase) GetCriteria() *Criteria {
	return this.criteria
}

// Sets the value of parameter to the column
// param col: The column
// param parameter: The value to set
func (this *DmlBase) SetParameterFor(col *Column, parameter interface{}) {
	this.SetParameter(col.GetAlias(), parameter)
}

func (this *DmlBase) GetAliasForAssociation(association *Association) string {
	if this.joinBag != nil {
		return this.joinBag.GetAlias(association)
	}
	return ""
}

// includes the associations as inner joins to the current path
// param associations
func (this *DmlBase) inner(inner bool, associations ...*Association) {
	for _, association := range associations {
		pe := new(PathElement)
		pe.Base = association
		pe.Inner = inner
		this.path = append(this.path, pe)
	}

	this.rawSQL = nil
}

/*
Indicates that the current association chain should be used to join only.
A table end alias can also be supplied.
This
*/
func (this *DmlBase) joinTo(path []*PathElement, fetch bool) {
	if len(path) > 0 {
		this.addJoin(path, nil, fetch)

		// the first position refers to constraints applied to the table, due to a association discriminator
		pathCriterias := this.buildPathCriterias(path)
		var firstCriterias []*Criteria
		// process the acumulated criterias
		for index, pathCriteria := range pathCriterias {
			if pathCriteria != nil {
				conds := pathCriteria.Criterias
				// adjustTableAlias()
				if conds != nil {
					// index == 0 applies to the starting table
					if index == 0 {
						// already with the alias applied
						firstCriterias = conds
					} else {
						if firstCriterias != nil {
							// add the criterias restriction refering to the table,
							// due to association discriminator
							tmp := make([]*Criteria, len(conds))
							copy(tmp, conds)
							conds = append(tmp, firstCriterias...)
							firstCriterias = nil
						}
						this.applyOn(path[:index], And(conds...))
					}
				}

				if pathCriteria.Columns != nil {
					this.applyInclude(path[:index], pathCriteria.Columns...)
				}
			}
		}
	}
}

/*
The path criteria on position 0 refers the criteria on the FROM table.
The Association can have a constraint(discriminator) refering a column in the source table.
Both ends of Discriminator criterias (association origin and destination tables) are treated in this block
*/
func (this *DmlBase) buildPathCriterias(paths []*PathElement) []*PathCriteria {
	// see if any targeted table has discriminator columns
	index := 0
	var tableCriterias []*Criteria
	length := len(paths) + 1
	pathCriterias := make([]*PathCriteria, length, length)

	// processes normal criterias
	for _, pe := range paths {
		index++

		var pc *PathCriteria
		if pe.Criteria != nil {
			pc = new(PathCriteria)
			pc.Criterias = append(pc.Criterias, pe.Criteria)
			pathCriterias[index] = pc
		}

		// table discriminator on target
		tableCriterias = pe.Base.GetTableTo().GetCriterias()
		if tableCriterias != nil {
			if pc == nil {
				pc = new(PathCriteria)
				pathCriterias[index] = pc
			}
			pc.Criterias = append(pc.Criterias, tableCriterias...)
		}

		// references column Includes
		if pe.Columns != nil {
			if pc == nil {
				pc = new(PathCriteria)
				pathCriterias[index] = pc
			}
			pc.Columns = pe.Columns
		}
	}

	// process criterias from the association discriminators
	var lastFkAlias = this.GetTableAlias()
	index = 0
	for _, pe := range paths {
		index++
		discriminators := pe.Base.GetDiscriminators()
		if discriminators != nil {
			pc := pathCriterias[index]
			if pc == nil {
				pc = new(PathCriteria)
				pathCriterias[index] = pc
			}

			if pe.Base.GetDiscriminatorTable().Equals(pe.Base.GetTableTo()) {
				for _, v := range discriminators {
					pc.Criterias = append(pc.Criterias, v.Criteria())
				}
			} else {
				// force table alias for the first criteria
				for _, v := range discriminators {
					crit := v.Criteria()
					crit.SetTableAlias(lastFkAlias)
					pc.Criterias = append(pc.Criterias, crit)
				}
			}
		}
		lastFkAlias = this.joinBag.GetAlias(pe.Derived)
	}

	return pathCriterias
}

func (this *DmlBase) addJoin(associations []*PathElement, common []*PathElement, fetch bool) []*PathElement {
	var local []*PathElement

	if common == nil {
		common = DeepestCommonPath(this.cachedAssociation, associations)
	}

	deriveds := make([]*Association, len(associations))
	var lastFk *Association
	matches := true
	f := 0
	for _, pe := range associations {
		association := pe.Base
		var lastCachedFk *Association
		if matches && f < len(common) {
			if common[f].Base.Equals(association) {
				lastCachedFk = common[f].Derived
			} else {
				matches = false
			}
		} else {
			matches = false
		}

		if lastCachedFk == nil {
			// copies to assign the alias to this query
			deriveds[f], _ = association.Clone().(*Association)

			/*
				Processes the associations.
				The alias of the initial side (from) of the first associations
				is assigned the value 'firstAlias' (main table value)
				The alias of the final side of the last association is assigned the
				value 'pe.Alias', if it is not empty
			*/
			var fkAlias string
			if f == 0 {
				fkAlias = this.tableAlias
			} else {
				fkAlias = this.joinBag.GetAlias(lastFk)
			}

			if deriveds[f].IsMany2Many() {
				fromFk := deriveds[f].FromM2M
				toFk := deriveds[f].ToM2M

				this.prepareAssociation(
					fkAlias,
					this.joinBag.GetAlias(fromFk),
					fromFk)

				if pe.PreferredAlias == "" {
					fkAlias = this.joinBag.GetAlias(toFk)
				} else {
					fkAlias = pe.PreferredAlias
					this.joinBag.SetAlias(toFk, pe.PreferredAlias)
				}
				this.prepareAssociation(
					this.joinBag.GetAlias(fromFk),
					fkAlias,
					toFk)
				lastFk = toFk
			} else {
				var fkAlias2 string
				if pe.PreferredAlias == "" {
					fkAlias2 = this.joinBag.GetAlias(deriveds[f])
				} else {
					fkAlias2 = pe.PreferredAlias
					this.joinBag.SetAlias(deriveds[f], pe.PreferredAlias)
				}
				this.prepareAssociation(
					fkAlias,
					fkAlias2,
					deriveds[f])
				lastFk = deriveds[f]
			}

		} else {
			// the main list allways with association many-to-many
			deriveds[f] = lastCachedFk
			// defines the previous fk
			if deriveds[f].IsMany2Many() {
				lastFk = deriveds[f].ToM2M
			} else {
				lastFk = lastCachedFk
			}
		}
		pe.Derived = deriveds[f]
		local = append(local, pe) // cache it

		f++
	}

	// only caches if the path was different
	if !matches {
		this.cachedAssociation = append(this.cachedAssociation, local)
	}

	// gets the alias of the last join
	this.lastFkAlias = this.joinBag.GetAlias(lastFk)

	this.lastJoin = NewJoin(local, fetch)
	this.joins = append(this.joins, this.lastJoin)

	return local
}

func (this *DmlBase) prepareAssociation(aliasFrom string, aliasTo string, currentFk *Association) {
	currentFk.SetAliasFrom(aliasFrom)
	currentFk.SetAliasTo(aliasTo)
	for _, rel := range currentFk.GetRelations() {
		rel.From.SetTableAlias(aliasFrom)
		rel.To.SetTableAlias(aliasTo)
	}
}

func (this *DmlBase) where(restrictions []*Criteria) {
	var criterias []*Criteria
	if len(restrictions) > 0 {
		criterias = append(criterias, restrictions...)
	}

	if len(this.discriminatorCriterias) > 0 {
		criterias = append(criterias, this.discriminatorCriterias...)
	}

	if len(criterias) > 0 {
		this.applyWhere(And(criterias...))
	}
}

func (this *DmlBase) applyOn(chain []*PathElement, criteria *Criteria) {
	if len(chain) > 0 {
		pe := chain[len(chain)-1]
		cpy, _ := criteria.Clone().(*Criteria)

		fk := pe.Derived
		var fkAlias string
		if fk.IsMany2Many() {
			fkAlias = this.joinBag.GetAlias(fk.ToM2M)
		} else {
			fkAlias = this.joinBag.GetAlias(pe.Derived)
		}
		cpy.SetTableAlias(fkAlias)

		this.replaceRaw(cpy)
		pe.Criteria = cpy

		this.rawSQL = nil
	}
}

func (this *DmlBase) applyInclude(chain []*PathElement, tokens ...Tokener) {
	if len(chain) > 0 {
		pe := chain[len(chain)-1]
		fk := pe.Derived
		var fkAlias string
		if fk.IsMany2Many() {
			fkAlias = this.joinBag.GetAlias(fk.ToM2M)
		} else {
			fkAlias = this.joinBag.GetAlias(pe.Derived)
		}
		for _, token := range tokens {
			token.SetTableAlias(fkAlias)
		}

		this.rawSQL = nil
	}
}

// WHERE ===
func (this *DmlBase) applyWhere(restriction *Criteria) {
	token, _ := restriction.Clone().(*Criteria)
	this.replaceRaw(token)
	token.SetTableAlias(this.tableAlias)

	this.criteria = token

	this.rawSQL = nil
}

func (this *DmlBase) dumpParameters(params map[string]interface{}) string {
	str := tk.NewStrBuffer()
	for name, v := range params {
		if strings.HasSuffix(name, "$") {
			// secret
			str.Add(fmt.Sprintf("[%s=****]", name))
		} else if v != nil {
			typ := reflect.ValueOf(v)
			k := typ.Kind()
			if k == reflect.Slice || k == reflect.Array {
				str.Add(fmt.Sprintf("[%s=<BLOB>]", name))
			} else if k == reflect.Ptr {
				if typ.IsNil() {
					str.Add(fmt.Sprintf("[%s=NULL]", name))
				} else {
					str.Add(fmt.Sprintf("[%s=(*)%v]", name, typ.Elem().Interface()))
				}
			} else {
				str.Add(fmt.Sprintf("[%s=%v]", name, typ.Interface()))
			}
		} else {
			str.Add(fmt.Sprintf("[%s=NULL]", name))
		}
	}

	return str.String()
}

func (this *DmlBase) debugTime(when time.Time, depth int) {
	elapsed := time.Since(when)
	if lgr.IsActive(log.DEBUG) {
		lgr.CallerAt(depth + 1).Debug(func() string {
			return fmt.Sprintf("executed in: %f secs", elapsed.Seconds())
		})
	}
}

func (this *DmlBase) debugSQL(sql string, depth int) {
	if lgr.IsActive(log.DEBUG) {
		dump := this.dumpParameters(this.parameters)
		lgr.CallerAt(depth + 1).Debug(func() string {
			return fmt.Sprintf("\n\t%T SQL: %s\n\tparameters: %s",
				this, sql, dump)
		})
	}
}

// replaces RAW with PARAM
//
// param baseDml: the instance DmlBase were to put the created parameters
// param token
// return
func (this *DmlBase) replaceRaw(token Tokener) {
	/*
		if tk.IsNil(token) {
			return
		}
	*/

	members := token.GetMembers()
	if token.GetOperator() == TOKEN_RAW {
		this.rawIndex++
		parameter := this.tableAlias + "_R" + strconv.Itoa(this.rawIndex)
		this.SetParameter(parameter, token.GetValue())
		token.SetOperator(TOKEN_PARAM)
		token.SetValue(parameter)
		return
	} else if token.GetOperator() == TOKEN_SUBQUERY {
		subquery := token.GetValue().(*Query)
		// copy the parameters of the subquery to the main query
		for k, v := range subquery.GetParameters() {
			this.SetParameter(k, v)
		}
		return
	} else {
		if members != nil {
			for _, t := range members {
				if t != nil {
					this.replaceRaw(t)
				}
			}
		}
	}
}
