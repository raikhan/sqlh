package database

import (
	"context"
	"sort"
	"strings"
)

type DBCacheGenerator struct {
	repo DBRepository
}

func NewDBCacheUpdater(repo DBRepository) *DBCacheGenerator {
	return &DBCacheGenerator{
		repo: repo,
	}
}

func (u *DBCacheGenerator) GenerateDBCachePrimary(ctx context.Context) (*DBCache, error) {
	var err error
	dbCache := &DBCache{}
	dbCache.defaultSchema, err = u.repo.CurrentSchema(ctx)
	if err != nil {
		return nil, err
	}
	schemas, err := u.genSchemaCache(ctx)
	if err != nil {
		return nil, err
	}
	dbCache.Schemas = make(map[string]string)
	for index, element := range schemas {
		dbCache.Schemas[strings.ToUpper(index)] = element
	}

	if dbCache.defaultSchema == "" {
		var topKey string
		for k := range dbCache.Schemas {
			topKey = k
			continue
		}
		dbCache.defaultSchema = dbCache.Schemas[topKey]
	}
	schemaTables, err := u.repo.SchemaTables(ctx)
	if err != nil {
		return nil, err
	}
	dbCache.SchemaTables = make(map[string][]string)
	for index, element := range schemaTables {
		dbCache.SchemaTables[strings.ToUpper(index)] = element
	}

	dbCache.ColumnsWithParent, err = u.genColumnCacheCurrent(ctx, dbCache.defaultSchema)
	if err != nil {
		return nil, err
	}
	dbCache.foreignKeyList, err = u.repo.DescribeForeignKeysBySchema(ctx, dbCache.defaultSchema)
	if err != nil {
		return nil, err
	}
	dbCache.ForeignKeys = genForeignKeysMap(dbCache.foreignKeyList)
	// views come with the background load
	dbCache.Relations = dbCache.BuildRelations(nil)
	// built-ins now; the database's functions come with the background load
	dbCache.Functions = NewFunctionCatalog(u.repo.Driver(), nil)
	return dbCache, nil
}

func (u *DBCacheGenerator) GenerateDBCacheSecondary(ctx context.Context) (map[string][]*ColumnDesc, error) {
	return u.genColumnCacheAll(ctx)
}

func (u *DBCacheGenerator) genSchemaCache(ctx context.Context) (map[string]string, error) {
	dbs, err := u.repo.Schemas(ctx)
	if err != nil {
		return nil, err
	}
	databaseMap := map[string]string{}
	for _, db := range dbs {
		databaseMap[strings.ToUpper(db)] = db
	}
	return databaseMap, nil
}

func (u *DBCacheGenerator) genColumnCacheCurrent(ctx context.Context, schemaName string) (map[string][]*ColumnDesc, error) {
	columnDescs, err := u.repo.DescribeDatabaseTableBySchema(ctx, schemaName)
	if err != nil {
		return nil, err
	}
	return genColumnMap(columnDescs), nil
}

func (u *DBCacheGenerator) genColumnCacheAll(ctx context.Context) (map[string][]*ColumnDesc, error) {
	columnDescs, err := u.repo.DescribeDatabaseTable(ctx)
	if err != nil {
		return nil, err
	}
	return genColumnMap(columnDescs), nil
}

// GenerateViews lists the views of a schema with their definitions; empty
// when the database can't.
func (u *DBCacheGenerator) GenerateViews(ctx context.Context, schemaName string) ([]*ViewDef, error) {
	vd, ok := u.repo.(ViewDescriber)
	if !ok {
		return []*ViewDef{}, nil
	}
	views, err := vd.DescribeViews(ctx, schemaName)
	if views == nil {
		views = []*ViewDef{}
	}
	return views, err
}

func genForeignKeysMap(fk []*ForeignKey) map[string]map[string][]*ForeignKey {
	retVal := make(map[string]map[string][]*ForeignKey)
	for _, cur := range fk {
		elem := (*cur)[0]
		refs, ok := retVal[elem[0].Table]
		if !ok {
			refs = make(map[string][]*ForeignKey)
		}
		refs[elem[1].Table] = append(refs[elem[1].Table], cur)
		retVal[elem[0].Table] = refs

		refs, ok = retVal[elem[1].Table]
		if !ok {
			refs = make(map[string][]*ForeignKey)
		}
		refs[elem[0].Table] = append(refs[elem[0].Table], cur)
		retVal[elem[1].Table] = refs
	}
	return retVal
}

