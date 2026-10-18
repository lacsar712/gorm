package callbacks

import (
	"reflect"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
	"gorm.io/gorm/utils"
)

func SaveBeforeAssociations(create bool) func(db *gorm.DB) {
	return func(db *gorm.DB) {
		if db.Error == nil && db.Statement.Schema != nil {
			selectColumns, restricted := db.Statement.SelectAndOmitColumns(create, !create)

			// Save Belongs To associations
			for _, rel := range db.Statement.Schema.Relationships.BelongsTo {
				if v, ok := selectColumns[rel.Name]; (ok && !v) || (!ok && restricted) {
					continue
				}

				setupReferences := func(obj reflect.Value, elem reflect.Value) {
					for _, ref := range rel.References {
						if !ref.OwnPrimaryKey {
							pv, _ := ref.PrimaryKey.ValueOf(db.Statement.Context, elem)
							db.AddError(ref.ForeignKey.Set(db.Statement.Context, obj, pv))

							if dest, ok := db.Statement.Dest.(map[string]interface{}); ok {
								dest[ref.ForeignKey.DBName] = pv
								if _, ok := dest[rel.Name]; ok {
									dest[rel.Name] = elem.Interface()
								}
							}
						}
					}
				}

				switch db.Statement.ReflectValue.Kind() {
				case reflect.Slice, reflect.Array:
					var (
						rValLen   = db.Statement.ReflectValue.Len()
						objs      = make([]reflect.Value, 0, rValLen)
						fieldType = rel.Field.FieldType
						isPtr     = fieldType.Kind() == reflect.Ptr
					)

					if !isPtr {
						fieldType = reflect.PointerTo(fieldType)
					}

					elems := reflect.MakeSlice(reflect.SliceOf(fieldType), 0, 10)
					distinctElems := reflect.MakeSlice(reflect.SliceOf(fieldType), 0, 10)
					identityMap := map[string]bool{}
					cyclicElems := map[reflect.Value]int{}
					for i := 0; i < rValLen; i++ {
						obj := db.Statement.ReflectValue.Index(i)
						if reflect.Indirect(obj).Kind() != reflect.Struct {
							break
						}
						if _, zero := rel.Field.ValueOf(db.Statement.Context, obj); !zero { // check belongs to relation value
							rv := rel.Field.ReflectValueOf(db.Statement.Context, obj) // relation reflect value
							if !isPtr {
								rv = rv.Addr()
							}
							objs = append(objs, obj)
							elems = reflect.Append(elems, rv)

							if rel.Polymorphic == nil {
								if key, ok := circularKey(rv); ok {
									if state := circularBelongsToState(db, rel.FieldSchema, rv); state != circularStateNormal {
										// circular reference: the related object is owned by a
										// create callback still on the stack, it must not be
										// saved again here
										cyclicElems[key] = state
										continue
									}
								}
							}

							relPrimaryValues := make([]interface{}, 0, len(rel.FieldSchema.PrimaryFields))
							for _, pf := range rel.FieldSchema.PrimaryFields {
								if pfv, ok := pf.ValueOf(db.Statement.Context, rv); !ok {
									relPrimaryValues = append(relPrimaryValues, pfv)
								}
							}
							cacheKey := utils.ToStringKey(relPrimaryValues...)
							if len(relPrimaryValues) != len(rel.FieldSchema.PrimaryFields) || !identityMap[cacheKey] {
								if cacheKey != "" { // has primary fields
									identityMap[cacheKey] = true
								}

								distinctElems = reflect.Append(distinctElems, rv)
							}
						}
					}

					if elems.Len() > 0 {
						var saveErr error
						if distinctElems.Len() > 0 {
							saveErr = saveAssociations(db, rel, distinctElems, selectColumns, restricted, nil)
						}
						if saveErr == nil {
							for i := 0; i < elems.Len(); i++ {
								elem := elems.Index(i)
								if rel.Polymorphic == nil {
									if key, ok := circularKey(elem); ok {
										switch cyclicElems[key] {
										case circularStateDefer:
											circularDeferReference(db, db.Statement.Schema, rel.FieldSchema, objs[i], elem,
												circularReferencePairs(rel, false))
											continue
										case circularStateUseExisting:
											setupReferences(objs[i], elem)
											continue
										}
									}
								}
								setupReferences(objs[i], elem)
							}
						}
					}
				case reflect.Struct:
					if _, zero := rel.Field.ValueOf(db.Statement.Context, db.Statement.ReflectValue); !zero {
						rv := rel.Field.ReflectValueOf(db.Statement.Context, db.Statement.ReflectValue) // relation reflect value
						if rv.Kind() != reflect.Ptr {
							rv = rv.Addr()
						}

						state := circularStateNormal
						if rel.Polymorphic == nil {
							state = circularBelongsToState(db, rel.FieldSchema, rv)
						}

						switch state {
						case circularStateDefer:
							circularDeferReference(db, db.Statement.Schema, rel.FieldSchema, db.Statement.ReflectValue, rv,
								circularReferencePairs(rel, false))
						case circularStateUseExisting:
							setupReferences(db.Statement.ReflectValue, rv)
						default:
							if saveAssociations(db, rel, rv, selectColumns, restricted, nil) == nil {
								setupReferences(db.Statement.ReflectValue, rv)
							}
						}
					}
				}
			}
		}
	}
}

func SaveAfterAssociations(create bool) func(db *gorm.DB) {
	return func(db *gorm.DB) {
		if db.Error == nil && db.Statement.Schema != nil {
			selectColumns, restricted := db.Statement.SelectAndOmitColumns(create, !create)

			// Save Has One associations
			for _, rel := range db.Statement.Schema.Relationships.HasOne {
				if v, ok := selectColumns[rel.Name]; (ok && !v) || (!ok && restricted) {
					continue
				}

				switch db.Statement.ReflectValue.Kind() {
				case reflect.Slice, reflect.Array:
					var (
						fieldType = rel.Field.FieldType
						isPtr     = fieldType.Kind() == reflect.Ptr
					)

					if !isPtr {
						fieldType = reflect.PointerTo(fieldType)
					}

					elems := reflect.MakeSlice(reflect.SliceOf(fieldType), 0, 10)

					for i := 0; i < db.Statement.ReflectValue.Len(); i++ {
						obj := db.Statement.ReflectValue.Index(i)

						if reflect.Indirect(obj).Kind() == reflect.Struct {
							if _, zero := rel.Field.ValueOf(db.Statement.Context, obj); !zero {
								rv := rel.Field.ReflectValueOf(db.Statement.Context, obj)
								if rv.Kind() != reflect.Ptr {
									rv = rv.Addr()
								}

								if rel.Polymorphic == nil && circularHasChildState(db, rel.FieldSchema, rv) == circularStateDefer {
									// circular reference: the related row is inserted later,
									// its foreign key is backfilled in the same transaction
									circularDeferReference(db, rel.FieldSchema, db.Statement.Schema, rv, obj,
										circularReferencePairs(rel, true))
									continue
								}

								for _, ref := range rel.References {
									if ref.OwnPrimaryKey {
										fv, _ := ref.PrimaryKey.ValueOf(db.Statement.Context, obj)
										db.AddError(ref.ForeignKey.Set(db.Statement.Context, rv, fv))
									} else if ref.PrimaryValue != "" {
										db.AddError(ref.ForeignKey.Set(db.Statement.Context, rv, ref.PrimaryValue))
									}
								}

								elems = reflect.Append(elems, rv)
							}
						}
					}

					if elems.Len() > 0 {
						assignmentColumns := make([]string, 0, len(rel.References))
						for _, ref := range rel.References {
							assignmentColumns = append(assignmentColumns, ref.ForeignKey.DBName)
						}

						saveAssociations(db, rel, elems, selectColumns, restricted, assignmentColumns)
					}
				case reflect.Struct:
					if _, zero := rel.Field.ValueOf(db.Statement.Context, db.Statement.ReflectValue); !zero {
						f := rel.Field.ReflectValueOf(db.Statement.Context, db.Statement.ReflectValue)
						if f.Kind() != reflect.Ptr {
							f = f.Addr()
						}

						if rel.Polymorphic == nil && circularHasChildState(db, rel.FieldSchema, f) == circularStateDefer {
							// circular reference: the related row is inserted later,
							// its foreign key is backfilled in the same transaction
							circularDeferReference(db, rel.FieldSchema, db.Statement.Schema, f, db.Statement.ReflectValue,
								circularReferencePairs(rel, true))
						} else {
							assignmentColumns := make([]string, 0, len(rel.References))
							for _, ref := range rel.References {
								if ref.OwnPrimaryKey {
									fv, _ := ref.PrimaryKey.ValueOf(db.Statement.Context, db.Statement.ReflectValue)
									db.AddError(ref.ForeignKey.Set(db.Statement.Context, f, fv))
								} else if ref.PrimaryValue != "" {
									db.AddError(ref.ForeignKey.Set(db.Statement.Context, f, ref.PrimaryValue))
								}
								assignmentColumns = append(assignmentColumns, ref.ForeignKey.DBName)
							}

							saveAssociations(db, rel, f, selectColumns, restricted, assignmentColumns)
						}
					}
				}
			}

			// Save Has Many associations
			for _, rel := range db.Statement.Schema.Relationships.HasMany {
				if v, ok := selectColumns[rel.Name]; (ok && !v) || (!ok && restricted) {
					continue
				}

				fieldType := rel.Field.IndirectFieldType.Elem()
				isPtr := fieldType.Kind() == reflect.Ptr
				if !isPtr {
					fieldType = reflect.PointerTo(fieldType)
				}
				elems := reflect.MakeSlice(reflect.SliceOf(fieldType), 0, 10)
				identityMap := map[string]bool{}
				appendToElems := func(v reflect.Value) {
					if _, zero := rel.Field.ValueOf(db.Statement.Context, v); !zero {
						f := reflect.Indirect(rel.Field.ReflectValueOf(db.Statement.Context, v))

						for i := 0; i < f.Len(); i++ {
							elem := f.Index(i)

							if rel.Polymorphic == nil && circularHasChildState(db, rel.FieldSchema, elem) == circularStateDefer {
								// circular reference: the child row is inserted later,
								// its foreign key is backfilled in the same transaction
								circularDeferReference(db, rel.FieldSchema, db.Statement.Schema, elem, v,
									circularReferencePairs(rel, true))
								continue
							}

							for _, ref := range rel.References {
								if ref.OwnPrimaryKey {
									pv, _ := ref.PrimaryKey.ValueOf(db.Statement.Context, v)
									db.AddError(ref.ForeignKey.Set(db.Statement.Context, elem, pv))
								} else if ref.PrimaryValue != "" {
									db.AddError(ref.ForeignKey.Set(db.Statement.Context, elem, ref.PrimaryValue))
								}
							}

							relPrimaryValues := make([]interface{}, 0, len(rel.FieldSchema.PrimaryFields))
							for _, pf := range rel.FieldSchema.PrimaryFields {
								if pfv, ok := pf.ValueOf(db.Statement.Context, elem); !ok {
									relPrimaryValues = append(relPrimaryValues, pfv)
								}
							}

							cacheKey := utils.ToStringKey(relPrimaryValues...)
							if len(relPrimaryValues) != len(rel.FieldSchema.PrimaryFields) || !identityMap[cacheKey] {
								if cacheKey != "" { // has primary fields
									identityMap[cacheKey] = true
								}

								if isPtr {
									elems = reflect.Append(elems, elem)
								} else {
									elems = reflect.Append(elems, elem.Addr())
								}
							}
						}
					}
				}

				switch db.Statement.ReflectValue.Kind() {
				case reflect.Slice, reflect.Array:
					for i := 0; i < db.Statement.ReflectValue.Len(); i++ {
						obj := db.Statement.ReflectValue.Index(i)
						if reflect.Indirect(obj).Kind() == reflect.Struct {
							appendToElems(obj)
						}
					}
				case reflect.Struct:
					appendToElems(db.Statement.ReflectValue)
				}

				if elems.Len() > 0 {
					assignmentColumns := make([]string, 0, len(rel.References))
					for _, ref := range rel.References {
						assignmentColumns = append(assignmentColumns, ref.ForeignKey.DBName)
					}

					saveAssociations(db, rel, elems, selectColumns, restricted, assignmentColumns)
				}
			}

			// Save Many2Many associations
			for _, rel := range db.Statement.Schema.Relationships.Many2Many {
				if v, ok := selectColumns[rel.Name]; (ok && !v) || (!ok && restricted) {
					continue
				}

				fieldType := rel.Field.IndirectFieldType.Elem()
				isPtr := fieldType.Kind() == reflect.Ptr
				if !isPtr {
					fieldType = reflect.PointerTo(fieldType)
				}
				elems := reflect.MakeSlice(reflect.SliceOf(fieldType), 0, 10)
				distinctElems := reflect.MakeSlice(reflect.SliceOf(fieldType), 0, 10)
				joins := reflect.MakeSlice(reflect.SliceOf(reflect.PointerTo(rel.JoinTable.ModelType)), 0, 10)
				objs := []reflect.Value{}

				appendToJoins := func(obj reflect.Value, elem reflect.Value) {
					joinValue := reflect.New(rel.JoinTable.ModelType)
					for _, ref := range rel.References {
						if ref.OwnPrimaryKey {
							fv, _ := ref.PrimaryKey.ValueOf(db.Statement.Context, obj)
							db.AddError(ref.ForeignKey.Set(db.Statement.Context, joinValue, fv))
						} else if ref.PrimaryValue != "" {
							db.AddError(ref.ForeignKey.Set(db.Statement.Context, joinValue, ref.PrimaryValue))
						} else {
							fv, _ := ref.PrimaryKey.ValueOf(db.Statement.Context, elem)
							db.AddError(ref.ForeignKey.Set(db.Statement.Context, joinValue, fv))
						}
					}
					joins = reflect.Append(joins, joinValue)
				}

				identityMap := map[string]bool{}
				appendToElems := func(v reflect.Value) {
					if _, zero := rel.Field.ValueOf(db.Statement.Context, v); !zero {
						f := reflect.Indirect(rel.Field.ReflectValueOf(db.Statement.Context, v))
						for i := 0; i < f.Len(); i++ {
							elem := f.Index(i)
							if !isPtr {
								elem = elem.Addr()
							}
							objs = append(objs, v)
							elems = reflect.Append(elems, elem)

							relPrimaryValues := make([]interface{}, 0, len(rel.FieldSchema.PrimaryFields))
							for _, pf := range rel.FieldSchema.PrimaryFields {
								if pfv, ok := pf.ValueOf(db.Statement.Context, elem); !ok {
									relPrimaryValues = append(relPrimaryValues, pfv)
								}
							}

							cacheKey := utils.ToStringKey(relPrimaryValues...)
							if len(relPrimaryValues) != len(rel.FieldSchema.PrimaryFields) || !identityMap[cacheKey] {
								if cacheKey != "" { // has primary fields
									identityMap[cacheKey] = true
								}

								distinctElems = reflect.Append(distinctElems, elem)
							}

						}
					}
				}

				switch db.Statement.ReflectValue.Kind() {
				case reflect.Slice, reflect.Array:
					for i := 0; i < db.Statement.ReflectValue.Len(); i++ {
						obj := db.Statement.ReflectValue.Index(i)
						if reflect.Indirect(obj).Kind() == reflect.Struct {
							appendToElems(obj)
						}
					}
				case reflect.Struct:
					appendToElems(db.Statement.ReflectValue)
				}

				// optimize elems of reflect value length
				if elemLen := elems.Len(); elemLen > 0 {
					if v, ok := selectColumns[rel.Name+".*"]; !ok || v {
						saveAssociations(db, rel, distinctElems, selectColumns, restricted, nil)
					}

					for i := 0; i < elemLen; i++ {
						appendToJoins(objs[i], elems.Index(i))
					}
				}

				if joins.Len() > 0 {
					db.AddError(db.Session(&gorm.Session{NewDB: true}).Clauses(clause.OnConflict{DoNothing: true}).Session(&gorm.Session{
						SkipHooks:                db.Statement.SkipHooks,
						DisableNestedTransaction: true,
					}).Create(joins.Interface()).Error)
				}
			}
		}
	}
}