func genColumnMap(columnDescs []*ColumnDesc) map[string][]*ColumnDesc {
	columnMap := map[string][]*ColumnDesc{}
	for _, desc := range columnDescs {
		key := columnDatabaseKey(desc.Schema, desc.Table)
		columnMap[key] = append(columnMap[key], desc)
	}
	return columnMap
}

type DBCache struct {
	defaultSchema     string
	Schemas           map[string]string
	SchemaTables      map[string][]string
	ColumnsWithParent map[string][]*ColumnDesc
	ForeignKeys       map[string]map[string][]*ForeignKey
	foreignKeyList    []*ForeignKey
	// Relations: the default schema's relationship graph (foreign keys, views,
	// guesses). Views are added by the background load.
	Relations *RelGraph
	// Synonyms: owner → names (Oracle; loaded in the background, nil until then
	// or when the database has none).
	Synonyms map[string][]string
	// SecondaryLoaded: the background load (all columns, synonyms) is done.
	SecondaryLoaded bool
	// Functions: callable functions (built-ins, then also the database's
	// once the background load is done). Nil for caches built by hand.
	Functions *FunctionCatalog
}

func (dc *DBCache) Database(dbName string) (db string, ok bool) {
	db, ok = dc.Schemas[strings.ToUpper(dbName)]
	return
}

func (dc *DBCache) SortedSchemas() []string {
	dbs := []string{}
	for _, db := range dc.Schemas {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	return dbs
}

func (dc *DBCache) SortedTablesByDBName(dbName string) (tbls []string, ok bool) {
	tbls, ok = dc.SchemaTables[strings.ToUpper(dbName)]
	sort.Strings(tbls)
	return
}

func (dc *DBCache) SortedTables() []string {
	tbls, _ := dc.SortedTablesByDBName(dc.defaultSchema)
	return tbls
}

// DefaultSchema is the connection's current schema.
func (dc *DBCache) DefaultSchema() string { return dc.defaultSchema }

// SetDefaultSchema sets the current schema (for caches built by hand, e.g. tests).
func (dc *DBCache) SetDefaultSchema(s string) { dc.defaultSchema = s }

// CanonicalTableName returns the cached spelling of a default-schema table
// name, matched case-insensitively (Oracle stores unquoted names upper-case,
// users usually type lower-case).
func (dc *DBCache) CanonicalTableName(name string) (string, bool) {
	for _, t := range dc.SchemaTables[strings.ToUpper(dc.defaultSchema)] {
		if strings.EqualFold(t, name) {
			return t, true
		}
	}
	return "", false
}

func (dc *DBCache) ColumnDescs(tableName string) (cols []*ColumnDesc, ok bool) {
	cols, ok = dc.ColumnsWithParent[columnDatabaseKey(dc.defaultSchema, tableName)]
	return
}

func (dc *DBCache) ColumnDatabase(dbName, tableName string) (cols []*ColumnDesc, ok bool) {
	cols, ok = dc.ColumnsWithParent[columnDatabaseKey(dbName, tableName)]
	return
}

func (dc *DBCache) Column(tableName, colName string) (*ColumnDesc, bool) {
	cols, ok := dc.ColumnsWithParent[columnDatabaseKey(dc.defaultSchema, tableName)]
	if !ok {
		return nil, false
	}
	for _, col := range cols {
		if strings.EqualFold(col.Name, colName) {
			return col, true
		}
	}
	return nil, false
}

func columnDatabaseKey(dbName, tableName string) string {
	return strings.ToUpper(dbName) + "\t" + strings.ToUpper(tableName)
}

// BuildRelations builds the default schema's relationship graph from the
// cache; views nil: not loaded yet.
func (dc *DBCache) BuildRelations(views []*ViewDef) *RelGraph {
	schema := dc.defaultSchema
	return BuildRelGraph(RelGraphInput{
		Schema: schema,
		Tables: dc.SchemaTables[strings.ToUpper(schema)],
		Columns: func(t string) []*ColumnDesc {
			cols, _ := dc.ColumnDatabase(schema, t)
			return cols
		},
		ForeignKeys: dc.foreignKeyList,
		Views:       views,
	})
}

// JoinKeys maps table → related table → join conditions for join completion:
// the relationship graph's (maybe: with guesses), else the foreign keys.
func (dc *DBCache) JoinKeys(maybe bool) map[string]map[string][]*ForeignKey {
	if dc.Relations != nil {
		return dc.Relations.JoinKeys(maybe)
	}
	return dc.ForeignKeys
}