func onConflictOption(stmt *gorm.Statement, s *schema.Schema, defaultUpdatingColumns []string) (onConflict clause.OnConflict) {
	if len(defaultUpdatingColumns) > 0 || stmt.DB.FullSaveAssociations {
		onConflict.Columns = make([]clause.Column, 0, len(s.PrimaryFieldDBNames))
		for _, dbName := range s.PrimaryFieldDBNames {
			onConflict.Columns = append(onConflict.Columns, clause.Column{Name: dbName})
		}

		onConflict.UpdateAll = stmt.DB.FullSaveAssociations
		if !onConflict.UpdateAll {
			onConflict.DoUpdates = clause.AssignmentColumns(defaultUpdatingColumns)
		}
	} else {
		onConflict.DoNothing = true
	}

	return
}

func saveAssociations(db *gorm.DB, rel *schema.Relationship, rValues reflect.Value, selectColumns map[string]bool, restricted bool, defaultUpdatingColumns []string) error {
	// stop save association loop
	if checkAssociationsSaved(db, rValues) {
		return nil
	}

	var (
		selects, omits []string
		onConflict     = onConflictOption(db.Statement, rel.FieldSchema, defaultUpdatingColumns)
		refName        = rel.Name + "."
		values         = rValues.Interface()
	)

	for name, ok := range selectColumns {
		columnName := ""
		if strings.HasPrefix(name, refName) {
			columnName = strings.TrimPrefix(name, refName)
		}

		if columnName != "" {
			if ok {
				selects = append(selects, columnName)
			} else {
				omits = append(omits, columnName)
			}
		}
	}

	tx := db.Session(&gorm.Session{NewDB: true}).Clauses(onConflict).Session(&gorm.Session{
		FullSaveAssociations:     db.FullSaveAssociations,
		SkipHooks:                db.Statement.SkipHooks,
		DisableNestedTransaction: true,
	})

	db.Statement.Settings.Range(func(k, v interface{}) bool {
		tx.Statement.Settings.Store(k, v)
		return true
	})

	// internal association saves carry ON CONFLICT but still take part in
	// circular reference handling, unlike caller provided upserts
	tx = tx.Set(associationSaveStoreKey, true)

	if tx.Statement.FullSaveAssociations {
		tx = tx.Set("gorm:update_track_time", true)
	}

	if len(selects) > 0 {
		tx = tx.Select(selects)
	} else if restricted && len(omits) == 0 {
		tx = tx.Omit(clause.Associations)
	}

	if len(omits) > 0 {
		tx = tx.Omit(omits...)
	}

	return db.AddError(tx.Create(values).Error)
}

// check association values has been saved
// if values kind is Struct, check it has been saved
// if values kind is Slice/Array, check all items have been saved
var visitMapStoreKey = "gorm:saved_association_map"

func checkAssociationsSaved(db *gorm.DB, values reflect.Value) bool {
	if visit, ok := db.Get(visitMapStoreKey); ok {
		if v, ok := visit.(*visitMap); ok {
			if loadOrStoreVisitMap(v, values) {
				return true
			}
		}
	} else {
		vistMap := make(visitMap)
		loadOrStoreVisitMap(&vistMap, values)
		db.Set(visitMapStoreKey, &vistMap)
	}

	return false
}
